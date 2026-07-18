package node

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// This file: the brain's LIVENESS probe for the P4 health-watcher (§5.2). It polls the box's stock
// GET /healthcheck (P1 A.2) — a cheap "is the box up and answering" check, separate from the
// audit signal (/v1/audit, PTR/DNS) and from block-rate (the LOG). A box can be audit-green yet
// down/wedged (5xx, unreachable); liveness catches THAT (§5.2 "box down / unreachable / wedged").
// It is a pure fact-fetcher: it never renders a health verdict — the watcher (internal/health)
// decides. The wrapper endpoints are Door-5-frozen (built in P1/P2); P4 only CONSUMES them.

// Liveness is one liveness poll result (§5.2). Up=true means the box answered 2xx on /healthcheck
// within the client deadline. On any transport error / non-2xx, Up=false and Err carries why (a
// wedged box returns 5xx; an unreachable box returns a transport error). The watcher counts
// consecutive Up=false to trip the liveness quarantine (§5.4, M consecutive).
type Liveness struct {
	Up  bool
	Err error
}

// FetchLiveness GETs <baseURL>/healthcheck and reports whether the box is up (2xx). It never errors
// the caller — a failure is captured in Liveness{Up:false, Err} so the watcher treats "can't reach"
// as a liveness miss (harden-toward-unknown: an unreachable box is NOT healthy), not a crash.
func (c *HTTPClient) FetchLiveness(ctx context.Context, baseURL, token string) Liveness {
	url := trimSlash(baseURL) + "/healthcheck"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Liveness{Up: false, Err: fmt.Errorf("node: liveness request: %w", err)}
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return Liveness{Up: false, Err: &TransportError{Err: err}}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) // drain to reuse the connection
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Liveness{Up: false, Err: &HTTPStatusError{Status: resp.StatusCode}}
	}
	return Liveness{Up: true}
}

// LivenessProbe is the narrow capability the health-watcher needs (§5.2). Satisfied by *HTTPClient;
// kept as its own interface so the watcher tests inject a controllable liveness surface (up/down per
// box) alongside the SimAuditSurface — the local multi-node substrate the P4 kickoff mandates.
type LivenessProbe interface {
	FetchLiveness(ctx context.Context, baseURL, token string) Liveness
}

// compile-time assertion that HTTPClient satisfies LivenessProbe.
var _ LivenessProbe = (*HTTPClient)(nil)

// ── The simulated liveness surface (LOCAL test/dev substrate, P4) ───────────────────────────────
// A pool of N boxes on my PC has no real /healthcheck to poll. SimLivenessSurface returns per-baseURL
// canned up/down so the watcher's liveness trigger (§5.4, M-consecutive-down → quarantine) is proven
// against a controllable multi-node surface — never a real box. A TEST/dev artifact.

// SimLivenessSurface is a fake LivenessProbe: per-baseURL up/down, default up for unset URLs.
type SimLivenessSurface struct {
	down map[string]bool
}

// NewSimLivenessSurface builds a surface where every box is up unless SetDown.
func NewSimLivenessSurface() *SimLivenessSurface {
	return &SimLivenessSurface{down: map[string]bool{}}
}

// SetDown marks a box down (FetchLiveness returns Up=false for it).
func (s *SimLivenessSurface) SetDown(baseURL string, down bool) *SimLivenessSurface {
	s.down[baseURL] = down
	return s
}

// FetchLiveness returns Up=false iff the box was marked down; otherwise Up=true.
func (s *SimLivenessSurface) FetchLiveness(_ context.Context, baseURL, _ string) Liveness {
	if s.down[baseURL] {
		return Liveness{Up: false, Err: &TransportError{Err: fmt.Errorf("sim: box %q down", baseURL)}}
	}
	return Liveness{Up: true}
}

// compile-time assertion that SimLivenessSurface satisfies LivenessProbe.
var _ LivenessProbe = (*SimLivenessSurface)(nil)
