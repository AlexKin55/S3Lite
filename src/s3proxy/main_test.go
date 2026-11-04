package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The write guard must refuse mutations while the control plane reports the
// cluster read-only, let reads through, and fail open when the control plane
// is unreachable.

func TestIsWriteMethod(t *testing.T) {
	for _, m := range []string{http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch} {
		if !isWriteMethod(m) {
			t.Errorf("%s must be treated as a write", m)
		}
	}
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if isWriteMethod(m) {
			t.Errorf("%s must NOT be treated as a write", m)
		}
	}
}

func newTestProxy(t *testing.T, control *httptest.Server, stateTTL time.Duration) *proxy {
	t.Helper()
	return &proxy{
		upstream:   control.URL, // replaced per-test; upstream may point anywhere
		controlURL: control.URL,
		client:     control.Client(),
		stateTTL:   stateTTL,
	}
}

func TestWriteRefusedWhenClusterReadOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be reached for writes in read-only mode, got %s", r.Method)
	}))
	defer upstream.Close()

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"nodes":20,"active_nodes":7,"min_write_nodes":10,"writable":false}`))
	}))
	defer control.Close()

	p := newTestProxy(t, control, time.Second)
	p.upstream = upstream.URL

	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(method, "/bucket/key", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: expected 503, got %d", method, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "S3LiteClusterReadOnly") {
			t.Fatalf("%s: expected S3LiteClusterReadOnly error code, got: %s", method, body)
		}
		if !strings.Contains(body, "7 of 10") {
			t.Fatalf("%s: expected node counts in the message, got: %s", method, body)
		}
	}
}

func TestWriteAllowedWhenWritable(t *testing.T) {
	var hit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"active_nodes":20,"min_write_nodes":10,"writable":true}`))
	}))
	defer control.Close()

	p := newTestProxy(t, control, time.Second)
	p.upstream = upstream.URL

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/bucket/key", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !hit.Load() {
		t.Fatal("request must reach the upstream when the cluster is writable")
	}
}

func TestReadsPassWhenClusterReadOnly(t *testing.T) {
	var hit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"active_nodes":7,"min_write_nodes":10,"writable":false}`))
	}))
	defer control.Close()

	p := newTestProxy(t, control, time.Second)
	p.upstream = upstream.URL

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bucket/key", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET must pass through in read-only mode, got %d", rec.Code)
	}
	if !hit.Load() {
		t.Fatal("GET must reach the upstream")
	}
}

// A write whose declared size cannot fit the cluster's free space (with the
// write amplification applied) must be refused upfront with 507 and a clear
// S3 error code, instead of dying mid-upload with a generic 503.
func TestWriteRefusedWhenObjectWontFit(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// raw free 1000 bytes, writeFactor 5 -> 200 usable bytes
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"writable":true,"active_nodes":10,"min_write_nodes":10,"free_bytes":1000}`))
	}))
	defer control.Close()

	p := newTestProxy(t, control, time.Second)
	p.upstream = upstream.URL
	p.writeFactor = 5

	req := httptest.NewRequest(http.MethodPut, "/bucket/big.bin", nil)
	req.ContentLength = 201
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("expected 507, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "S3LiteInsufficientCapacity") {
		t.Fatalf("expected S3LiteInsufficientCapacity error code, got: %s", body)
	}
	if !strings.Contains(body, "201") {
		t.Fatalf("expected the object size in the message, got: %s", body)
	}
	if upstreamHits.Load() != 0 {
		t.Fatal("upstream must not be reached for an object that cannot fit")
	}
}

// A write within the usable space passes the check and reaches the upstream.
func TestWriteAllowedWithinCapacity(t *testing.T) {
	var hit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"writable":true,"active_nodes":10,"min_write_nodes":10,"free_bytes":1000}`))
	}))
	defer control.Close()

	p := newTestProxy(t, control, time.Second)
	p.upstream = upstream.URL
	p.writeFactor = 5

	req := httptest.NewRequest(http.MethodPut, "/bucket/ok.bin", nil)
	req.ContentLength = 200 // exactly the usable space
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !hit.Load() {
		t.Fatal("request must reach the upstream when the object fits")
	}
}

// A control plane that does not report capacity numbers (older version) must
// not block writes: the size check fails open.
func TestWritePassesWithoutCapacityNumbers(t *testing.T) {
	var hit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"writable":true,"active_nodes":10,"min_write_nodes":10}`))
	}))
	defer control.Close()

	p := newTestProxy(t, control, time.Second)
	p.upstream = upstream.URL
	p.writeFactor = 5

	req := httptest.NewRequest(http.MethodPut, "/bucket/big.bin", nil)
	req.ContentLength = 1 << 30
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 without capacity numbers, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !hit.Load() {
		t.Fatal("request must reach the upstream when capacity is not reported")
	}
}

