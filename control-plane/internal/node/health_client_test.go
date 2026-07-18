package node

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The live liveness probe: 2xx → Up, non-2xx → down, unreachable → down (never errors the caller).
func TestFetchLiveness(t *testing.T) {
	c := NewHTTPClient(2 * time.Second)
	ctx := context.Background()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthcheck" {
			t.Errorf("expected /healthcheck, got %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer up.Close()
	if l := c.FetchLiveness(ctx, up.URL, "tok"); !l.Up {
		t.Fatalf("2xx must be Up, got %+v", l)
	}

	wedged := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer wedged.Close()
	if l := c.FetchLiveness(ctx, wedged.URL, "tok"); l.Up {
		t.Fatalf("a 5xx (wedged) box must be down, got Up")
	}

	// Unreachable → down, no panic, error captured.
	if l := c.FetchLiveness(ctx, "http://127.0.0.1:0", "tok"); l.Up || l.Err == nil {
		t.Fatalf("an unreachable box must be down with an error, got %+v", l)
	}
}

// The simulated liveness surface: default up, SetDown flips a box.
func TestSimLivenessSurface(t *testing.T) {
	s := NewSimLivenessSurface()
	if l := s.FetchLiveness(context.Background(), "http://box1", ""); !l.Up {
		t.Fatal("unset box defaults to up")
	}
	s.SetDown("http://box1", true)
	if l := s.FetchLiveness(context.Background(), "http://box1", ""); l.Up {
		t.Fatal("a box marked down must report down")
	}
}
