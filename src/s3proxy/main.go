package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// s3proxy sits in front of the SeaweedFS S3 gateway. It forwards every request
// unchanged, but when a download (GET) fails with 5xx it asks the S3Lite control
// plane which PCs are offline (nodes_to_start for AT_RISK volumes) and returns a
// machine-readable S3 error that tells the caller exactly which nodes to power on.
//
// Usage:
//   bin/s3proxy -listen=:8333 -upstream=http://127.0.0.1:8332 -control=http://192.168.1.9:8099

func main() {
	listen := flag.String("listen", ":8333", "external S3 listener")
	upstream := flag.String("upstream", "http://127.0.0.1:8332", "SeaweedFS S3 gateway address")
	control := flag.String("control", "http://192.168.1.9:8099", "S3Lite control plane address")
	flag.Parse()

	proxy := &proxy{
		upstream:   *upstream,
		controlURL: *control,
		client:     &http.Client{Timeout: 60 * time.Second},
	}

	log.Printf("s3proxy on %s -> %s (control %s)", *listen, *upstream, *control)
	if err := http.ListenAndServe(*listen, proxy); err != nil {
		log.Fatal(err)
	}
}

type proxy struct {
	upstream   string
	controlURL string
	client     *http.Client
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

// writeS3Failure returns an S3-compatible error body (like MinIO/SeaweedFS do)
// with a human-readable recovery hint.
func writeS3Failure(w http.ResponseWriter, advice []string) {
	msg := "Storage nodes are offline. Please power on the following nodes " +
		"to recover the data: " + strings.Join(advice, ", ")
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>S3LiteRecoveryRequired</Code>
  <Message>%s</Message>
  <RequestId>s3lite-%d</RequestId>
</Error>`, xmlEscape(msg), time.Now().UnixMilli())

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", "s3lite")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, body)
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&", "<", "<", ">", ">")
	return r.Replace(s)
}