// Two simultaneous uploads cannot both pass the check against the same cached
// capacity numbers: the first reserves its bytes for its duration, the second
// is refused while the first is still in flight.
func TestConcurrentWritesReserveCapacity(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the first upload in flight
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"writable":true,"active_nodes":10,"min_write_nodes":10,"free_bytes":1000}`))
	}))
	defer control.Close()

	p := newTestProxy(t, control, time.Second)
	p.upstream = upstream.URL
	p.writeFactor = 5

	var wg sync.WaitGroup
	first := make(chan int, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest(http.MethodPut, "/bucket/first.bin", nil)
		req.ContentLength = 150 // usable is 200; leaves 50
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		first <- rec.Code
	}()

	// Wait until the first request is in flight (reserved its bytes).
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		r := p.reserved
		p.mu.Unlock()
		if r == 150 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	req := httptest.NewRequest(http.MethodPut, "/bucket/second.bin", nil)
	req.ContentLength = 100 // 150 reserved + 100 > 200 usable
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("second concurrent write: expected 507, got %d body=%s", rec.Code, rec.Body.String())
	}

	close(release)
	wg.Wait()
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first concurrent write: expected 200, got %d", code)
	}
	// After both requests finish, the reservations must be released.
	if p.reserved != 0 {
		t.Fatalf("reserved = %d after completion, want 0", p.reserved)
	}
}

// SigV4 streaming uploads declare the payload size in
// x-amz-decoded-content-length, not Content-Length (which covers the signed
// chunking wrapper).
func TestDeclaredWriteSizePrefersDecodedLength(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/b/k", nil)
	r.Header.Set("X-Amz-Decoded-Content-Length", "4294967296")
	r.ContentLength = 4294967296 + 4096
	if got := declaredWriteSize(r); got != 4294967296 {
		t.Fatalf("declaredWriteSize = %d, want the decoded 4294967296", got)
	}
	r2 := httptest.NewRequest(http.MethodPut, "/b/k", nil)
	r2.ContentLength = 42
	if got := declaredWriteSize(r2); got != 42 {
		t.Fatalf("declaredWriteSize = %d, want 42", got)
	}
	if got := declaredWriteSize(httptest.NewRequest(http.MethodGet, "/b/k", nil)); got != 0 {
		t.Fatalf("declaredWriteSize = %d for a request without length, want 0", got)
	}
}

// fits must fail closed on a full cluster and stay open without numbers.
func TestClusterStateFits(t *testing.T) {
	full := &clusterState{Writable: true, FreeBytes: new(int64)}
	if full.fits(1, 0, 5) {
		t.Fatal("a reported-full cluster must refuse any write")
	}
	fb := int64(1000)
	st := &clusterState{Writable: true, FreeBytes: &fb}
	if st.fits(201, 0, 5) {
		t.Fatal("201 usable-needed bytes must not fit into 1000 raw free at factor 5")
	}
	if !st.fits(200, 0, 5) {
		t.Fatal("200 bytes must fit into 1000 raw free at factor 5")
	}
	unreported := &clusterState{Writable: true}
	if !unreported.fits(1<<40, 0, 5) {
		t.Fatal("writes must pass when the control plane reports no numbers")
	}
}

func TestFailOpenWhenControlPlaneDown(t *testing.T) {
	var hit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// Dead control plane: nothing listens here.
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	controlURL := control.URL
	control.Close()

	p := newTestProxy(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})), time.Second)
	p.upstream = upstream.URL
	p.controlURL = controlURL

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/bucket/key", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("writes must fail OPEN when the control plane is unreachable, got %d", rec.Code)
	}
	if !hit.Load() {
		t.Fatal("write must reach the upstream when the control plane is down")
	}
}

func TestStateCacheAvoidsControlPlaneHits(t *testing.T) {
	var controlHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		controlHits.Add(1)
		_, _ = w.Write([]byte(`{"active_nodes":20,"min_write_nodes":10,"writable":true}`))
	}))
	defer control.Close()

	p := newTestProxy(t, control, 10*time.Second)
	p.upstream = upstream.URL

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/bucket/key", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rec.Code)
		}
	}
	if got := controlHits.Load(); got != 1 {
		t.Fatalf("control plane must be polled once within the TTL, got %d polls", got)
	}
}
