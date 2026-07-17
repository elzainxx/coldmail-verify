package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// This file: the brain's client for the fork's GET /v1/audit (P3 §4.2/§4.3). The wrapper endpoint
// was BUILT in Phase 2 (read/logged only); Phase 3 CONSUMES it to drive the gate. The client is a
// pure fact-fetcher — it returns the captured audit result verbatim (current_host_ip + warnings +
// audited_at); the brain (internal/audit) derives green/red, the GAP-24 cross-check, and the gate.
// The box renders no eligibility decision (Door 5: dumb box; all interpretation control-plane-side).

// AuditResult is the captured /v1/audit response the brain interprets (P3 §4.1, GT-4 ground truth).
// It is the wrapper's superset of host_audit: current_host_ip + warnings (green iff {}) + the
// wrapper-derived audit_status + audited_at. The brain re-derives audit_status itself (never trusts
// a box-computed verdict); AuditStatus here is captured for cross-check/logging only.
type AuditResult struct {
	CurrentHostIP string            `json:"current_host_ip"`
	Warnings      map[string]string `json:"warnings"`
	AuditStatus   string            `json:"audit_status"` // box-derived; advisory (brain re-derives)
	AuditedAt     string            `json:"audited_at"`   // RFC3339 UTC
}

// FetchAudit GETs <baseURL>/v1/audit and returns the captured result. A non-2xx or unreadable
// response is an error: the brain must NOT record a confident audit signal it couldn't fetch (an
// unreachable box is not "green"; the gate treats a fetch failure as no fresh audit → not eligible,
// harden-toward-unknown). It never fabricates a green.
func (c *HTTPClient) FetchAudit(ctx context.Context, baseURL, token string) (AuditResult, error) {
	url := trimSlash(baseURL) + "/v1/audit"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return AuditResult{}, fmt.Errorf("node: audit request: %w", err)
	}
	req.Header.Set("Authorization", token) // raw node token (NOT Bearer) — Appendix A.2
	resp, err := c.hc.Do(req)
	if err != nil {
		return AuditResult{}, &TransportError{Err: err}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return AuditResult{}, fmt.Errorf("node: read audit: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AuditResult{}, &HTTPStatusError{Status: resp.StatusCode, Body: body}
	}
	var ar AuditResult
	if err := json.Unmarshal(body, &ar); err != nil {
		return AuditResult{}, fmt.Errorf("node: decode audit: %w", err)
	}
	return ar, nil
}

// AuditClient is the narrow capability the scheduler needs (P3 §4.5). Kept as its own interface
// (not folded into node.Client) so the re-audit scheduler can be tested with a simulated audit
// surface — the local "controllable host_audit-shaped inputs" the kickoff mandates (green, ptr-
// warning, dns-warning, multi-homed-mismatch) — without a real box or HTTP.
type AuditClient interface {
	FetchAudit(ctx context.Context, baseURL, token string) (AuditResult, error)
}

// compile-time assertion that HTTPClient satisfies AuditClient.
var _ AuditClient = (*HTTPClient)(nil)

// ── The simulated audit surface (LOCAL test/dev substrate, P3 kickoff) ──────────────────────
// A box on my PC has no real PTR/egress-25. So the local build exercises every gate/state path
// against a CONTROLLABLE audit surface constructed here — green, ptr-warning, dns-warning, and the
// multi-homed egress≠audit-IP box (GAP-24). This is a TEST/dev artifact, never a production path;
// a real box's FetchAudit returns the real host_audit. Marked so it is never mistaken for a stub
// that fakes a real-IP result "green" (it returns whatever shape the caller constructed).

// SimAuditSurface is a fake AuditClient returning per-baseURL canned AuditResults. It lets the local
// build drive the gate/state-machine end-to-end without a real IP. Real-IP proofs (actual egress-25,
// real PTR propagation) remain PENDING-REAL-BOX — this surface proves the LOGIC, not the network.
type SimAuditSurface struct {
	byURL map[string]AuditResult
	err   map[string]error // per-URL fetch error (simulates an unreachable box)
}

