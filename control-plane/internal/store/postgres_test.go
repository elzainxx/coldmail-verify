package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/idgen"
)

// These tests run against a REAL Postgres with the P1 migrations applied. They are gated on
// TM_TEST_DATABASE_URL and SKIP honestly when it is unset (test-strategy §4 — never fake
// green; a green run without the DSN means "not exercised", not "passed"). The migrations
// must be applied to the target DB first (see control-plane/migrations/README.md).
//
// Run locally, e.g.:
//   docker run -d --name cp-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=cptest -p 55432:5432 postgres:16-alpine
//   for f in migrations/0001*.sql migrations/0002*.sql migrations/0003*.sql migrations/0004*.sql; do \
//     psql "postgres://postgres:test@localhost:55432/cptest" -f "$f"; done
//   TM_TEST_DATABASE_URL="postgres://postgres:test@localhost:55432/cptest" go test ./internal/store/ -run PG -v

func testPG(t *testing.T) *PG {
	t.Helper()
	dsn := os.Getenv("TM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PENDING: set TM_TEST_DATABASE_URL to a migrated Postgres to exercise the real store (never faked)")
	}
	pg, err := OpenPG(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

func TestPGAppendAndCacheRoundTrip(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, err := idgen.NewV7String(now)
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	reqID, _ := idgen.NewV7String(now)

	row := VerificationRow{
		ID: id, RequestID: reqID, CheckedAt: now, CreatedAt: now,
		EmailHash: []byte{0xde, 0xad, 0xbe, 0xef}, HashScheme: "norm/v1+hmac-sha256/v1",
		Domain: "example.com", MXProvider: "other",
		Status: "risky", Reason: "plausible", Confidence: 30, Recommendation: "review",
		Flags: map[string]bool{"from_cache": false}, Signals: map[string]any{"syntax_valid": true},
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1",
		ConfigFingerprint: "cfg_sha256:test", FromCache: false, LatencyMS: 12,
	}
	if err := pg.AppendVerification(ctx, row, nil); err != nil {
		t.Fatalf("AppendVerification: %v", err)
	}

	// Cache round-trip on the composite key.
	cv := CachedVerdict{
		EmailHash: row.EmailHash, ConfigFingerprint: "cfg_sha256:test", Domain: "example.com",
		MXProvider: "other", HashScheme: row.HashScheme,
		Status: "risky", Reason: "plausible", Confidence: 30, Recommendation: "review",
		Flags: map[string]bool{"from_cache": false}, Signals: map[string]any{"syntax_valid": true},
		CachedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := pg.PutCachedVerdict(ctx, cv); err != nil {
		t.Fatalf("PutCachedVerdict: %v", err)
	}
	got, err := pg.GetCachedVerdict(ctx, row.EmailHash, "cfg_sha256:test")
	if err != nil {
		t.Fatalf("GetCachedVerdict: %v", err)
	}
	if got.Status != "risky" || got.Confidence != 30 || got.Domain != "example.com" {
		t.Errorf("cache round-trip mismatch: %+v", got)
	}
	// Wrong config => miss (composite-key correctness against real PG).
	if _, err := pg.GetCachedVerdict(ctx, row.EmailHash, "different-cfg"); err != ErrCacheMiss {
		t.Errorf("wrong config must miss, got %v", err)
	}
}

// A NULL-hash (invalid_syntax) row must insert (§3.2.7) — email_hash + hash_scheme + domain
// all NULL, which the DB CHECK (email_hash IS NULL)=(hash_scheme IS NULL) must accept.
func TestPGNullHashRowInserts(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, _ := idgen.NewV7String(now)
	reqID, _ := idgen.NewV7String(now)
	row := VerificationRow{
		ID: id, RequestID: reqID, CheckedAt: now, CreatedAt: now,
		// EmailHash/HashScheme/Domain deliberately zero => NULL.
		Status: "invalid", Reason: "invalid_syntax", Confidence: 99, Recommendation: "skip",
		Flags: map[string]bool{}, Signals: map[string]any{},
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1",
		ConfigFingerprint: "cfg_sha256:test", FromCache: false,
	}
	if err := pg.AppendVerification(ctx, row, nil); err != nil {
		t.Fatalf("un-addressable reject row must insert with NULL hash (§3.2.7): %v", err)
	}
}

// P2: the SMTP path writes the raw engine blob to the verification_raw sidecar (§6.4), atomically
// with the LOG row. Proves the sidecar insert works against the real partitioned table.
func TestPGAppendWithRawSidecar(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, _ := idgen.NewV7String(now)
	reqID, _ := idgen.NewV7String(now)
	row := VerificationRow{
		ID: id, RequestID: reqID, CheckedAt: now, CreatedAt: now,
		EmailHash: []byte{0x01, 0x02, 0x03, 0x04}, HashScheme: "norm/v1+hmac-sha256/v1",
		Domain: "corp.test", Status: "valid", Reason: "accepted", Confidence: 90, Recommendation: "send",
		Flags: map[string]bool{"from_cache": false}, Signals: map[string]any{"smtp_code": "250"},
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg_sha256:test",
		NodeID: "n1", EngineVersion: "rack@0.9.0+gem@3.3.1", EngineValidationType: "smtp",
		EngineSuccessSet: true, EngineSuccess: true,
	}
	raw := &RawRow{VerificationID: id, Raw: []byte(`{"wrapper_api_version":"v1","success":true}`)}
	if err := pg.AppendVerification(ctx, row, raw); err != nil {
		t.Fatalf("SMTP verdict + raw sidecar must insert atomically (§6.4): %v", err)
	}
}

// P2 §5.6: the brain persists the catch-all conclusion (migration 0006 grants cp_runtime the
// write). A definitive true/false upserts; an inconclusive (nil) is a NO-OP that never overwrites.
func TestPGSetCatchAllTriState(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	dom := "catchall-" + must(idgen.NewV7String(time.Now().UTC())) + ".test" // unique per run
	tru := true

	// First probe → catchall true.
	if err := pg.SetCatchAll(ctx, dom, &tru, "probe", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetCatchAll(true): %v", err)
	}
	is, known, err := pg.CatchAllState(ctx, dom)
	if err != nil || !known || !is {
		t.Fatalf("after true probe want known=true is=true, got known=%v is=%v err=%v", known, is, err)
	}

	// Inconclusive (nil) must NOT overwrite the known value (§5.6).
	if err := pg.SetCatchAll(ctx, dom, nil, "probe", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetCatchAll(nil) should be a no-op, got: %v", err)
	}
	is, known, _ = pg.CatchAllState(ctx, dom)
	if !known || !is {
		t.Fatal("inconclusive must NOT overwrite a known catch-all value (§5.6)")
	}

	// A definitive false re-probe updates it.
	fal := false
	if err := pg.SetCatchAll(ctx, dom, &fal, "probe", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetCatchAll(false): %v", err)
	}
	is, known, _ = pg.CatchAllState(ctx, dom)
	if !known || is {
		t.Fatalf("after false re-probe want known=true is=false, got known=%v is=%v", known, is)
	}
}

// ── Phase 3 (§4.3/§4.4/§5.2) — node health, append-only audit history, durable warmup ──────
// These require migration 0007 (node_audits, node_warmup, nodes UPDATE grant) applied.

func TestPGNodeStateAndAudit(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	nodeID := "n-" + must(idgen.NewV7String(time.Now().UTC()))
	url := "http://" + nodeID + ".test"

	// Register provisioning (mirrors the boot upsert).
	if err := pg.UpsertNodeState(ctx, nodeID, url, "provisioning", false); err != nil {
		t.Fatalf("UpsertNodeState: %v", err)
	}
	h, known, err := pg.GetNodeHealth(ctx, nodeID)
	if err != nil || !known {
		t.Fatalf("health must be known after register, got known=%v err=%v", known, err)
	}
	if h.Status != "provisioning" || h.Port25Open {
		t.Fatalf("want provisioning/port25=false, got %+v", h)
	}

	// Append a green audit — the summary (audit_status/last_audit_at/current_host_ip) must update.
	now := time.Now().UTC().Truncate(time.Second)
	auditID := must(idgen.NewV7String(now))
	if err := pg.AppendNodeAudit(ctx, NodeAuditRow{
		ID: auditID, NodeID: nodeID, AuditedAt: now, AuditStatus: "green",
		Warnings: map[string]any{}, CurrentHostIP: "203.0.113.10",
	}); err != nil {
		t.Fatalf("AppendNodeAudit: %v", err)
	}
	h, _, _ = pg.GetNodeHealth(ctx, nodeID)
	if h.AuditStatus != "green" || h.LastAuditHostIP != "203.0.113.10" {
		t.Fatalf("audit summary must reflect the green audit + host IP, got %+v", h)
	}

	// Promote to active via UpsertNodeState with port25 proven — audit columns must SURVIVE.
	if err := pg.UpsertNodeState(ctx, nodeID, url, "active", true); err != nil {
		t.Fatalf("promote: %v", err)
	}
	h, _, _ = pg.GetNodeHealth(ctx, nodeID)
	if h.Status != "active" || !h.Port25Open || h.AuditStatus != "green" {
		t.Fatalf("promotion must keep the audit summary, got %+v", h)
	}
}

// Door-2-style append-only: node_audits DELETE is REVOKED from cp_runtime (migration 0007). This
// requires connecting AS cp_runtime; if the test DSN is a superuser the REVOKE won't bite, so this
// test only asserts the migration ran (the grant is proven in the migration itself + CI). Here we
// prove the HISTORY accumulates (multiple rows for one node), which is the append-only behavior.
func TestPGNodeAuditHistoryAccumulates(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	nodeID := "n-" + must(idgen.NewV7String(time.Now().UTC()))
	if err := pg.UpsertNodeState(ctx, nodeID, "http://"+nodeID+".test", "provisioning", true); err != nil {
		t.Fatalf("register: %v", err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	for i, st := range []string{"green", "red", "green"} {
		w := map[string]any{}
		if st == "red" {
			w = map[string]any{"ptr": "PTR mismatch"}
		}
		if err := pg.AppendNodeAudit(ctx, NodeAuditRow{
			ID: must(idgen.NewV7String(base.Add(time.Duration(i) * time.Second))), NodeID: nodeID,
			AuditedAt: base.Add(time.Duration(i) * time.Second), AuditStatus: st, Warnings: w, CurrentHostIP: "203.0.113.10",
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// The latest-audit summary must be the LAST row (green), proving the join reads latest-per-node.
	h, _, _ := pg.GetNodeHealth(ctx, nodeID)
	if h.AuditStatus != "green" {
		t.Fatalf("summary must reflect the LATEST audit (green), got %q", h.AuditStatus)
	}
}

func TestPGWarmupReserveAtomicCap(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	nodeID := "n-" + must(idgen.NewV7String(time.Now().UTC()))
	window := time.Now().UTC().Truncate(24 * time.Hour)

	// Cap of 2 → first two reservations granted, third denied.
	d1, err := pg.ReserveWarmupSlot(ctx, nodeID, window, 2)
	if err != nil || !d1.Allowed || d1.Count != 1 {
		t.Fatalf("reservation 1 want allowed count=1, got %+v err=%v", d1, err)
	}
	d2, _ := pg.ReserveWarmupSlot(ctx, nodeID, window, 2)
	if !d2.Allowed || d2.Count != 2 {
		t.Fatalf("reservation 2 want allowed count=2, got %+v", d2)
	}
	d3, _ := pg.ReserveWarmupSlot(ctx, nodeID, window, 2)
	if d3.Allowed {
		t.Fatalf("reservation 3 must be DENIED (over cap), got %+v", d3)
	}
	if d3.Count != 2 {
		t.Fatalf("denied decision must report the current count 2, got %d", d3.Count)
	}

	// A NEW window (next day) resets the allowance — the ramp is per-window.
	d4, _ := pg.ReserveWarmupSlot(ctx, nodeID, window.Add(24*time.Hour), 2)
	if !d4.Allowed || d4.Count != 1 {
		t.Fatalf("a new window must reset the allowance, got %+v", d4)
	}

	// Cap of 0 → never allowed, no row created.
	d0, _ := pg.ReserveWarmupSlot(ctx, "n-zero-cap", window, 0)
	if d0.Allowed {
		t.Fatal("cap<=0 must never grant a slot")
	}
}

// PG↔Mem parity (the P3 review divergence findings, now fixed both sides). These assert PG's exact
// behavior so the Mem fake's mirroring is anchored to reality, not to itself.

// An audit for an UNREGISTERED node updates history but NOT the summary → GetNodeHealth known=false
// (PG's `UPDATE nodes WHERE id=$1` matches 0 rows). Mem now mirrors this (TestMemAppendNodeAudit_…).
func TestPGAppendNodeAudit_UnregisteredNodeNotKnown(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	nodeID := "ghost-" + must(idgen.NewV7String(time.Now().UTC()))
	now := time.Now().UTC().Truncate(time.Second)
	if err := pg.AppendNodeAudit(ctx, NodeAuditRow{
		ID: must(idgen.NewV7String(now)), NodeID: nodeID, AuditedAt: now, AuditStatus: "green",
		Warnings: map[string]any{}, CurrentHostIP: "203.0.113.10",
	}); err != nil {
		t.Fatalf("append (unregistered): %v", err)
	}
	_, known, err := pg.GetNodeHealth(ctx, nodeID)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if known {
		t.Fatal("PG: an audit for an unregistered node must leave GetNodeHealth known=false (no nodes row)")
	}
}

// The warmup deny-path returns the STORED cap unchanged when the caller passes a lower cap (PG's
// ON CONFLICT SET, incl. cap, does not apply on deny). Mem now mirrors this.
func TestPGReserveWarmup_DenyReportsStoredCap(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	nodeID := "n-" + must(idgen.NewV7String(time.Now().UTC()))
	w := time.Now().UTC().Truncate(24 * time.Hour)
	if d, _ := pg.ReserveWarmupSlot(ctx, nodeID, w, 2); !d.Allowed || d.Count != 1 {
		t.Fatalf("res1 %+v", d)
	}
	if d, _ := pg.ReserveWarmupSlot(ctx, nodeID, w, 2); !d.Allowed || d.Count != 2 {
		t.Fatalf("res2 %+v", d)
	}
	d, _ := pg.ReserveWarmupSlot(ctx, nodeID, w, 1) // lower cap → deny
	if d.Allowed {
		t.Fatalf("over stored cap must deny, got %+v", d)
	}
	if d.Cap != 2 {
		t.Fatalf("PG deny must report the STORED cap 2, got %d", d.Cap)
	}
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}
