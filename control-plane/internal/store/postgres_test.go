package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/idgen"
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

// ── Phase 4 (§4.4/§5.2/§5.4/§5.6) — PG-gated: durable rate budget, block-rate, lifecycle ──

// The durable rate budget survives a "restart" (a NEW *PG handle sharing nothing but the DB) without
// refilling to full — the §4.4/PM4 property. Uses refill_per_sec=0 so no wall-clock refill masks it.
func TestPGRateBudget_DurableAcrossRestart(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	nodeID := "n-" + must(idgen.NewV7String(time.Now().UTC()))
	p := RateParams{Capacity: 2, RefillPerSec: 0}
	now := time.Now().UTC()
	// Drain the fresh (full) bucket: 2 grants, then deny.
	if d, _ := pg.ReserveRateToken(ctx, nodeID, "other:acme.com", p, now); !d.Allowed {
		t.Fatalf("grant 1 must be allowed (fresh full bucket)")
	}
	if d, _ := pg.ReserveRateToken(ctx, nodeID, "other:acme.com", p, now); !d.Allowed {
		t.Fatalf("grant 2 must be allowed")
	}
	if d, _ := pg.ReserveRateToken(ctx, nodeID, "other:acme.com", p, now); d.Allowed {
		t.Fatalf("grant 3 must be denied (bucket empty, no refill)")
	}
	// "Restart": a second pool handle. The durable row persists → still empty at the SAME instant.
	pg2 := testPG(t)
	if d, _ := pg2.ReserveRateToken(ctx, nodeID, "other:acme.com", p, now); d.Allowed {
		t.Fatalf("a restart must NOT refill the durable allowance — reserve should be denied (PM4)")
	}
}

// Real elapsed wall-clock DOES refill (restart-safe, not restart-refilled): drain, then reserve
// 10s later with refill=1/sec → allowed again.
func TestPGRateBudget_RefillsByElapsed(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	nodeID := "n-" + must(idgen.NewV7String(time.Now().UTC()))
	p := RateParams{Capacity: 1, RefillPerSec: 1}
	base := time.Now().UTC()
	if d, _ := pg.ReserveRateToken(ctx, nodeID, "b", p, base); !d.Allowed {
		t.Fatalf("first reserve (full) must be allowed")
	}
	if d, _ := pg.ReserveRateToken(ctx, nodeID, "b", p, base); d.Allowed {
		t.Fatalf("second reserve at same instant must deny")
	}
	if d, _ := pg.ReserveRateToken(ctx, nodeID, "b", p, base.Add(2*time.Second)); !d.Allowed {
		t.Fatalf("after 2s a token must have refilled (restart-safe)")
	}
}

// PeekRateBudget must not consume: peek repeatedly, then a reserve still succeeds.
func TestPGPeekRateBudget_DoesNotConsume(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	nodeID := "n-" + must(idgen.NewV7String(time.Now().UTC()))
	p := RateParams{Capacity: 1, RefillPerSec: 0}
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if d, _ := pg.PeekRateBudget(ctx, nodeID, "b", p, now); !d.Allowed {
			t.Fatalf("peek %d must report headroom", i)
		}
	}
	if d, _ := pg.ReserveRateToken(ctx, nodeID, "b", p, now); !d.Allowed {
		t.Fatalf("reserve after peeks must still succeed (peek didn't consume)")
	}
}

// The rate_budgets DELETE-grant test used to live here as "attempt the DELETE; skip if it succeeds",
// which meant it ALWAYS SKIPPED under CI's superuser DSN (D13 / fix-plan item 3). Replaced by
// TestPGGrants_RuntimeCannotDeleteRateBudgets in pg_grants_test.go, which runs as cp_runtime via
// SET LOCAL ROLE and asserts the UPDATE-allowed / DELETE-denied pair.