// NewSimAuditSurface builds an empty simulated surface.
func NewSimAuditSurface() *SimAuditSurface {
	return &SimAuditSurface{byURL: map[string]AuditResult{}, err: map[string]error{}}
}

// Set installs a canned audit result for a base URL.
func (s *SimAuditSurface) Set(baseURL string, r AuditResult) *SimAuditSurface {
	s.byURL[baseURL] = r
	return s
}

// SetError makes FetchAudit fail for a base URL (simulate an unreachable/500 box).
func (s *SimAuditSurface) SetError(baseURL string, err error) *SimAuditSurface {
	s.err[baseURL] = err
	return s
}

// FetchAudit satisfies AuditClient from the canned map. An unset URL is a transport error (a box
// we don't know about is unreachable — never silently "green").
func (s *SimAuditSurface) FetchAudit(_ context.Context, baseURL, _ string) (AuditResult, error) {
	if err, ok := s.err[baseURL]; ok {
		return AuditResult{}, err
	}
	if r, ok := s.byURL[baseURL]; ok {
		return r, nil
	}
	return AuditResult{}, &TransportError{Err: fmt.Errorf("sim: no audit surface for %q", baseURL)}
}

// ── Simulated-surface constructors: the four kickoff-mandated shapes ────────────────────────
// Each returns a controllable host_audit-shaped input. hostIP is the audit's self-detected IP —
// set it EQUAL to the node's expected egress IP for a valid green, or DIFFERENT to model the
// multi-homed GAP-24 trap. auditedAt lets a test control freshness deterministically.

// SimGreen is a green audit on hostIP (warnings=={}). Eligible only if hostIP == the node's
// expected public egress IP (GAP-24 cross-check) and port25_open (GAP-15) — both enforced brain-side.
func SimGreen(hostIP, auditedAt string) AuditResult {
	return AuditResult{CurrentHostIP: hostIP, Warnings: map[string]string{}, AuditStatus: "green", AuditedAt: auditedAt}
}

// SimPTRWarning models a red audit whose PTR (reverse DNS) doesn't reference the verifier domain —
// the make-or-break record (§4.2). Real MX distrust such a source → verdicts degrade to unknown.
func SimPTRWarning(hostIP, auditedAt string) AuditResult {
	return AuditResult{
		CurrentHostIP: hostIP,
		Warnings:      map[string]string{"ptr": "PTR-record does not reference to current verifier domain"},
		AuditStatus:   "red", AuditedAt: auditedAt,
	}
}

// SimDNSWarning models a red audit whose verifier-domain A-record doesn't resolve to the host IP.
func SimDNSWarning(hostIP, auditedAt string) AuditResult {
	return AuditResult{
		CurrentHostIP: hostIP,
		Warnings:      map[string]string{"dns": "A-record of verifier domain not refers to current host ip address"},
		AuditStatus:   "red", AuditedAt: auditedAt,
	}
}

// SimMultiHomedMismatch models the GAP-24 trap (§6.6.1): the audit went GREEN, but it self-detected
// the PRIVATE ingress IP — NOT the public egress IP the node routes SMTP through. A naive "green ==
// ready" reading would run :smtp on an unaudited public IP; the brain's four-way cross-check
// (audit IP == expected egress IP) catches it and refuses eligibility. privateIP is what the box
// wrongly audited; the caller compares it against the node's real expected egress IP.
func SimMultiHomedMismatch(privateIP, auditedAt string) AuditResult {
	// Green warnings ({}) but on the WRONG (private) IP — the silent-pass this trap is about.
	return AuditResult{CurrentHostIP: privateIP, Warnings: map[string]string{}, AuditStatus: "green", AuditedAt: auditedAt}
}

// AuditedAtNow is a small helper so tests/schedulers can stamp a deterministic audited_at.
func AuditedAtNow(t time.Time) string { return t.UTC().Format(time.RFC3339) }
