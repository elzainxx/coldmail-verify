package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPClient is the Phase-2 live node client (Door 5: pure function email → raw engine JSON).
// It calls the fork wrapper's POST /v1/verify (§4.1) with the node token and returns the raw
// response bytes VERBATIM — the brain parses/classifies/scores; the box renders no verdict.
// A brain-side deadline (§6.5) guards against a hung box: a timeout is the caller's smtp_timeout.
type HTTPClient struct {
	hc *http.Client
}

// VerifyOptions carries the P2 /v1/verify request knobs (§4.1) beyond the bare email.
type VerifyOptions struct {
	ValidationType string // "smtp" etc.; "" => wrapper/engine default
	ProbeCatchall  *bool  // nil => wrapper default (true for :smtp); explicit false opts out
	RequestID      string // brain-supplied, echoed for trace correlation
}

// NewHTTPClient builds a live client with a per-request deadline. deadline<=0 falls back to a
// safe default; the brain deadline MUST exceed the box's own SMTP timeouts (X-7 coupling, config).
func NewHTTPClient(deadline time.Duration) *HTTPClient {
	if deadline <= 0 {
		deadline = 15 * time.Second
	}
	return &HTTPClient{hc: &http.Client{Timeout: deadline}}
}

// Verify satisfies Client for the bare (email-only) call — used where options are defaulted.
func (c *HTTPClient) Verify(ctx context.Context, baseURL, token, email string) (RawEngineResult, error) {
	return c.VerifyWith(ctx, baseURL, token, email, VerifyOptions{})
}

// VerifyWith POSTs to <baseURL>/v1/verify and returns the raw JSON body. A non-2xx with a body is
// returned WITH an error carrying the status, so the caller can map it (401→route elsewhere,
// 500→connection_failed/unknown, §4.1 failure modes). A transport timeout → a timeout error the
// caller maps to smtp_timeout/unknown. It NEVER returns a fabricated verdict.
func (c *HTTPClient) VerifyWith(ctx context.Context, baseURL, token, email string, opt VerifyOptions) (RawEngineResult, error) {
	body := map[string]any{"email": email}
	if opt.ValidationType != "" {
		body["validation_type"] = opt.ValidationType
	}
	if opt.ProbeCatchall != nil {
		body["probe_catchall"] = *opt.ProbeCatchall
	}
	if opt.RequestID != "" {
		body["request_id"] = opt.RequestID
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("node: marshal request: %w", err)
	}

	url := trimSlash(baseURL) + "/v1/verify"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("node: new request: %w", err)
	}
	req.Header.Set("Authorization", token) // raw node token (NOT Bearer) — Appendix A.2
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		// Transport error (incl. timeout/context deadline) → caller maps to smtp_timeout/unknown.
		return nil, &TransportError{Err: err}
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap on an engine blob
	if readErr != nil {
		return nil, fmt.Errorf("node: read body: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, &HTTPStatusError{Status: resp.StatusCode, Body: raw}
	}
	return raw, nil
}

// TransportError marks a connection/timeout failure (no HTTP response) → smtp_timeout/unknown.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return "node: transport: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// HTTPStatusError marks a non-2xx response with its body preserved (§4.1 failure-mode mapping).
type HTTPStatusError struct {
	Status int
	Body   []byte
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("node: http status %d", e.Status)
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// compile-time assertion that HTTPClient satisfies Client.
var _ Client = (*HTTPClient)(nil)