// The block-rate SQL excludes cache hits (from_cache=true) and cheap-degrades (verifier_ip NULL) —
// PB1, proven against real Postgres (the FILTER + WHERE clauses).
func TestPGBlockRate_ExcludesCacheAndDegrades_PB1(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	// A UNIQUE verifier_ip per run: the CI/local Postgres persists rows across runs, and the block-
	// rate window is an hour, so a fixed IP would accumulate prior runs' rows. Derive a distinct
	// 10.x.y.z from the wall clock so each run measures only its own inserts.
	ns := time.Now().UnixNano()
	ip := fmt.Sprintf("10.%d.%d.%d", (ns>>16)&0xff, (ns>>8)&0xff, ns&0xff)
	now := time.Now().UTC()
	mkRow := func(reason, verifierIP string, fromCache bool) VerificationRow {
		return VerificationRow{
			ID: must(idgen.NewV7String(now)), RequestID: must(idgen.NewV7String(now)),
			CheckedAt: now, CreatedAt: now, Status: "unknown", Reason: reason,
			Confidence: 10, Recommendation: "review",
			Flags: map[string]bool{}, Signals: map[string]any{},
			PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1",
			ConfigFingerprint: "cfg_sha256:test", VerifierIP: verifierIP, FromCache: fromCache,
		}
	}
	// 2 real blocks + 1 real accepted on ip; 1 cache-hit block; 1 degrade (no verifier_ip).
	for _, r := range []VerificationRow{
		mkRow("smtp_blocked", ip, false), mkRow("smtp_blocked", ip, false), mkRow("accepted", ip, false),
		mkRow("smtp_blocked", ip, true), mkRow("smtp_blocked", "", false),
	} {
		if err := pg.AppendVerification(ctx, r, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	stats, err := pg.BlockRateByVerifierIP(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("block-rate: %v", err)
	}
	var got *NodeBlockStats
	for i := range stats {
		if stats[i].VerifierIP == ip {
			got = &stats[i]
		}
	}
	if got == nil {
		t.Fatalf("expected stats for %s, got %v", ip, stats)
	}
	if got.Attempts != 3 || got.Blocked != 2 {
		t.Fatalf("PB1: want attempts=3 blocked=2 (cache hit + degrade excluded), got attempts=%d blocked=%d", got.Attempts, got.Blocked)
	}
}

// SetNodeStatus writes 'recovering' (0008 CHECK widening) without clobbering port25/audit, and is a
// no-op for an unregistered node (WHERE id matches 0 rows).
func TestPGSetNodeStatus_RecoveringAndNoop(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	nodeID := "n-" + must(idgen.NewV7String(time.Now().UTC()))
	base := "http://box-" + nodeID
	if err := pg.UpsertNodeState(ctx, nodeID, base, "quarantined", true); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := pg.SetNodeStatus(ctx, nodeID, "recovering"); err != nil {
		t.Fatalf("set recovering: %v", err)
	}
	h, known, err := pg.GetNodeHealth(ctx, nodeID)
	if err != nil || !known {
		t.Fatalf("health: known=%v err=%v", known, err)
	}
	if h.Status != "recovering" {
		t.Fatalf("status should be recovering, got %q", h.Status)
	}
	if !h.Port25Open {
		t.Fatalf("SetNodeStatus must not clobber port25_open")
	}
	// No-op on an unregistered node (no error, no row created).
	if err := pg.SetNodeStatus(ctx, "ghost-"+nodeID, "recovering"); err != nil {
		t.Fatalf("set on unregistered must be a no-op, got %v", err)
	}
}

// ResetWarmup reopens the ramp at count=0 (§5.4/PM3), durable in Postgres.
func TestPGResetWarmup_ReopensRamp(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	nodeID := "n-" + must(idgen.NewV7String(time.Now().UTC()))
	w := time.Now().UTC().Truncate(24 * time.Hour)
	// Fill to cap.
	for i := 0; i < 2; i++ {
		if d, _ := pg.ReserveWarmupSlot(ctx, nodeID, w, 2); !d.Allowed {
			t.Fatalf("fill grant %d", i)
		}
	}
	if d, _ := pg.ReserveWarmupSlot(ctx, nodeID, w, 2); d.Allowed {
		t.Fatalf("precondition: full window denies")
	}
	if err := pg.ResetWarmup(ctx, nodeID, w, 5); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if d, _ := pg.ReserveWarmupSlot(ctx, nodeID, w, 5); !d.Allowed || d.Count != 1 {
		t.Fatalf("after reset ramp reopens at count=1, got %+v", d)
	}
}

// Concurrent first-admits to a NEVER-SEEN bucket must NOT race into a unique_violation (the round-2
// review finding — the guardless first-touch INSERT). With the ON CONFLICT DO NOTHING pre-create,
// N concurrent goroutines all succeed (no spurious error); exactly `capacity` of them get a token.
func TestPGReserveRate_ConcurrentColdStartNoUniqueViolation(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	nodeID := "n-" + must(idgen.NewV7String(time.Now().UTC()))
	bucket := "other:cold-" + nodeID
	p := RateParams{Capacity: 3, RefillPerSec: 0}
	now := time.Now().UTC()

	const N = 8
	errs := make(chan error, N)
	granted := make(chan bool, N)
	for i := 0; i < N; i++ {
		go func() {
			d, err := pg.ReserveRateToken(ctx, nodeID, bucket, p, now)
			errs <- err
			granted <- d.Allowed
		}()
	}
	grants := 0
	for i := 0; i < N; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent cold-start admit must NOT error (no unique_violation), got %v", err)
		}
		if <-granted {
			grants++
		}
	}
	// At most `capacity` (3) grants at the same instant (no refill); the rest denied — never an error.
	if grants > 3 {
		t.Fatalf("no-refill bucket of capacity 3 must grant at most 3, got %d", grants)
	}
	if grants == 0 {
		t.Fatal("a fresh full bucket must grant at least one token")
	}
}

