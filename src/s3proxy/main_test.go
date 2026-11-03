package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
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
