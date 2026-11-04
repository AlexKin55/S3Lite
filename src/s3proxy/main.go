package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// s3proxy sits in front of the SeaweedFS S3 gateway. It forwards every request
// unchanged, but:
//   - when a download (GET) fails with 5xx it asks the S3Lite control plane
//     which PCs are offline (nodes_to_start for AT_RISK volumes) and returns a
//     machine-readable S3 error that tells the caller exactly which nodes to
//     power on;
//   - writes (PUT/POST/DELETE/PATCH) are refused with 503 while the control
//     plane reports the cluster read-only (fewer active volume servers than
//     min_write_nodes, e.g. the 6+4 EC scheme needs 10);
//   - writes with a declared size that cannot fit the cluster's free space
//     are refused upfront with 507 and a clear "won't fit" message (also
//     logged), instead of dying mid-upload with a generic 503 "temporarily
//     unavailable". The space an in-flight write consumes is reserved for the
//     request's duration (released by defer on any exit path — completion,
//     upstream error or client cancel), so two simultaneous uploads cannot
//     both pass the check against the same cached numbers and then collide.
//
// If the control plane is unreachable the proxy fails OPEN: the last known
// verdict applies within its TTL, then writes pass through again — losing the
// control plane must not take the S3 endpoint down.
//
// Usage:
//   bin/s3proxy -listen=:8333 -upstream=http://127.0.0.1:8332 -control=http://192.168.1.9:8099

const clusterStateTTL = 5 * time.Second

func main() {
	listen := flag.String("listen", ":8333", "external S3 listener")
	upstream := flag.String("upstream", "http://127.0.0.1:8332", "SeaweedFS S3 gateway address")
	control := flag.String("control", "http://192.168.1.9:8099", "S3Lite control plane address")
	// writeFactor: how many bytes of raw node capacity one byte of user data
	// ultimately consumes — replication copies × (data+parity)/data shards.
	// 5 for the s3lite default (3× replication via DEFAULT_REPLICATION=020,
	// 6+4 EC): a 4 GiB object needs 20 GiB of raw space. Must be updated
	// together with the replication config or the EC scheme.
	writeFactor := flag.Int64("writeFactor", 5, "raw bytes of cluster space consumed per byte written (replication × EC overhead)")
	flag.Parse()

	proxy := &proxy{
		upstream:    *upstream,
		controlURL:  *control,
		client:      &http.Client{Timeout: 60 * time.Second},
		stateTTL:    clusterStateTTL,
		writeFactor: *writeFactor,
	}

	log.Printf("s3proxy on %s -> %s (control %s, writeFactor %d)", *listen, *upstream, *control, *writeFactor)
	if err := http.ListenAndServe(*listen, proxy); err != nil {
		log.Fatal(err)
	}
}

type proxy struct {
	upstream    string
	controlURL  string
	client      *http.Client
	stateTTL    time.Duration
	writeFactor int64

	mu          sync.Mutex
	cachedState *clusterState
	cachedAt    time.Time
	reserved    int64 // usable bytes booked by in-flight writes (see reserve)
}

// clusterState is the subset of the control plane's /api/v1/cluster/status
// that drives the write guards.
type clusterState struct {
	Writable      bool `json:"writable"`
	ActiveNodes   int  `json:"active_nodes"`
	MinWriteNodes int  `json:"min_write_nodes"`
	// FreeBytes is the raw free node capacity reported by the control plane
	// (sum of the nodes' STORAGE_SIZE quotas minus their actual usage).
	// A pointer on purpose: nil means the field was absent (an older control
	// plane) and the size check fails open; an explicit 0 or negative value
	// means the control plane reports the cluster as full and the check
	// refuses writes.
	FreeBytes *int64 `json:"free_bytes"`
}

// capacityReported reports whether the control plane answered with a raw
// capacity number at all.
func (st *clusterState) capacityReported() bool {
	return st.FreeBytes != nil
}