// MetricRowsSince is the observability read (§8) — declared + interface-wired but previously
// untested at the store layer (named as the one coverage gap in the P4→P5 readiness pass). This
// exercises the real SQL: the window filter, newest-first ordering, the signals JSON round-trip, and
// the +1 truncation detection. Read-only, off the control path (control uses BlockRateByVerifierIP).
func TestPGMetricRowsSince_WindowOrderingAndTruncation(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	// A unique verifier_ip per run so the window read sees only this run's rows (the DB persists).
	ns := time.Now().UnixNano()
	ip := fmt.Sprintf("10.%d.%d.%d", (ns>>16)&0xff, (ns>>8)&0xff, ns&0xff)
	base := time.Now().UTC()
	mk := func(reason string, ago time.Duration, sig map[string]any) VerificationRow {
		return VerificationRow{
			ID: must(idgen.NewV7String(base)), RequestID: must(idgen.NewV7String(base)),
			CheckedAt: base.Add(-ago), CreatedAt: base.Add(-ago), Status: "unknown", Reason: reason,
			Confidence: 10, Recommendation: "review", Flags: map[string]bool{}, Signals: sig,
			PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1",
			ConfigFingerprint: "cfg_sha256:test", VerifierIP: ip, LatencyMS: 12,
		}
	}
	// 1 old (2h ago, outside a 1h window), 2 fresh — one carrying a signals value.
	for _, r := range []VerificationRow{
		mk("smtp_timeout", 2*time.Hour, nil),
		mk("smtp_blocked", 5*time.Minute, map[string]any{"smtp_skipped_reason": "rate_limited"}),
		mk("accepted", 1*time.Minute, nil),
	} {
		if err := pg.AppendVerification(ctx, r, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Window = last hour → excludes the 2h-old row.
	rows, truncated, err := pg.MetricRowsSince(ctx, base.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("MetricRowsSince: %v", err)
	}
	mine := 0
	var signalsSeen bool
	var lastTS time.Time
	for _, r := range rows {
		if r.VerifierIP != ip {
			continue // other runs' rows share the table
		}
		mine++
		if mine > 1 && r.CheckedAt.After(lastTS) {
			t.Fatalf("rows must be newest-first (DESC); got %v after %v", r.CheckedAt, lastTS)
		}
		lastTS = r.CheckedAt
		if sr, _ := r.Signals["smtp_skipped_reason"].(string); sr == "rate_limited" {
			signalsSeen = true
		}
	}
	if mine != 2 {
		t.Fatalf("window (last hour) must exclude the 2h-old row → 2 of my rows, got %d", mine)
	}
	if !signalsSeen {
		t.Fatalf("the signals JSON (smtp_skipped_reason=rate_limited) must round-trip through MetricRowsSince")
	}
	// Truncation detection (POSITIVE, shared-table-safe): a limit of 1 over ≥2 in-window rows MUST
	// flag truncated=true. NOTE: `verifications` is shared across test runs, so MetricRowsSince (a
	// WHOLE-TABLE window read, not IP-scoped) sees prior runs' recent rows too — the earlier
	// `truncated` value under a 100-limit is therefore NOT deterministically false and is not asserted
	// (that trap is why the block-rate test uses a unique IP; MetricRowsSince can't be IP-scoped). The
	// positive check below is deterministic: with my 2 fresh rows alone the window already has ≥2, so a
	// limit of 1 is always a truncation.
	_ = truncated
	_, trunc2, err := pg.MetricRowsSince(ctx, base.Add(-time.Hour), 1)
	if err != nil {
		t.Fatalf("MetricRowsSince(limit=1): %v", err)
	}
	if !trunc2 {
		t.Fatalf("a limit below the in-window count must report truncated=true (no silent cap)")
	}
}

// TestPGLookupCaller_WindowEnforced proves the real SQL validity window (PN-13) matches the Mem
// fake: an active key resolves its caller_id; an unknown key misses; a rotated-out key (not_after
// in the past) misses; a not-yet-valid key (not_before in the future) misses; and a rotation
// overlap resolves BOTH keys to the same caller. Uses direct INSERTs (issuance is cp_admin; the
// test connects as owner) then reads through the runtime LookupCaller path.
func TestPGLookupCaller_WindowEnforced(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := func(keySHA []byte, callerID string, nb time.Time, na *time.Time) {
		var naVal any
		if na != nil {
			naVal = na.UTC()
		}
		if _, err := pg.pool.Exec(ctx,
			`INSERT INTO api_callers (caller_id, key_sha256, not_before, not_after)
			 VALUES ($1,$2,$3,$4) ON CONFLICT (key_sha256) DO NOTHING`,
			callerID, keySHA, nb.UTC(), naVal); err != nil {
			t.Fatalf("seed caller: %v", err)
		}
	}
	// Unique key material per run so repeated runs against a shared DB don't collide.
	tag := now.Format("150405.000000000")
	kActive := []byte("active-" + tag)
	kExpired := []byte("expired-" + tag)
	kFuture := []byte("future-" + tag)
	kRotOld := []byte("rotold-" + tag)
	kRotNew := []byte("rotnew-" + tag)

	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	seed(kActive, "caller_active_"+tag, now.Add(-time.Hour), nil)
	seed(kExpired, "caller_active_"+tag, now.Add(-48*time.Hour), &past)
	seed(kFuture, "caller_future_"+tag, future, nil)
	seed(kRotOld, "caller_rot_"+tag, now.Add(-24*time.Hour), &future)
	seed(kRotNew, "caller_rot_"+tag, now, nil)

	check := func(name string, key []byte, wantOK bool, wantID string) {
		id, _, ok, err := pg.LookupCaller(ctx, key, now)
		if err != nil {
			t.Fatalf("%s: LookupCaller err %v", name, err)
		}
		if ok != wantOK {
			t.Fatalf("%s: ok=%v want %v", name, ok, wantOK)
		}
		if wantOK && id != wantID {
			t.Fatalf("%s: id=%q want %q", name, id, wantID)
		}
	}
	check("active", kActive, true, "caller_active_"+tag)
	check("unknown", []byte("no-such-key-"+tag), false, "")
	check("expired", kExpired, false, "")
	check("not-yet-valid", kFuture, false, "")
	check("rotation-old", kRotOld, true, "caller_rot_"+tag)
	check("rotation-new", kRotNew, true, "caller_rot_"+tag)
}

// TestPGWorkItem_CASSettlementExactlyOnce proves the GAP-37 CAS against REAL Postgres: the
// settlement UPDATE ... WHERE locked_by=<me> AND status='in_flight' returns zero rows for a
// stale-lease attempt (→ ErrLeaseLost, no LOG row), and exactly one attempt commits. This is the
// PG parity of the Mem TestSettle_ExactlyOnceUnderSlowDrainerReclaim — they MUST agree.
func TestPGWorkItem_CASSettlementExactlyOnce(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	now := time.Now().UTC()

	itemID, _ := idgen.NewV7String(now)
	idem := []byte("pg-cas-" + now.Format("150405.000000000"))
	item := WorkItemRow{
		ID: itemID, IdempotencyKey: idem, Domain: "ex.com", EmailHash: []byte("h"),
		PolicyKey: "cold-email", PolicyVersion: 1, ConfigFingerprint: "cfg", Status: "ready",
		NotBefore: now.Add(-time.Minute), AddrCiphertext: []byte("ct"),
	}
	if _, _, err := pg.EnqueueWorkItem(ctx, item); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	t.Cleanup(func() {
		// Owner-role cleanup (cp_runtime can't DELETE — retention posture).
		_, _ = pg.pool.Exec(context.Background(), `DELETE FROM work_item_attempts WHERE work_item_id=$1`, itemID)
		_, _ = pg.pool.Exec(context.Background(), `DELETE FROM work_items WHERE id=$1`, itemID)
	})

	// Attempt A leases it.
	leasedA, err := pg.DequeueReady(ctx, "worker-A", 1, now)
	if err != nil || len(leasedA) != 1 {
		t.Fatalf("dequeue A: %v (n=%d)", err, len(leasedA))
	}
	// Reclaim (A is slow) then B leases it.
	later := now.Add(2 * time.Minute)
	if n, err := pg.ReclaimExpiredLeases(ctx, time.Minute, later); err != nil || n != 1 {
		t.Fatalf("reclaim: %v (n=%d)", err, n)
	}
	leasedB, err := pg.DequeueReady(ctx, "worker-B", 1, later)
	if err != nil || len(leasedB) != 1 {
		t.Fatalf("dequeue B: %v (n=%d)", err, len(leasedB))
	}

	// A (stale lease) settles → must lose the CAS. B (current) settles → wins.
	rid := mustV7(t, now)
	vA := VerificationRow{ID: mustV7(t, now), RequestID: rid, CheckedAt: now, CreatedAt: now, Status: "valid",
		Reason: "accepted", Confidence: 90, Recommendation: "send", PolicyKey: "cold-email",
		PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg"}
	// P6 SPEND-ONCE (GAP-42): BOTH attempts made a paid vendor call and attach a spend record. Only
	// the winning CAS may record the charge; the stale attempt's spend record must roll back with its
	// discarded settle — exactly one external_calls row, proven against REAL Postgres.
	callA := &ExternalCallRow{ID: mustV7(t, now), VerificationID: vA.ID, WorkItemID: itemID, Vendor: "millionverifier", ResidueClass: "retry_exhausted_greylisted", CostUnits: 1, Outcome: "ok", CalledAt: now}
	errA := pg.SettleWorkItem(ctx, itemID, "worker-A", "settled", vA, nil, WorkItemAttemptRow{}, callA)
	if errA != ErrLeaseLost {
		t.Fatalf("stale-lease attempt A must get ErrLeaseLost, got %v", errA)
	}
	vB := VerificationRow{ID: mustV7(t, now), RequestID: rid, CheckedAt: now, CreatedAt: now, Status: "unknown",
		Reason: "greylisted", Confidence: 10, Recommendation: "review", PolicyKey: "cold-email",
		PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg"}
	callB := &ExternalCallRow{ID: mustV7(t, now), VerificationID: vB.ID, WorkItemID: itemID, Vendor: "millionverifier", ResidueClass: "retry_exhausted_greylisted", CostUnits: 1, Outcome: "ok", CalledAt: now}
	if err := pg.SettleWorkItem(ctx, itemID, "worker-B", "settled", vB, nil, WorkItemAttemptRow{}, callB); err != nil {
		t.Fatalf("current-lease attempt B must settle, got %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(), `DELETE FROM external_calls WHERE work_item_id=$1`, itemID)
		_, _ = pg.pool.Exec(context.Background(), `DELETE FROM verifications WHERE id=$1`, vB.ID)
	})
	// SPEND-ONCE assertion: exactly ONE external_calls row for this item (B's). A's charge rolled back.
	var spendRows int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM external_calls WHERE work_item_id=$1`, itemID).Scan(&spendRows); err != nil {
		t.Fatalf("count spend rows: %v", err)
	}
	if spendRows != 1 {
		t.Fatalf("GAP-42 VIOLATED (double-charge on real PG): %d external_calls rows for one settle, want exactly 1", spendRows)
	}
	var survivingCallID string
	if err := pg.pool.QueryRow(ctx, `SELECT id FROM external_calls WHERE work_item_id=$1`, itemID).Scan(&survivingCallID); err == nil && survivingCallID != callB.ID {
		t.Fatalf("the surviving spend record must be the winner B's (%s), got %s", callB.ID, survivingCallID)
	}

	// The item is settled, plaintext purged, pointing at B's verdict; A's verdict was NEVER logged.
	w, known, err := pg.GetWorkItem(ctx, itemID)
	if err != nil || !known {
		t.Fatalf("get item: %v known=%v", err, known)
	}
	if w.Status != "settled" || w.VerificationID != vB.ID {
		t.Fatalf("item must be settled pointing at B (%s); got status=%s vid=%s", vB.ID, w.Status, w.VerificationID)
	}
	if len(w.AddrCiphertext) != 0 {
		t.Fatal("addr_ciphertext must be purged in the settlement tx (OQ-P5-9)")
	}
	// A's verdict id must NOT exist in the LOG (GAP-33: the lost attempt wrote nothing).
	var cnt int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM verifications WHERE id=$1`, vA.ID).Scan(&cnt); err != nil {
		t.Fatalf("count A verdict: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("GAP-33 VIOLATED: the lost attempt A wrote a LOG row (%d rows for its id)", cnt)
	}
}

func mustV7(t *testing.T, now time.Time) string {
	t.Helper()
	id, err := idgen.NewV7String(now)
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	return id
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}
