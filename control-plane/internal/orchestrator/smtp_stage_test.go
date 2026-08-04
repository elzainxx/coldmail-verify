package orchestrator

import (
	"context"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/node"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/router"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// nxdomainResolver yields a definitive "no such host" (NXDOMAIN) for the test domain → the cheap
// MX layer gates invalid/no_mx (a terminal cheap gate). Used to prove the SMTP stage is skipped.
func nxdomainResolver() fixtureResolver {
	return fixtureResolver{err: map[string]error{
		"nxdomain.example": &net.DNSError{Err: "no such host", Name: "nxdomain.example", IsNotFound: true},
	}}
}

// fakeNode is a node.Client that returns a canned /v1/verify body — no HTTP, no socket. It lets
// the FULL orchestrator lifecycle (cheap → SMTP-eligible → node call → parse → score → fold →
// LOG+sidecar → settled-only cache) be proven at the brain level, complementing the socket-level
// smtp_mock proof. err (if set) simulates a node transport/HTTP failure.
type fakeNode struct {
	body []byte
	err  error
}

func (f fakeNode) Verify(_ context.Context, _, _, _ string) (node.RawEngineResult, error) {
	return f.body, f.err
}

// testEgressIP is the node's public egress IP used across the SMTP-stage tests. Because the P3
// §4.4 gate now requires an ELIGIBLE node (active ∧ green ∧ port25_open ∧ fresh ∧ GAP-24 egress
// match), smtpSetup seeds the node's health as fully eligible so the SMTP-PATH behaviors (valid /
// invalid / catch-all / 552 / timeout / redaction) are still exercised. The gate itself is proven
// separately (audit + router tests, and the P3 skip tests below).
const testEgressIP = "203.0.113.10"

// smtpSetup builds an orchestrator whose cheap path yields the SMTP-eligible residue (a clean
// corp domain → risky/plausible), wired to a fake node returning `body`, with the node seeded as
// a fully :smtp-ELIGIBLE, warmed-up box (P3 §4.4). Freshness is disabled (MaxAuditAge=0) so these
// behavioral tests don't depend on wall-clock; the freshness conjunct is proven in the audit pkg.
func smtpSetup(t *testing.T, body []byte, nodeErr error, ttl time.Duration) (*Orchestrator, *store.MemStore, *policy.Definition) {
	t.Helper()
	h, err := hash.NewHasher([]byte("smtp-stage-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	mem := store.NewMem(nil)
	// Seed the node as eligible: active, audit-green, egress-25 proven, audited on its expected IP.
	mem.SeedNodeHealth(store.NodeHealth{
		NodeID: "n1", Status: "active", AuditStatus: "green", Port25Open: true,
		LastAuditAt: time.Now(), LastAuditHostIP: testEgressIP,
	})
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	nodes := []router.Node{{ID: "n1", BaseURL: "http://mock", TokenRef: "n1", Enabled: true, PublicEgressIP: testEgressIP}}
	o := New(Deps{
		Hasher: h, Store: mem, Resolver: corpResolver(), CacheTTL: ttl,
		ConfigFingerprint: "cfg_sha256:test",
		SMTP: &SMTPDeps{
			Client:      fakeNode{body: body, err: nodeErr},
			Registry:    router.New(nodes, nil),
			TokenFor:    func(string) string { return "tok" },
			Health:      mem,
			MaxAuditAge: 0, // freshness disabled for behavioral tests (proven in the audit pkg)
		},
	})
	return o, mem, def
}

// Appendix B row 1 — valid ONLY via target-250 + probe-REJECTED (the seeded-probe path, GAP-14).
func TestSMTPStage_Valid_TargetAcceptedProbeRejected(t *testing.T) {
	body := []byte(`{
	  "wrapper_api_version":"v1","email":"jane@acmecorp.com","validation_type":"smtp","success":true,
	  "smtp_debug":null,
	  "catchall":{"probed":true,"probe_local_part":"nx-seed","accepted":false,"conclusion":"not_catchall"},
	  "mx":{"domain":"acmecorp.com","mail_servers":["1.2.3.4"]}}`)
	o, mem, def := smtpSetup(t, body, nil, time.Hour)
	resp, err := o.Verify(context.Background(), "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusValid || resp.Reason != vocab.ReasonAccepted {
		t.Fatalf("target 250 + probe rejected → valid/accepted, got %s/%s", resp.Status, resp.Reason)
	}
	if resp.Recommendation != vocab.RecSend {
		t.Fatalf("valid/accepted → send, got %s", resp.Recommendation)
	}
	// A settled valid is cached and its raw blob rides the sidecar (§6.4).
	if len(mem.Raw) != 1 {
		t.Fatalf("valid SMTP verdict must write a raw sidecar row, got %d", len(mem.Raw))
	}
	if mem.CacheLen() != 1 {
		t.Fatalf("settled valid must be cached, got cache len %d", mem.CacheLen())
	}
	if s, _ := resp.Signals["catchall_conclusion"].(string); s != "not_catchall" {
		t.Fatalf("catchall_conclusion signal want not_catchall, got %q", s)
	}
}

// Appendix B row 4 — accept-all (probe also 250) → risky/catch_all, NEVER valid; writes catchall.
func TestSMTPStage_CatchAll_IsRiskyAndPersisted(t *testing.T) {
	body := []byte(`{
	  "wrapper_api_version":"v1","email":"jane@acmecorp.com","validation_type":"smtp","success":true,
	  "smtp_debug":null,
	  "catchall":{"probed":true,"probe_local_part":"nx-seed","accepted":true,"conclusion":"catchall"},
	  "mx":{"domain":"acmecorp.com","mail_servers":["1.2.3.4"]}}`)
	o, mem, def := smtpSetup(t, body, nil, time.Hour)
	resp, err := o.Verify(context.Background(), "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusRisky || resp.Reason != vocab.ReasonCatchAll {
		t.Fatalf("catch-all → risky/catch_all, got %s/%s", resp.Status, resp.Reason)
	}
	// §5.6: the brain persisted the catch-all conclusion.
	is, known, _ := mem.CatchAllState(context.Background(), "acmecorp.com")
	if !known || !is {
		t.Fatalf("catch-all conclusion must be persisted true, got known=%v is=%v", known, is)
	}
}

// Appendix B row 2 — 550 no-user → invalid/mailbox_not_found (address-fault). Settled → cached.
func TestSMTPStage_550NoUser_IsInvalid(t *testing.T) {
	body := []byte(`{
	  "wrapper_api_version":"v1","email":"ghost@acmecorp.com","validation_type":"smtp","success":false,
	  "smtp_debug":[{"mail_host":"1.2.3.4","port_opened":true,"connection":true,"helo":true,
	    "rcptto":false,"errors":{"rcptto":"550 5.1.1 User unknown\n"}}],
	  "mx":{"domain":"acmecorp.com","mail_servers":["1.2.3.4"]}}`)
	o, _, def := smtpSetup(t, body, nil, time.Hour)
	resp, err := o.Verify(context.Background(), "ghost@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusInvalid || resp.Reason != vocab.ReasonMailboxNotFound {
		t.Fatalf("550 no-user → invalid/mailbox_not_found, got %s/%s", resp.Status, resp.Reason)
	}
	if resp.Recommendation != vocab.RecSkip {
		t.Fatalf("invalid → skip (composability), got %s", resp.Recommendation)
	}
	if s, _ := resp.Signals["smtp_code"].(string); s != "550" {
		t.Fatalf("smtp_code must be parsed to 550 (GAP-4), got %q", s)
	}
}

// Appendix B row 3 — a your-fault block → unknown, and NOT cached (§6.5). Uses the real 421 delta.
func TestSMTPStage_BlockDelta421_IsUnknownNotCached(t *testing.T) {
	body := []byte(`{
	  "wrapper_api_version":"v1","email":"jane@acmecorp.com","validation_type":"smtp","success":false,
	  "smtp_debug":[{"mail_host":"1.2.3.4","port_opened":true,"connection":true,"helo":true,
	    "rcptto":false,"errors":{"rcptto":"421 Service not available\n"}}],
	  "mx":{"domain":"acmecorp.com","mail_servers":["1.2.3.4"]}}`)
	o, mem, def := smtpSetup(t, body, nil, time.Hour)
	resp, err := o.Verify(context.Background(), "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusUnknown {
		t.Fatalf("421 block → unknown, got %s/%s", resp.Status, resp.Reason)
	}
	if mem.CacheLen() != 0 {
		t.Fatalf("a transient unknown must NOT be cached (§6.5/OQ-P2-6), got cache len %d", mem.CacheLen())
	}
	// But it is still LOGGED (Door 2) with its raw transcript in the sidecar.
	if len(mem.Log) != 1 || len(mem.Raw) != 1 {
		t.Fatalf("unknown must still be logged + sidecar'd, got log=%d raw=%d", len(mem.Log), len(mem.Raw))
	}
}

// Appendix B row 7 — 552 mailbox full → valid/mailbox_full (positive existence proof), rec=review.
func TestSMTPStage_552MailboxFull_IsValidReview(t *testing.T) {
	body := []byte(`{
	  "wrapper_api_version":"v1","email":"jane@acmecorp.com","validation_type":"smtp","success":false,
	  "smtp_debug":[{"mail_host":"1.2.3.4","port_opened":true,"connection":true,"helo":true,
	    "rcptto":false,"errors":{"rcptto":"552 mailbox full\n"}}],
	  "mx":{"domain":"acmecorp.com","mail_servers":["1.2.3.4"]}}`)
	o, _, def := smtpSetup(t, body, nil, time.Hour)
	resp, err := o.Verify(context.Background(), "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusValid || resp.Reason != vocab.ReasonMailboxFull {
		t.Fatalf("552 → valid/mailbox_full, got %s/%s", resp.Status, resp.Reason)
	}
	if resp.Recommendation != vocab.RecReview {
		t.Fatalf("mailbox_full valid must carry rec=review (full-now caveat), got %s", resp.Recommendation)
	}
}

// A node transport failure (hung box) → unknown/smtp_timeout, NEVER a fabricated verdict, and
// the cheap-eligible request still LOGs (Door 2).
func TestSMTPStage_NodeTimeout_DegradesToUnknown(t *testing.T) {
	o, mem, def := smtpSetup(t, nil, &node.TransportError{Err: context.DeadlineExceeded}, time.Hour)
	resp, err := o.Verify(context.Background(), "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusUnknown || resp.Reason != vocab.ReasonSMTPTimeout {
		t.Fatalf("node timeout → unknown/smtp_timeout, got %s/%s", resp.Status, resp.Reason)
	}
	if resp.Status == vocab.StatusValid || resp.Status == vocab.StatusInvalid {
		t.Fatal("a node failure must NEVER fabricate valid/invalid")
	}
	if len(mem.Log) != 1 {
		t.Fatalf("degraded verdict must still be logged, got %d", len(mem.Log))
	}
	if mem.CacheLen() != 0 {
		t.Fatal("a timeout unknown must not be cached")
	}
}

// DOOR 3 (the raw full email is NEVER persisted): the raw blob written to verification_raw must
// NOT contain the plaintext local-part — not in the echoed `email` field, and not in an SMTP
// rejection body that echoes the address. The domain stays (Door-3-clear); email_hash is the join
// key. This locks the fix for the CRITICAL review finding (ADR-0008).
func TestSMTPStage_RawBlobRedactsPlaintextAddress(t *testing.T) {
	// The wrapper echoes the checked address in `email` AND the smtp_mock/real server echoes it
	// inside the 550 body — both must be scrubbed from the persisted blob.
	body := []byte(`{
	  "wrapper_api_version":"v1","email":"secretlocal@acmecorp.com","validation_type":"smtp","success":false,
	  "smtp_debug":[{"mail_host":"1.2.3.4","port_opened":true,"connection":true,"helo":true,
	    "rcptto":false,"errors":{"rcptto":"550 <secretlocal@acmecorp.com> User unknown\n"}}],
	  "mx":{"domain":"acmecorp.com","mail_servers":["1.2.3.4"]}}`)
	o, mem, def := smtpSetup(t, body, nil, time.Hour)
	if _, err := o.Verify(context.Background(), "secretlocal@acmecorp.com", def); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(mem.Raw) != 1 {
		t.Fatalf("expected one raw sidecar row, got %d", len(mem.Raw))
	}
	blob := string(mem.Raw[0].Raw)
	if strings.Contains(blob, "secretlocal") {
		t.Fatalf("DOOR 3 VIOLATION: plaintext local-part 'secretlocal' persisted in verification_raw:\n%s", blob)
	}
	// The domain is Door-3-clear and must remain (join/analytics rely on it).
	if !strings.Contains(blob, "acmecorp.com") {
		t.Fatal("the domain (Door-3-clear) should remain in the raw blob")
	}
	// The transcript's non-identity content (the SMTP code) must survive redaction (join gold).
	if !strings.Contains(blob, "550") {
		t.Fatal("redaction must preserve the transcript verbatim except the address identity")
	}
}

// DOOR 3, short local-part edge (the review HIGH): a 1-2 char local-part must ALSO be scrubbed,
// including where a body prints ONLY the bare local part ("User jo does not exist"). The redaction
// uses word boundaries + NO length guard, so it must catch a standalone "jo" without shredding the
// "jo" inside "json"/"rejected".
func TestSMTPStage_RawBlobRedactsShortLocalPart(t *testing.T) {
	body := []byte(`{
	  "wrapper_api_version":"v1","email":"jo@acmecorp.com","validation_type":"smtp","success":false,
	  "smtp_debug":[{"mail_host":"1.2.3.4","port_opened":true,"connection":true,"helo":true,
	    "rcptto":false,"errors":{"rcptto":"550 User jo does not exist (rejected via json policy)\n"}}],
	  "mx":{"domain":"acmecorp.com","mail_servers":["1.2.3.4"]}}`)
	o, mem, def := smtpSetup(t, body, nil, time.Hour)
	if _, err := o.Verify(context.Background(), "jo@acmecorp.com", def); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	blob := string(mem.Raw[0].Raw)
	// The standalone "jo" (the address field + the "User jo" body) must be gone...
	if regexp.MustCompile(`\bjo\b`).MatchString(blob) {
		t.Fatalf("DOOR 3 VIOLATION: standalone short local-part 'jo' persisted:\n%s", blob)
	}
	// ...but the transcript must NOT be shredded: the "jo" inside json/rejected stays, code survives.
	if !strings.Contains(blob, "json") || !strings.Contains(blob, "rejected") || !strings.Contains(blob, "550") {
		t.Fatalf("word-boundary redaction must NOT eat substrings (json/rejected) or the code:\n%s", blob)
	}
	if !strings.Contains(blob, "acmecorp.com") {
		t.Fatal("domain (Door-3-clear) must remain")
	}
}

// DOOR 3, case-variant echo (ratification review): a real SMTP server may echo the address in a
// DIFFERENT CASE than the normalized form the brain sent (RFC 5321 permits it). The persisted blob
// must NOT leak the address in any case. normalized is lowercased (Door 3); the server here echoes
// mixed-case. Redaction must be case-insensitive.
func TestSMTPStage_RawBlobRedactsCaseVariantEcho(t *testing.T) {
	body := []byte(`{
	  "wrapper_api_version":"v1","email":"secretlocal@acmecorp.com","validation_type":"smtp","success":false,
	  "smtp_debug":[{"mail_host":"1.2.3.4","port_opened":true,"connection":true,"helo":true,
	    "rcptto":false,"errors":{"rcptto":"550 5.1.1 <SecretLocal@AcmeCorp.com> Recipient rejected\n"}}],
	  "mx":{"domain":"acmecorp.com","mail_servers":["1.2.3.4"]}}`)
	o, mem, def := smtpSetup(t, body, nil, time.Hour)
	if _, err := o.Verify(context.Background(), "secretlocal@acmecorp.com", def); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	blob := strings.ToLower(string(mem.Raw[0].Raw))
	if strings.Contains(blob, "secretlocal") {
		t.Fatalf("DOOR 3 VIOLATION: case-variant address echo persisted (case-insensitive redaction required):\n%s", mem.Raw[0].Raw)
	}
}

// DOOR 3, nested catchall.smtp_debug (ratification review): the target address must be scrubbed
// EVERYWHERE in the blob, including a nested catchall sub-transcript, since redaction is a global
// substitution over the whole blob (not field-aware). (The probe's own nx- address is a synthetic
// instrument, not PII — it is intentionally not redacted; §5.3.)
func TestSMTPStage_RawBlobRedactsNestedCatchallField(t *testing.T) {
	// Contrived: the target address also appears inside catchall.smtp_debug (defense-in-depth —
	// even if some future wrapper field carried it, the global scrub must reach it).
	body := []byte(`{
	  "wrapper_api_version":"v1","email":"secretlocal@acmecorp.com","validation_type":"smtp","success":true,
	  "smtp_debug":null,
	  "catchall":{"probed":true,"probe_local_part":"nx-abc","accepted":false,"conclusion":"not_catchall",
	    "smtp_debug":[{"mail_host":"1.2.3.4","port_opened":true,"connection":true,"helo":true,
	      "rcptto":false,"errors":{"rcptto":"550 secretlocal@acmecorp.com context leak\n"}}]},
	  "mx":{"domain":"acmecorp.com","mail_servers":["1.2.3.4"]}}`)
	o, mem, def := smtpSetup(t, body, nil, time.Hour)
	if _, err := o.Verify(context.Background(), "secretlocal@acmecorp.com", def); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	blob := string(mem.Raw[0].Raw)
	if strings.Contains(blob, "secretlocal") {
		t.Fatalf("DOOR 3 VIOLATION: address leaked via a nested field (global scrub must reach it):\n%s", blob)
	}
	// The synthetic probe instrument is not PII and is intentionally preserved.
	if !strings.Contains(blob, "nx-abc") {
		t.Fatal("the probe instrument (not PII) should be preserved")
	}
}

// DOOR 3, special-char local-parts (ratification review test-gap): local-parts with internal or
// edge non-word chars ("foo.bar", "foo+tag", ".foo", "foo.") must ALL be scrubbed from a bare body
// echo. Internal special chars are fine with \b; leading/trailing ones fall back to an unguarded
// replace (edgeIsNonWord). None may leak.
func TestSMTPStage_RawBlobRedactsSpecialCharLocalParts(t *testing.T) {
	cases := []struct {
		addr string
		body string
		leak string
	}{
		{"foo.bar@acmecorp.com", "550 User foo.bar does not exist", "foo.bar"},
		{"foo+tag@acmecorp.com", "550 User foo+tag rejected", "foo+tag"},
		{"foo-bar@acmecorp.com", "550 foo-bar unknown", "foo-bar"},
		{".foo@acmecorp.com", "550 Mailbox .foo not found", ".foo"},
		{"foo.@acmecorp.com", "550 foo. rejected", "foo."},
	}
	for _, c := range cases {
		t.Run(c.leak, func(t *testing.T) {
			body := []byte(`{"wrapper_api_version":"v1","email":"` + c.addr + `","validation_type":"smtp","success":false,` +
				`"smtp_debug":[{"mail_host":"1.2.3.4","port_opened":true,"connection":true,"helo":true,` +
				`"rcptto":false,"errors":{"rcptto":"` + c.body + `\n"}}],` +
				`"mx":{"domain":"acmecorp.com","mail_servers":["1.2.3.4"]}}`)
			o, mem, def := smtpSetup(t, body, nil, time.Hour)
			if _, err := o.Verify(context.Background(), c.addr, def); err != nil {
				t.Fatalf("Verify: %v", err)
			}
			blob := string(mem.Raw[0].Raw)
			if strings.Contains(blob, c.leak) {
				t.Fatalf("DOOR 3 VIOLATION: local-part %q leaked:\n%s", c.leak, blob)
			}
			if !strings.Contains(blob, "acmecorp.com") {
				t.Fatal("domain (Door-3-clear) must remain")
			}
		})
	}
}

// ── Phase 3: the eligibility gate + skip reasons + PB1 cacheability + verifier_ip ──────────────

// p3Setup builds an orchestrator with an explicit node health + descriptor so a test can drive the
// §4.4 gate to each non-eligible outcome. warmupCap<=0 disables warmup; freshness is disabled.
func p3Setup(t *testing.T, health store.NodeHealth, egressIP string, warmupCap int, body []byte) (*Orchestrator, *store.MemStore, *policy.Definition) {
	t.Helper()
	h, err := hash.NewHasher([]byte("p3-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	mem := store.NewMem(nil)
	mem.SeedNodeHealth(health)
	def, _ := policy.ColdEmailDefinition()
	nodes := []router.Node{{ID: "n1", BaseURL: "http://mock", TokenRef: "n1", Enabled: true, PublicEgressIP: egressIP}}
	o := New(Deps{
		Hasher: h, Store: mem, Resolver: corpResolver(), CacheTTL: time.Hour,
		ConfigFingerprint: "cfg_sha256:test",
		SMTP: &SMTPDeps{
			Client:      fakeNode{body: body},
			Registry:    router.New(nodes, nil),
			TokenFor:    func(string) string { return "tok" },
			Health:      mem,
			MaxAuditAge: 0,
			WarmupCap:   warmupCap,
		},
	})
	return o, mem, def
}

// validBody is a clean-250 + probe-rejected body → valid/accepted when SMTP runs.
var validBody = []byte(`{
  "wrapper_api_version":"v1","email":"jane@acmecorp.com","validation_type":"smtp","success":true,
  "smtp_debug":null,
  "catchall":{"probed":true,"probe_local_part":"nx","accepted":false,"conclusion":"not_catchall"},
  "mx":{"domain":"acmecorp.com","mail_servers":["1.2.3.4"]}}`)

// OQ-P3-6 + PB1: when the only node's audit is RED, the SMTP-eligible residue degrades to the CHEAP
// verdict (risky/plausible) — NEVER invalid — with smtp_skipped_reason=audit_red, and is NOT cached
// (capacity gap → provisional). verifier_ip is null (SMTP never ran).
func TestSMTPStage_RedNode_DegradesToCheapNotInvalid(t *testing.T) {
	o, mem, def := p3Setup(t, store.NodeHealth{
		NodeID: "n1", Status: "active", AuditStatus: "red", Port25Open: true,
		LastAuditAt: time.Now(), LastAuditHostIP: testEgressIP,
	}, testEgressIP, 0, validBody)
	resp, err := o.Verify(context.Background(), "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status == vocab.StatusInvalid {
		t.Fatal("OQ-P3-6 VIOLATION: a red node must NEVER make the address invalid")
	}
	if resp.Status != vocab.StatusRisky || resp.Reason != vocab.ReasonPlausible {
		t.Fatalf("red node → cheap residue risky/plausible, got %s/%s", resp.Status, resp.Reason)
	}
	if s, _ := resp.Signals["smtp_skipped_reason"].(string); s != "audit_red" {
		t.Fatalf("smtp_skipped_reason must be audit_red, got %q", s)
	}
	if mem.CacheLen() != 0 {
		t.Fatalf("PB1: a capacity-gap fallback must NOT be cached, got cache len %d", mem.CacheLen())
	}
	if len(mem.Log) != 1 {
		t.Fatalf("the degraded verdict must still be LOGged (Door 2), got %d", len(mem.Log))
	}
	if resp.Meta.VerifierIP != nil {
		t.Fatalf("verifier_ip must be null when SMTP didn't run, got %v", *resp.Meta.VerifierIP)
	}
	// No node was consulted → no raw sidecar blob.
	if len(mem.Raw) != 0 {
		t.Fatal("SMTP must not have run against a red node (no sidecar)")
	}
}

// PB1: the warmup-cap overflow degrades to the cheap verdict with smtp_skipped_reason=warmup_cap,
// non-cacheable, never queued/invalid (§5.2).
func TestSMTPStage_WarmupCapExceeded_DegradesNonCacheable(t *testing.T) {
	o, _, def := p3Setup(t, store.NodeHealth{
		NodeID: "n1", Status: "active", AuditStatus: "green", Port25Open: true,
		LastAuditAt: time.Now(), LastAuditHostIP: testEgressIP,
	}, testEgressIP, 1, validBody) // cap of 1
	// 1st request consumes the single slot → SMTP runs → valid.
	r1, _ := o.Verify(context.Background(), "a@acmecorp.com", def)
	if r1.Status != vocab.StatusValid {
		t.Fatalf("first request under cap should run SMTP → valid, got %s/%s", r1.Status, r1.Reason)
	}
	// 2nd request is over the cap → degrade to cheap, warmup_cap, non-cacheable.
	r2, err := o.Verify(context.Background(), "b@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r2.Status == vocab.StatusInvalid {
		t.Fatal("over-cap must NEVER be invalid (§5.2)")
	}
	if s, _ := r2.Signals["smtp_skipped_reason"].(string); s != "warmup_cap" {
		t.Fatalf("over-cap smtp_skipped_reason must be warmup_cap, got %q", s)
	}
	if r2.Status != vocab.StatusRisky || r2.Reason != vocab.ReasonPlausible {
		t.Fatalf("over-cap → cheap residue, got %s/%s", r2.Status, r2.Reason)
	}
}

// §5.4: a real SMTP verdict logs a non-null verifier_ip sourced from the brain registry (the node's
// public egress IP == the audited IP, GAP-24), not self-reported by the box.
func TestSMTPStage_VerifierIPLoggedFromRegistry(t *testing.T) {
	o, mem, def := p3Setup(t, store.NodeHealth{
		NodeID: "n1", Status: "active", AuditStatus: "green", Port25Open: true,
		LastAuditAt: time.Now(), LastAuditHostIP: testEgressIP,
	}, testEgressIP, 0, validBody)
	resp, err := o.Verify(context.Background(), "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusValid {
		t.Fatalf("eligible node should run SMTP → valid, got %s/%s", resp.Status, resp.Reason)
	}
	if resp.Meta.VerifierIP == nil || *resp.Meta.VerifierIP != testEgressIP {
		t.Fatalf("verifier_ip must be the registry egress IP %q, got %v", testEgressIP, resp.Meta.VerifierIP)
	}
	if len(mem.Log) != 1 || mem.Log[0].VerifierIP != testEgressIP {
		t.Fatalf("LOG row verifier_ip must be %q, got %q", testEgressIP, mem.Log[0].VerifierIP)
	}
}

// GAP-24 at the orchestrator level: a multi-homed box audited GREEN on its private IP (LastAuditHostIP
// != expected egress) must degrade to cheap — the silent-poisoning trap is blocked end-to-end.
func TestSMTPStage_MultiHomedMismatch_DegradesNotPoisons_GAP24(t *testing.T) {
	o, _, def := p3Setup(t, store.NodeHealth{
		NodeID: "n1", Status: "active", AuditStatus: "green", Port25Open: true,
		LastAuditAt: time.Now(), LastAuditHostIP: "10.0.0.5", // audited the PRIVATE interface
	}, testEgressIP /* expected public egress */, 0, validBody)
	resp, err := o.Verify(context.Background(), "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status == vocab.StatusValid || resp.Status == vocab.StatusInvalid {
		t.Fatal("GAP-24: a box audited on the wrong IP must not produce an SMTP verdict")
	}
	if s, _ := resp.Signals["smtp_skipped_reason"].(string); s != "audit_red" {
		t.Fatalf("GAP-24 mismatch is a health failure → skip reason audit_red, got %q", s)
	}
}

// A definitive cheap gate (no MX) is terminal → the SMTP stage must NOT run (cheapest-sufficient).
func TestSMTPStage_SkippedWhenCheapGateStops(t *testing.T) {
	// nxdomainResolver yields no MX → invalid/no_mx, a terminal gate. Even though a fake node is
	// wired, smtpEligible is false, so the node is never consulted (no raw sidecar written).
	body := []byte(`{"success":true,"smtp_debug":null,"catchall":{"probed":true,"conclusion":"not_catchall"}}`)
	h, _ := hash.NewHasher([]byte("p"))
	mem := store.NewMem(nil)
	def, _ := policy.ColdEmailDefinition()
	nodes := []router.Node{{ID: "n1", BaseURL: "http://mock", Enabled: true}}
	o := New(Deps{
		Hasher: h, Store: mem, Resolver: nxdomainResolver(), CacheTTL: time.Hour,
		ConfigFingerprint: "cfg", SMTP: &SMTPDeps{Client: fakeNode{body: body}, Registry: router.New(nodes, nil), TokenFor: func(string) string { return "" }},
	})
	resp, err := o.Verify(context.Background(), "x@nxdomain.example", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if resp.Status != vocab.StatusInvalid || resp.Reason != vocab.ReasonNoMX {
		t.Fatalf("no-MX is a terminal cheap gate → invalid/no_mx, got %s/%s", resp.Status, resp.Reason)
	}
	if len(mem.Raw) != 0 {
		t.Fatal("SMTP stage must be SKIPPED on a terminal cheap gate (no node call, no sidecar)")
	}
}