// rawFree returns the reported raw free space (0 when absent or negative).
func (st *clusterState) rawFree() int64 {
	if st.FreeBytes == nil || *st.FreeBytes < 0 {
		return 0
	}
	return *st.FreeBytes
}

// usableFree returns the space a new object may claim: raw free space
// divided by the write amplification factor.
func (st *clusterState) usableFree(factor int64) int64 {
	if factor <= 0 {
		factor = 1
	}
	free := st.rawFree()
	if free <= 0 {
		return 0
	}
	return free / factor
}

// fits reports whether a write of n bytes fits the usable space, counting
// the bytes already reserved by in-flight writes. Without a capacity report
// it always returns true: the check must never block writes when the control
// plane does not report numbers.
func (st *clusterState) fits(n, reserved, factor int64) bool {
	if n <= 0 || !st.capacityReported() {
		return true
	}
	limit := st.usableFree(factor)
	return n+reserved <= limit
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Refuse writes while the cluster is read-only, and writes whose declared
	// size cannot fit the free space. Reads always pass: a degraded cluster
	// can still serve whatever shards remain available.
	if isWriteMethod(r.Method) {
		if st := p.clusterState(); st != nil {
			if !st.Writable {
				writeReadOnlyError(w, st)
				return
			}
			if size := declaredWriteSize(r); size > 0 && st.capacityReported() {
				p.mu.Lock()
				reserved := p.reserved
				p.mu.Unlock()
				if !st.fits(size, reserved, p.writeFactor) {
					log.Printf("s3proxy: refusing %s %s: %d bytes do not fit (%d usable, %d raw free, %d reserved by in-flight writes)",
						r.Method, r.URL.Path, size, st.usableFree(p.writeFactor), st.rawFree(), reserved)
					writeCapacityError(w, st, size, p.writeFactor)
					return
				}
				p.addReserved(size)
				defer p.release(size)
			}
		}
	}

	target := p.upstream + r.URL.RequestURI()
	body := r.Body
	if r.Body == nil {
		body = http.NoBody
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out.Header = r.Header.Clone()
	// Keep the original Host so virtual-host-style buckets keep working.
	out.Host = r.Host
	// Preserve the declared body length. Without it Go drops Content-Length
	// and re-sends the body with chunked transfer encoding; SigV4 clients
	// sign "content-length", so the gateway then computes a different
	// canonical request and rejects the upload with SignatureDoesNotMatch.
	out.ContentLength = r.ContentLength

	resp, err := p.client.Do(out)
	if err != nil {
		http.Error(w, "s3proxy: upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Intercept failed downloads: a 5xx on GET is almost always "not enough
	// EC shards to read the object". Enhance it with the recovery hint.
	if r.Method == http.MethodGet && resp.StatusCode >= 500 && looksLikeObjectPath(r.URL.Path) {
		if advice := p.fetchAdvice(); len(advice) > 0 {
			writeS3Failure(w, advice)
			return
		}
	}

	// Otherwise pass the response through unchanged.
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// clusterState returns the control plane's writable verdict, cached for
// stateTTL so a write burst does not hammer the control plane. A fetch error
// within the TTL falls back to the cached verdict; past the TTL it returns
// nil (fail open).
func (p *proxy) clusterState() *clusterState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cachedState != nil && time.Since(p.cachedAt) < p.stateTTL {
		return p.cachedState
	}
	st, err := fetchClusterState(p.client, p.controlURL)
	if err != nil {
		log.Printf("s3proxy: control plane unreachable (%v), failing open", err)
		return nil
	}
	p.cachedState = st
	p.cachedAt = time.Now()
	return st
}

func fetchClusterState(client *http.Client, controlURL string) (*clusterState, error) {
	resp, err := client.Get(controlURL + "/api/v1/cluster/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("control plane returned %d", resp.StatusCode)
	}
	var st clusterState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// isWriteMethod reports whether the request mutates data. GET/HEAD/OPTIONS and
// the S3 list operations pass through even when the cluster is read-only.
func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

func writeReadOnlyError(w http.ResponseWriter, st *clusterState) {
	msg := fmt.Sprintf(
		"Cluster is read-only: only %d of %d required storage nodes are online. "+
			"Please power on more nodes to enable writes.", st.ActiveNodes, st.MinWriteNodes)
	writeS3Error(w, "S3LiteClusterReadOnly", msg)
}

// declaredWriteSize returns how many bytes this write is going to store.
// SigV4 streaming uploads (aws-cli) sign a chunked wrapper, so the real
// payload size is in x-amz-decoded-content-length, not in Content-Length.
// A zero result means the size is unknown (chunked, empty or not applicable)
// and the size check is skipped.
func declaredWriteSize(r *http.Request) int64 {
	if v := r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	if r.ContentLength > 0 {
		return r.ContentLength
	}
	return 0
}

// addReserved books n bytes for an in-flight write that passed the size check,
// so concurrent uploads are checked against each other and not only against
// the (cached, up to stateTTL old) control plane numbers.
func (p *proxy) addReserved(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reserved += n
}

// release gives back the reservation made by a finished (or failed) write.
// It runs via defer on every exit path of ServeHTTP, so a cancelled or
// errored upload frees its space immediately.
func (p *proxy) release(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reserved -= n
	if p.reserved < 0 {
		p.reserved = 0
	}
}

// looksLikeObjectPath avoids intercepting the S3 service endpoints (/, /api,
// bucket listing) where 5xx has different meaning.
func looksLikeObjectPath(path string) bool {
	if path == "/" || strings.HasPrefix(path, "/?") {
		return false
	}
	return true
}

// fetchAdvice asks the control plane which offline nodes to power on for
// AT_RISK volumes and returns the union of the recommendations.
func (p *proxy) fetchAdvice() []string {
	u := p.controlURL + "/api/v1/volumes/risk"
	resp, err := p.client.Get(u)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var d struct {
		Volumes []struct {
			State        string   `json:"state"`
			NodesToStart []string `json:"nodes_to_start"`
		} `json:"volumes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, v := range d.Volumes {
		if v.State != "AT_RISK" {
			continue
		}
		for _, n := range v.NodesToStart {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// writeCapacityError refuses a write whose declared size cannot fit the
// cluster's usable (replication- and EC-adjusted) free space. The refusal
// goes to the S3 client as 507 and to the s3proxy log (see ServeHTTP).
func writeCapacityError(w http.ResponseWriter, st *clusterState, size, factor int64) {
	msg := fmt.Sprintf(
		"Object of %d bytes will not fit: only %d bytes of usable storage left "+
			"(%d raw free across %d nodes; every stored byte needs replication+EC overhead). "+
			"Delete objects or add nodes, then retry.",
		size, st.usableFree(factor), st.rawFree(), st.ActiveNodes)
	writeS3ErrorStatus(w, "S3LiteInsufficientCapacity", msg, http.StatusInsufficientStorage)
}

// writeS3Failure returns an S3-compatible error body (like MinIO/SeaweedFS do)
// with a human-readable recovery hint.
func writeS3Failure(w http.ResponseWriter, advice []string) {
	msg := "Storage nodes are offline. Please power on the following nodes " +
		"to recover the data: " + strings.Join(advice, ", ")
	writeS3Error(w, "S3LiteRecoveryRequired", msg)
}

func writeS3Error(w http.ResponseWriter, code, msg string) {
	writeS3ErrorStatus(w, code, msg, http.StatusServiceUnavailable)
}

func writeS3ErrorStatus(w http.ResponseWriter, code, msg string, status int) {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>%s</Code>
  <Message>%s</Message>
  <RequestId>s3lite-%d</RequestId>
</Error>`, code, xmlEscape(msg), time.Now().UnixMilli())

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", "s3lite")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&", "<", "<", ">", ">")
	return r.Replace(s)
}
