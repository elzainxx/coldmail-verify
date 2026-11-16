package store

// TestRuntimeAsVerifyApp drives the REAL store over a REAL login as verify_app (not a superuser with SET ROLE) against a scratch database migrated
// through 0016. Everything the control plane's main.go wires and that is not dead (research file m1-public-and-privileges.md section B.1) is
// called once, and each call's effect is read back through a SEPARATE superuser connection, never through the store's own pool. No test DSN
// carries a search_path or a role: the store sees only the default search_path, in which public holds no table of module 1, so one
// unqualified statement anywhere would fail with 42P01 here.
//
// It runs whenever TM_TEST_DATABASE_URL is set and derives everything from it: the verify_app DSN is that DSN with the user replaced and a
// constant test password set on the (cluster-wide) role for the length of the test. No new environment variable.

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-verify/control-plane/internal/testpg"
)

// verifyAppTestPassword is a throwaway low-entropy secret for a role in a throwaway database cluster; it is set for the duration of one test and
// cleared again, and the migrations never contain a password.
const verifyAppTestPassword = "verify_app_test_pw"

// openAsVerifyApp sets verify_app's password, then opens the real store with a DSN that logs in as verify_app on the scratch database suDSN.
func openAsVerifyApp(t *testing.T, suDSN string) *PG {
	t.Helper()
	testpg.SetRolePassword(t, "verify_app", verifyAppTestPassword)
	u, err := url.Parse(suDSN)
	if err != nil {
		t.Fatalf("parse scratch dsn: %v", err)
	}
	u.User = url.UserPassword("verify_app", verifyAppTestPassword)
	pg, err := OpenPG(context.Background(), u.String())
	if err != nil {
		t.Fatalf("OpenPG as verify_app: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

func suOne[T any](t *testing.T, su *pgx.Conn, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := su.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("superuser read-back %q: %v", sql, err)
	}
	return v
}

func suExec(t *testing.T, su *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := su.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("superuser exec %q: %v", sql, err)
	}
}

func rtRow(t *testing.T, at time.Time, domain string, hash []byte) VerificationRow {
	t.Helper()
	return VerificationRow{
		ID: mustV7(t, at), RequestID: mustV7(t, at), CheckedAt: at, CreatedAt: at,
		EmailHash: hash, HashScheme: "norm/v1+hmac-sha256/v1", Domain: domain, MXProvider: "other",
		Status: "valid", Reason: "accepted", Confidence: 90, Recommendation: "send",
		Flags: map[string]bool{"from_cache": false}, Signals: map[string]any{"smtp_code": "250"},
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg_sha256:rt",
		NodeID: "n-rt", EngineVersion: "rack@0.9.0+gem@3.3.1", EngineValidationType: "smtp",
		EngineSuccessSet: true, EngineSuccess: true, VerifierIP: "203.0.113.7", LatencyMS: 12,
	}
}

func TestRuntimeAsVerifyApp(t *testing.T) {
	ctx := context.Background()
	suDSN := testpg.Scratch(t, 0)
	su := superConn(t, suDSN)
	pg := openAsVerifyApp(t, suDSN)
	now := time.Now().UTC().Truncate(time.Millisecond)

	t.Run("login_identity_and_ping", func(t *testing.T) {
		if err := pg.Ping(ctx); err != nil {
			t.Fatalf("Ping: %v", err)
		}
		var session, current string
		var super bool
		if err := pg.Pool().QueryRow(ctx,
			`SELECT session_user, current_user, (SELECT rolsuper FROM pg_roles WHERE rolname = current_user)`).Scan(&session, &current, &super); err != nil {
			t.Fatal(err)
		}
		if session != "verify_app" || current != "verify_app" || super {
			t.Fatalf("the store must be connected as the non-superuser login verify_app, got session=%q current=%q super=%v", session, current, super)
		}
		var sp string
		if err := pg.Pool().QueryRow(ctx, `SHOW search_path`).Scan(&sp); err != nil {
			t.Fatal(err)
		}
		if sp != `"$user", public` {
			t.Fatalf("this test must run with the default search_path, got %q", sp)
		}
	})

	t.Run("EnsurePolicy_the_startup_path", func(t *testing.T) {
		def := []byte(`{"key":"rt-test","version":1}`)
		if err := pg.EnsurePolicy(ctx, "rt-test", 1, def, true); err != nil {
			t.Fatalf("EnsurePolicy (INSERT + UPDATE is_default as verify_app): %v", err)
		}
		if got := suOne[bool](t, su, `SELECT is_default FROM verify.policies WHERE key='rt-test' AND version=1`); !got {
			t.Fatal("policy v1 must be the default after EnsurePolicy(asDefault=true) on a database without one")
		}
		if got := suOne[string](t, su, `SELECT definition->>'key' FROM verify.policies WHERE key='rt-test' AND version=1`); got != "rt-test" {
			t.Fatalf("stored definition key = %q", got)
		}
		d, err := pg.GetDefaultPolicy(ctx)
		if err != nil || d.Key != "rt-test" || d.Version != 1 || !d.IsDefault {
			t.Fatalf("GetDefaultPolicy = %+v err=%v", d, err)
		}
		// Idempotent, and a second version does NOT take the default away (the NOT EXISTS guard).
		if err := pg.EnsurePolicy(ctx, "rt-test", 1, def, true); err != nil {
			t.Fatalf("EnsurePolicy re-run: %v", err)
		}
		if err := pg.EnsurePolicy(ctx, "rt-test", 2, []byte(`{"key":"rt-test","version":2}`), true); err != nil {
			t.Fatalf("EnsurePolicy v2: %v", err)
		}
		if got := suOne[bool](t, su, `SELECT is_default FROM verify.policies WHERE key='rt-test' AND version=2`); got {
			t.Fatal("v2 must not become default while v1 is")
		}
		if got := suOne[int](t, su, `SELECT count(*) FROM verify.policies WHERE key='rt-test'`); got != 2 {
			t.Fatalf("want 2 policy rows, got %d", got)
		}
	})

	nodeID := "n-" + mustV7(t, now)
	t.Run("node_state_audit_status", func(t *testing.T) {
		if err := pg.UpsertNodeState(ctx, nodeID, "http://"+nodeID+".test", "provisioning", false); err != nil {
			t.Fatalf("UpsertNodeState: %v", err)
		}
		if got := suOne[string](t, su, `SELECT status FROM verify.nodes WHERE id=$1`, nodeID); got != "provisioning" {
			t.Fatalf("nodes.status = %q", got)
		}
		if err := pg.UpsertNodeState(ctx, nodeID, "http://"+nodeID+".test", "active", true); err != nil {
			t.Fatalf("UpsertNodeState (update path): %v", err)
		}
		if st, p25 := suOne[string](t, su, `SELECT status FROM verify.nodes WHERE id=$1`, nodeID), suOne[bool](t, su, `SELECT port25_open FROM verify.nodes WHERE id=$1`, nodeID); st != "active" || !p25 {
			t.Fatalf("after the upsert update: status=%q port25=%v", st, p25)
		}
		auditAt := now.Truncate(time.Second)
		if err := pg.AppendNodeAudit(ctx, NodeAuditRow{ID: mustV7(t, auditAt), NodeID: nodeID, AuditedAt: auditAt, AuditStatus: "green",
			Warnings: map[string]any{}, CurrentHostIP: "203.0.113.10"}); err != nil {
			t.Fatalf("AppendNodeAudit (INSERT node_audits + UPDATE nodes): %v", err)
		}
		if got := suOne[int](t, su, `SELECT count(*) FROM verify.node_audits WHERE node_id=$1`, nodeID); got != 1 {
			t.Fatalf("node_audits rows = %d", got)
		}
		if got := suOne[string](t, su, `SELECT audit_status FROM verify.nodes WHERE id=$1`, nodeID); got != "green" {
			t.Fatalf("nodes.audit_status = %q", got)
		}
		h, known, err := pg.GetNodeHealth(ctx, nodeID)
		if err != nil || !known || h.AuditStatus != "green" || h.LastAuditHostIP != "203.0.113.10" || h.Status != "active" {
			t.Fatalf("GetNodeHealth = %+v known=%v err=%v", h, known, err)
		}
		if err := pg.SetNodeStatus(ctx, nodeID, "quarantined"); err != nil {
			t.Fatalf("SetNodeStatus: %v", err)
		}
		if got := suOne[string](t, su, `SELECT status FROM verify.nodes WHERE id=$1`, nodeID); got != "quarantined" {
			t.Fatalf("nodes.status after SetNodeStatus = %q", got)
		}
	})

	t.Run("warmup_reserve_and_reset", func(t *testing.T) {
		w := now.Truncate(24 * time.Hour)
		for i := 1; i <= 2; i++ {
			if d, err := pg.ReserveWarmupSlot(ctx, nodeID, w, 2); err != nil || !d.Allowed || d.Count != i {
				t.Fatalf("ReserveWarmupSlot #%d = %+v err=%v", i, d, err)
			}
		}
		if d, err := pg.ReserveWarmupSlot(ctx, nodeID, w, 2); err != nil || d.Allowed {
			t.Fatalf("third reservation must be denied, got %+v err=%v", d, err)
		}
		if got := suOne[int](t, su, `SELECT count FROM verify.node_warmup WHERE node_id=$1 AND window_start=$2`, nodeID, w); got != 2 {
			t.Fatalf("node_warmup.count = %d, want 2", got)
		}
		if err := pg.ResetWarmup(ctx, nodeID, w, 5); err != nil {
			t.Fatalf("ResetWarmup: %v", err)
		}
		if c, cp := suOne[int](t, su, `SELECT count FROM verify.node_warmup WHERE node_id=$1 AND window_start=$2`, nodeID, w),
			suOne[int](t, su, `SELECT cap FROM verify.node_warmup WHERE node_id=$1 AND window_start=$2`, nodeID, w); c != 0 || cp != 5 {
			t.Fatalf("after ResetWarmup: count=%d cap=%d, want 0/5", c, cp)
		}
	})

	t.Run("rate_budget_reserve_and_peek", func(t *testing.T) {
		p := RateParams{Capacity: 2, RefillPerSec: 0}
		for i := 0; i < 2; i++ {
			if d, err := pg.ReserveRateToken(ctx, nodeID, "rt-bucket", p, now); err != nil || !d.Allowed {
				t.Fatalf("ReserveRateToken #%d = %+v err=%v", i+1, d, err)
			}
		}
		if d, err := pg.ReserveRateToken(ctx, nodeID, "rt-bucket", p, now); err != nil || d.Allowed {
			t.Fatalf("third token must be denied, got %+v err=%v", d, err)
		}
		if got := suOne[float64](t, su, `SELECT tokens FROM verify.rate_budgets WHERE node_id=$1 AND provider_bucket='rt-bucket'`, nodeID); got != 0 {
			t.Fatalf("rate_budgets.tokens = %v, want 0 after draining", got)
		}
		if d, err := pg.PeekRateBudget(ctx, nodeID, "rt-peek", p, now); err != nil || !d.Allowed {
			t.Fatalf("PeekRateBudget on a fresh bucket = %+v err=%v", d, err)
		}
		if got := suOne[float64](t, su, `SELECT tokens FROM verify.rate_budgets WHERE node_id=$1 AND provider_bucket='rt-peek'`, nodeID); got != 2 {
			t.Fatalf("a peek must not consume: tokens = %v, want 2", got)
		}
	})

	t.Run("AppendVerification_with_raw_sidecar", func(t *testing.T) {
		row := rtRow(t, now, "corp.test", []byte{0xa1, 0xb2, 0xc3, 0xd4})
		raw := &RawRow{VerificationID: row.ID, Raw: []byte(`{"wrapper_api_version":"v1","success":true}`)}
		if err := pg.AppendVerification(ctx, row, raw); err != nil {
			t.Fatalf("AppendVerification (verifications + verification_raw sidecar): %v", err)
		}
		if got := suOne[string](t, su, `SELECT reason FROM verify.verifications WHERE id=$1`, row.ID); got != "accepted" {
			t.Fatalf("verifications.reason = %q", got)
		}
		if got := suOne[string](t, su, `SELECT raw->>'success' FROM verify.verification_raw WHERE verification_id=$1`, row.ID); got != "true" {
			t.Fatalf("verification_raw.raw success = %q", got)
		}
		part := suOne[string](t, su, `SELECT tableoid::regclass::text FROM verify.verification_raw WHERE verification_id=$1`, row.ID)
		if want := "verify.verification_raw_" + now.Format("2006_01"); part != want {
			t.Fatalf("the sidecar row landed in %q, want the %s partition", part, want)
		}
		// The LOG row is also visible to the runtime's own SELECT path (block-rate window).
		stats, err := pg.BlockRateByVerifierIP(ctx, now.Add(-time.Hour))
		if err != nil {
			t.Fatalf("BlockRateByVerifierIP: %v", err)
		}
		found := false
		for _, s := range stats {
			if s.VerifierIP == "203.0.113.7" && s.Attempts == 1 {
				found = true
			}
		}
		if !found {
			t.Fatalf("BlockRateByVerifierIP did not see the row just appended: %+v", stats)
		}
	})

	t.Run("cache_put_get_and_update", func(t *testing.T) {
		hash := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
		cv := CachedVerdict{
			EmailHash: hash, ConfigFingerprint: "cfg_sha256:rt", Domain: "cache.test", MXProvider: "other", HashScheme: "norm/v1+hmac-sha256/v1",
			Status: "risky", Reason: "plausible", Confidence: 30, Recommendation: "review",
			Flags: map[string]bool{"from_cache": false}, Signals: map[string]any{"syntax_valid": true},
			CachedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		if err := pg.PutCachedVerdict(ctx, cv); err != nil {
			t.Fatalf("PutCachedVerdict (INSERT): %v", err)
		}
		got, err := pg.GetCachedVerdict(ctx, hash, "cfg_sha256:rt")
		if err != nil || got.Status != "risky" || got.Domain != "cache.test" {
			t.Fatalf("GetCachedVerdict = %+v err=%v", got, err)
		}
		cv.Status, cv.Reason, cv.Confidence, cv.Recommendation = "valid", "accepted", 90, "send"
		if err := pg.PutCachedVerdict(ctx, cv); err != nil {
			t.Fatalf("PutCachedVerdict (ON CONFLICT DO UPDATE): %v", err)
		}
		if got := suOne[string](t, su, `SELECT status FROM verify.cache WHERE email_hash=$1 AND config_fingerprint='cfg_sha256:rt'`, hash); got != "valid" {
			t.Fatalf("cache.status after the update = %q", got)
		}
		if _, err := pg.GetCachedVerdict(ctx, hash, "other-config"); err != ErrCacheMiss {
			t.Fatalf("a different config must miss, got %v", err)
		}
	})

	t.Run("catchall_domains", func(t *testing.T) {
		dom := "catchall-rt.test"
		tru, fal := true, false
		if err := pg.SetCatchAll(ctx, dom, &tru, "probe", now.Add(time.Hour)); err != nil {
			t.Fatalf("SetCatchAll(true) (INSERT): %v", err)
		}
		if got := suOne[bool](t, su, `SELECT is_catchall FROM verify.catchall_domains WHERE domain=$1`, dom); !got {
			t.Fatal("catchall_domains.is_catchall must be true")
		}
		if err := pg.SetCatchAll(ctx, dom, &fal, "probe", now.Add(time.Hour)); err != nil {
			t.Fatalf("SetCatchAll(false) (ON CONFLICT DO UPDATE): %v", err)
		}
		if got := suOne[bool](t, su, `SELECT is_catchall FROM verify.catchall_domains WHERE domain=$1`, dom); got {
			t.Fatal("catchall_domains.is_catchall must be false after the re-probe")
		}
	})

	t.Run("disposable_and_spam_trap_lists", func(t *testing.T) {
		// verify_app can only SELECT these lists, so the seed goes through the superuser connection.
		suExec(t, su, `INSERT INTO verify.disposable_domains (domain, source, added_at) VALUES ('disposable-rt.test','rt',now())`)
		suExec(t, su, `INSERT INTO verify.spam_trap_domains (domain, source, added_at) VALUES ('trap-rt.test','rt',now())`)
		for _, c := range []struct {
			name   string
			fn     func(context.Context, string) (bool, error)
			domain string
			want   bool
		}{
			{"IsDisposable listed", pg.IsDisposable, "disposable-rt.test", true},
			{"IsDisposable unlisted", pg.IsDisposable, "not-listed-rt.test", false},
			{"IsSpamTrap listed", pg.IsSpamTrap, "trap-rt.test", true},
			{"IsSpamTrap unlisted", pg.IsSpamTrap, "disposable-rt.test", false},
		} {
			if got, err := c.fn(ctx, c.domain); err != nil || got != c.want {
				t.Errorf("%s(%q) = %v err=%v, want %v", c.name, c.domain, got, err, c.want)
			}
		}
	})

	t.Run("queue_and_job_lifecycle", func(t *testing.T) {
		const owner = "0b0f4f2a-6d3c-4c53-9f3b-1b6a2c9d7e11"
		jobID := mustV7(t, now)
		mk := func(tag string) WorkItemRow {
			id := mustV7(t, now)
			return WorkItemRow{ID: id, IdempotencyKey: []byte("rt-idem-" + tag), JobID: jobID, Domain: "queue.test", EmailHash: []byte("h-" + tag),
				PolicyKey: "cold-email", PolicyVersion: 1, ConfigFingerprint: "cfg_sha256:rt", Status: "ready",
				NotBefore: now.Add(-time.Minute), AddrCiphertext: []byte("ct-" + tag)}
		}
		i1, i2 := mk("one"), mk("two")
		// 0018: the job, ALL its input rows and the LOG row of its malformed row are recorded in ONE transaction, as verify_app: INSERT on
		// verify.jobs, verify.verifications and verify.job_rows. Row 0 carries a caller label, row 1 is a work item, row 2 is malformed
		// (its own invalid_syntax LOG row, NULL hash, §3.2.7).
		bad := rtRow(t, now, "", nil)
		bad.EmailHash, bad.HashScheme, bad.Domain, bad.MXProvider, bad.NodeID, bad.EngineVersion, bad.EngineValidationType, bad.VerifierIP = nil, "", "", "", "", "", "", ""
		bad.EngineSuccessSet, bad.EngineSuccess, bad.LatencyMS = false, false, 0
		bad.Status, bad.Reason, bad.Confidence, bad.Recommendation = "invalid", "invalid_syntax", 99, "skip"
		bad.Flags, bad.Signals = map[string]bool{"from_cache": false}, map[string]any{"syntax_valid": false}
		if err := pg.RecordJob(ctx, JobRow{ID: jobID, PolicyKey: "cold-email", PolicyVersion: 1, Status: "running", TotalCount: 3, CallerRef: "rt-ref", OwnerUserID: owner},
			[]JobRowEntry{
				{Ordinal: 0, ItemID: "rt-row-0", WorkItemID: i1.ID},
				{Ordinal: 1, WorkItemID: i2.ID},
				{Ordinal: 2, ItemID: "rt-bad", Malformed: true, VerificationID: bad.ID},
			}, []VerificationRow{bad}); err != nil {
			t.Fatalf("RecordJob as verify_app (INSERT on verify.jobs, verify.verifications and verify.job_rows): %v", err)
		}
		if got := suOne[string](t, su, `SELECT owner_user_id::text FROM verify.jobs WHERE id=$1`, jobID); got != owner {
			t.Fatalf("jobs.owner_user_id = %q", got)
		}
		if got := suOne[int](t, su, `SELECT count(*) FROM verify.job_rows WHERE job_id=$1`, jobID); got != 3 {
			t.Fatalf("job_rows rows = %d, want 3", got)
		}
		if got := suOne[int](t, su, `SELECT count(*) FROM verify.verifications WHERE id=$1 AND reason='invalid_syntax' AND email_hash IS NULL AND hash_scheme IS NULL AND NOT from_cache`, bad.ID); got != 1 {
			t.Fatalf("the malformed row's LOG row (invalid_syntax, NULL hash, from_cache false) = %d rows, want exactly 1", got)
		}
		for _, it := range []WorkItemRow{i1, i2} {
			if id, dup, err := pg.EnqueueWorkItem(ctx, it); err != nil || dup || id != it.ID {
				t.Fatalf("EnqueueWorkItem(%s) = %q dup=%v err=%v", it.ID, id, dup, err)
			}
		}
		if id, dup, err := pg.EnqueueWorkItem(ctx, i1); err != nil || !dup || id != i1.ID {
			t.Fatalf("a repeated idempotency key in the SAME job must dedup and name the work item that holds it, got id=%q dup=%v err=%v", id, dup, err)
		}
		if got := suOne[int](t, su, `SELECT count(*) FROM verify.work_items WHERE job_id=$1`, jobID); got != 2 {
			t.Fatalf("work_items rows = %d, want 2", got)
		}

		leased, err := pg.DequeueReady(ctx, "worker-rt", 10, now)
		if err != nil || len(leased) != 2 {
			t.Fatalf("DequeueReady = %d items err=%v, want 2", len(leased), err)
		}
		if got := suOne[int](t, su, `SELECT count(*) FROM verify.work_items WHERE job_id=$1 AND status='in_flight' AND locked_by='worker-rt'`, jobID); got != 2 {
			t.Fatalf("in_flight rows = %d, want 2", got)
		}

		// Settle item one: the LOG row + raw sidecar + job progress + the attempt transcript commit together.
		v1 := rtRow(t, now, "queue.test", []byte("h-one"))
		att := WorkItemAttemptRow{ID: mustV7(t, now), WorkItemID: i1.ID, Attempt: 1, AttemptedAt: now, NodeID: nodeID, VerifierIP: "203.0.113.7", Reason: "accepted", Signals: map[string]any{}, LatencyMS: 9}
		if err := pg.SettleWorkItem(ctx, i1.ID, "worker-rt", "settled", v1, &RawRow{VerificationID: v1.ID, Raw: []byte(`{"success":true}`)}, att, nil); err != nil {
			t.Fatalf("SettleWorkItem: %v", err)
		}
		if st, vid := suOne[string](t, su, `SELECT status FROM verify.work_items WHERE id=$1`, i1.ID), suOne[string](t, su, `SELECT verification_id::text FROM verify.work_items WHERE id=$1`, i1.ID); st != "settled" || vid != v1.ID {
			t.Fatalf("settled item: status=%q verification_id=%q, want settled/%s", st, vid, v1.ID)
		}
		if got := suOne[bool](t, su, `SELECT addr_ciphertext IS NULL FROM verify.work_items WHERE id=$1`, i1.ID); !got {
			t.Fatal("the plaintext ciphertext must be purged in the settlement transaction")
		}
		if got := suOne[int](t, su, `SELECT settled_count FROM verify.jobs WHERE id=$1`, jobID); got != 1 {
			t.Fatalf("jobs.settled_count = %d, want 1", got)
		}
		if got := suOne[int](t, su, `SELECT count(*) FROM verify.work_item_attempts WHERE work_item_id=$1`, i1.ID); got != 1 {
			t.Fatalf("work_item_attempts rows = %d, want 1", got)
		}
		if got := suOne[int](t, su, `SELECT count(*) FROM verify.verification_raw WHERE verification_id=$1`, v1.ID); got != 1 {
			t.Fatalf("verification_raw rows for the settled verdict = %d, want 1", got)
		}

		// Requeue item two (provider-transient: bumps the attempt), lease it again, and let its lease expire.
		att2 := WorkItemAttemptRow{ID: mustV7(t, now), WorkItemID: i2.ID, Attempt: 1, AttemptedAt: now, NodeID: nodeID, Reason: "greylisted", Signals: map[string]any{}, LatencyMS: 7}
		if err := pg.RequeueWorkItem(ctx, i2.ID, "worker-rt", now.Add(time.Second), "greylisted", true, att2); err != nil {
			t.Fatalf("RequeueWorkItem: %v", err)
		}
		if st, at, why := suOne[string](t, su, `SELECT status FROM verify.work_items WHERE id=$1`, i2.ID), suOne[int](t, su, `SELECT attempt FROM verify.work_items WHERE id=$1`, i2.ID),
			suOne[string](t, su, `SELECT last_reason FROM verify.work_items WHERE id=$1`, i2.ID); st != "waiting_retry" || at != 1 || why != "greylisted" {
			t.Fatalf("requeued item: status=%q attempt=%d last_reason=%q", st, at, why)
		}
		later := now.Add(time.Minute)
		again, err := pg.DequeueReady(ctx, "worker-rt-2", 10, later)
		if err != nil || len(again) != 1 || again[0].ID != i2.ID {
			t.Fatalf("DequeueReady after requeue = %d items err=%v, want item two", len(again), err)
		}
		reclaimed, err := pg.ReclaimExpiredLeases(ctx, time.Minute, later.Add(2*time.Minute))
		if err != nil || reclaimed != 1 {
			t.Fatalf("ReclaimExpiredLeases = %d err=%v, want 1", reclaimed, err)
		}
		if st, lb := suOne[string](t, su, `SELECT status FROM verify.work_items WHERE id=$1`, i2.ID), suOne[bool](t, su, `SELECT locked_by IS NULL FROM verify.work_items WHERE id=$1`, i2.ID); st != "ready" || !lb {
			t.Fatalf("reclaimed item: status=%q lock cleared=%v", st, lb)
		}

		// Reads the API serves for a job.
		res, next, err := pg.ListJobResults(ctx, jobID, owner, "", 10)
		if err != nil || next != "" || len(res) != 2 || res[0].WorkItemID != i1.ID || res[0].Verdict.ID != v1.ID || res[0].Verdict.Reason != "accepted" ||
			res[0].Ordinal != 0 || res[0].ItemID != "rt-row-0" {
			t.Fatalf("ListJobResults = %+v next=%q err=%v, want the settled item's row (ordinal 0, item_id rt-row-0, its verdict) and the malformed row, nothing else", res, next, err)
		}
		// The malformed row is served FROM ITS OWN LOG ROW (read back through the runtime login), never made up.
		if m := res[1]; !m.Malformed || m.Ordinal != 2 || m.ItemID != "rt-bad" || m.WorkItemID != "" || m.Verdict.ID != bad.ID ||
			m.Verdict.Status != "invalid" || m.Verdict.Reason != "invalid_syntax" || m.Verdict.Recommendation != "skip" || m.Verdict.Confidence != 99 {
			t.Fatalf("the malformed result = %+v, want ordinal 2 / rt-bad answered by LOG row %s (invalid/invalid_syntax/skip/99)", m, bad.ID)
		}
		if other, _, err := pg.ListJobResults(ctx, jobID, "11111111-1111-4111-8111-111111111111", "", 10); err != nil || len(other) != 0 {
			t.Fatalf("a different owner must see no results, got %d err=%v", len(other), err)
		}
		j, known, err := pg.GetJob(ctx, jobID, owner)
		if err != nil || !known || j.TotalCount != 3 || j.SettledCount != 1 || j.CallerRef != "rt-ref" {
			t.Fatalf("GetJob = %+v known=%v err=%v", j, known, err)
		}
		complete, total, settled, err := pg.JobCompletion(ctx, jobID, owner)
		if err != nil || complete || total != 3 || settled != 2 {
			t.Fatalf("JobCompletion = complete=%v total=%d settled=%d err=%v, want not complete 3 rows of which 2 have an outcome (item one's row and the malformed row)", complete, total, settled, err)
		}
	})

	t.Run("LookupCaller", func(t *testing.T) {
		// verify_app only has SELECT on api_callers: the key is issued through the superuser connection.
		key := []byte("rt-key-" + now.Format("150405.000000000"))
		const user = "22222222-2222-4222-8222-222222222222"
		suExec(t, su, `INSERT INTO verify.api_callers (caller_id, key_sha256, not_before, user_id) VALUES ($1,$2,$3,$4)`, "caller_rt", key, now.Add(-time.Hour), user)
		id, owner, ok, err := pg.LookupCaller(ctx, key, now)
		if err != nil || !ok || id != "caller_rt" || owner != user {
			t.Fatalf("LookupCaller = %q %q ok=%v err=%v", id, owner, ok, err)
		}
		if _, _, ok, err := pg.LookupCaller(ctx, []byte("no-such-key"), now); err != nil || ok {
			t.Fatalf("an unknown key must miss without error, got ok=%v err=%v", ok, err)
		}
	})

	t.Run("raw_partitions_through_the_wrapper", func(t *testing.T) {
		const ahead = 6
		before, err := testpg.Parts(ctx, suDSN)
		if err != nil {
			t.Fatal(err)
		}
		have := map[string]bool{}
		for _, p := range before {
			have[p.Name] = true
		}
		wantNew := 0
		for i := 0; i <= ahead; i++ {
			if !have["verification_raw_"+testpg.MonthStart(now).AddDate(0, i, 0).Format("2006_01")] {
				wantNew++
			}
		}
		if wantNew < 3 {
			t.Fatalf("precondition: a fresh database covers at most the 3 legacy months plus this one, so >= 3 of the next %d months must be missing, got %d", ahead, wantNew)
		}
		created, err := pg.EnsureRawPartitions(ctx, ahead)
		if err != nil || created != wantNew {
			t.Fatalf("EnsureRawPartitions(%d) through the SECURITY DEFINER wrapper = %d err=%v, want %d", ahead, created, err, wantNew)
		}
		after, err := testpg.Parts(ctx, suDSN)
		if err != nil {
			t.Fatal(err)
		}
		if err := testpg.HeadroomError(after, now, ahead); err != nil {
			t.Fatalf("headroom after the wrapper call: %v", err)
		}
		if again, err := pg.EnsureRawPartitions(ctx, ahead); err != nil || again != 0 {
			t.Fatalf("a second call must be a no-op, got %d err=%v", again, err)
		}
		// The partitions the wrapper created carry the grants the runtime needs: a verdict dated in the LAST new month inserts as verify_app.
		last := testpg.MonthStart(now).AddDate(0, ahead, 0)
		part := "verify.verification_raw_" + last.Format("2006_01")
		if !suOne[bool](t, su, `SELECT has_table_privilege('verify_app', $1, 'INSERT') AND NOT has_table_privilege('verify_app', $1, 'UPDATE') AND NOT has_table_privilege('verify_app', $1, 'DELETE')`, part) {
			t.Fatalf("%s must grant verify_app INSERT but neither UPDATE nor DELETE", part)
		}
		if got := suOne[string](t, su, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = $1::regclass`, part); got != "verify_owner" {
			t.Fatalf("%s is owned by %q, want verify_owner (the DEFINER wrapper creates it as the owner)", part, got)
		}
		row := rtRow(t, last.Add(time.Hour), "future.test", []byte{0xf0, 0xf1})
		if err := pg.AppendVerification(ctx, row, &RawRow{VerificationID: row.ID, Raw: []byte(`{"success":true}`)}); err != nil {
			t.Fatalf("AppendVerification into the newly created %s: %v", part, err)
		}
		if got := suOne[string](t, su, `SELECT tableoid::regclass::text FROM verify.verification_raw WHERE verification_id=$1`, row.ID); got != part {
			t.Fatalf("the row landed in %q, want %q", got, part)
		}
	})

	t.Run("refusals_the_runtime_must_keep", func(t *testing.T) {
		for _, c := range []struct{ name, sql string }{
			{"UPDATE the LOG (Door 2)", `UPDATE verify.verifications SET status='valid' WHERE false`},
			{"DELETE from the LOG (Door 2)", `DELETE FROM verify.verifications WHERE false`},
			{"TRUNCATE the LOG", `TRUNCATE verify.verifications`},
			{"DELETE from the raw sidecar", `DELETE FROM verify.verification_raw WHERE false`},
			{"mint an API key", `INSERT INTO verify.api_callers (caller_id, key_sha256, not_before) VALUES ('forged', decode('00','hex'), now())`},
			{"rewrite a policy definition (only is_default is updatable)", `UPDATE verify.policies SET definition = '{}'::jsonb WHERE false`},
			{"DELETE a policy", `DELETE FROM verify.policies WHERE false`},
			{"CREATE in schema verify", `CREATE TABLE verify.zz_intruder (i int)`},
			{"call the admin core with a caller-chosen time", `SELECT verify.ensure_verification_raw_partitions_at(now(), 1)`},
		} {
			_, err := pg.Pool().Exec(ctx, c.sql)
			if got := pgCode(err); got != "42501" {
				t.Errorf("%s: want SQLSTATE 42501 (insufficient_privilege), got %q (err=%v)", c.name, got, err)
			}
		}
	})
}

// TestRuntimeAsVerifyApp_ControlMissingPoliciesGrant is the paired control of TestRuntimeAsVerifyApp: with the one grant 0016 ADDS taken away
// again, the startup path must fail with 42501. It proves the test above would notice the missing grant (the defect that made a login holding only
// cp_runtime's old privileges fail at startup and never serve), once for INSERT and once for the column-level UPDATE(is_default).
func TestRuntimeAsVerifyApp_ControlMissingPoliciesGrant(t *testing.T) {
	ctx := context.Background()
	suDSN := testpg.Scratch(t, 0)
	su := superConn(t, suDSN)
	pg := openAsVerifyApp(t, suDSN)
	def := []byte(`{"key":"rt-control","version":1}`)

	// (1) INSERT revoked.
	suExec(t, su, `REVOKE INSERT ON verify.policies FROM verify_app`)
	err := pg.EnsurePolicy(ctx, "rt-control", 1, def, true)
	if err == nil || pgCode(err) != "42501" {
		t.Fatalf("EnsurePolicy without INSERT on verify.policies must fail with 42501, got %v", err)
	}
	if got := suOne[int](t, su, `SELECT count(*) FROM verify.policies`); got != 0 {
		t.Fatalf("a refused INSERT must leave no row, found %d", got)
	}

	// (2) INSERT restored, UPDATE(is_default) revoked: the row goes in, flipping the default is refused.
	suExec(t, su, `GRANT INSERT ON verify.policies TO verify_app`)
	suExec(t, su, `REVOKE UPDATE (is_default) ON verify.policies FROM verify_app`)
	err = pg.EnsurePolicy(ctx, "rt-control", 1, def, true)
	if err == nil || pgCode(err) != "42501" {
		t.Fatalf("EnsurePolicy without UPDATE(is_default) must fail with 42501 on the default flip, got %v", err)
	}
	if got := suOne[int](t, su, `SELECT count(*) FROM verify.policies WHERE key='rt-control' AND is_default = false`); got != 1 {
		t.Fatalf("the INSERT half must have succeeded and the flip must not have happened, rows=%d", got)
	}

	// (3) Both back: the same call now succeeds, so (1) and (2) failed because of the grants and nothing else.
	suExec(t, su, `GRANT UPDATE (is_default) ON verify.policies TO verify_app`)
	if err := pg.EnsurePolicy(ctx, "rt-control", 1, def, true); err != nil {
		t.Fatalf("EnsurePolicy with both grants restored: %v", err)
	}
	if got := suOne[bool](t, su, `SELECT is_default FROM verify.policies WHERE key='rt-control' AND version=1`); !got {
		t.Fatal("the default flag must be set once both grants are back")
	}
}
