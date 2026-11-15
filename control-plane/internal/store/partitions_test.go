package store

// Tests for the verification_raw partition maintenance (ADR-0017 / migration 0014).
//
// Every PG test here runs in a THROWAWAY database (testpg.Scratch) so it can age, break and race the
// table without touching the shared test database other packages insert into in parallel. They SKIP
// honestly without TM_TEST_DATABASE_URL, like every PG-gated test in this package.
//
// The property under guard is "the table always has this month + N months ahead", and the standing
// proof that the guard is not vacuous is in THIS file, not in a one-off manual mutation:
//   - TestPGRawPartitions_LegacyCliffIsReal         the original defect, reproduced (date-independent)
//   - TestPGRawPartitions_MaintenanceKeepsNMonthsAhead   the guard goes GREEN after a real tick
//   - TestPGRawPartitions_NoopMaintenanceLeavesTheCliff  ...and RED when the tick does nothing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/idgen"
	"github.com/coldmail-verify/control-plane/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const partitionsAhead = 3 // the default N; the tests assert behavior for this value, not the constant's origin

func scratchPG(t *testing.T, through int) (*PG, string) {
	t.Helper()
	dsn := testpg.Scratch(t, through)
	pg, err := OpenPG(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenPG(scratch): %v", err)
	}
	t.Cleanup(pg.Close)
	return pg, dsn
}

// dbNow is the database's clock — the same clock ensure_verification_raw_partitions() uses.
func dbNow(t *testing.T, pg *PG) time.Time {
	t.Helper()
	var now time.Time
	if err := pg.pool.QueryRow(context.Background(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatalf("SELECT now(): %v", err)
	}
	return now.UTC()
}

// rawRowAt builds an SMTP verdict row + raw sidecar dated `at`.
func rawRowAt(t *testing.T, at time.Time) (VerificationRow, *RawRow) {
	t.Helper()
	id, err := idgen.NewV7String(at)
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	reqID, _ := idgen.NewV7String(at)
	row := VerificationRow{
		ID: id, RequestID: reqID, CheckedAt: at, CreatedAt: at,
		EmailHash: []byte{0x0a, 0x0b, 0x0c, 0x0d}, HashScheme: "norm/v1+hmac-sha256/v1",
		Domain: "corp.test", Status: "valid", Reason: "accepted", Confidence: 90, Recommendation: "send",
		Flags: map[string]bool{"from_cache": false}, Signals: map[string]any{"smtp_code": "250"},
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg_sha256:test",
		NodeID: "n1", EngineVersion: "rack@0.9.0+gem@3.3.1", EngineValidationType: "smtp",
		EngineSuccessSet: true, EngineSuccess: true,
	}
	return row, &RawRow{VerificationID: id, Raw: []byte(`{"success":true}`)}
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// ageTable drops every partition dated AFTER the current month, simulating a database whose
// maintenance has not run for months. Partitions at or before the current month are kept.
func ageTable(t *testing.T, dsn string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	parts, err := testpg.Parts(ctx, dsn)
	if err != nil {
		t.Fatalf("Parts: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	for _, p := range parts {
		if p.From.After(testpg.MonthStart(now)) {
			if _, err := conn.Exec(ctx, `DROP TABLE verify.`+pgx.Identifier{p.Name}.Sanitize()); err != nil {
				t.Fatalf("drop %s: %v", p.Name, err)
			}
		}
	}
}

// ── the original defect, reproduced ─────────────────────────────────────────────────

// TestPGRawPartitions_LegacyCliffIsReal reproduces the production failure on the schema as it was
// before 0014 (migrations 0001..0013). It is deliberately DATE-INDEPENDENT: it uses a date after the
// last legacy partition (2026-09), so it holds on any day this suite runs — unlike
// TestPGAppendWithRawSidecar, which only started failing when the calendar passed 2026-09-30.
//
// Since 0016 the store addresses schema verify only, and 0001..0013 alone leave the tables in public, so this
// database is 0001..0013 plus 0016 (0014 and 0015 left out): the three legacy partitions, no 0014 bootstrap
// partition, no headroom, in the schema the store reads. A state no upgrade passes through, chosen because it is
// the only one in which the store can still reach the pre-0014 partition layout.
func TestPGRawPartitions_LegacyCliffIsReal(t *testing.T) {
	dsn := testpg.ScratchSelect(t, func(n int) bool { return n <= 13 || n == 16 })
	pg, err := OpenPG(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenPG(scratch): %v", err)
	}
	t.Cleanup(pg.Close)
	ctx := context.Background()

	// Control: a date INSIDE legacy coverage inserts fine, so the failure below is the missing
	// partition and not something generic about the row.
	row, raw := rawRowAt(t, time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC))
	if err := pg.AppendVerification(ctx, row, raw); err != nil {
		t.Fatalf("control: a row inside the legacy partitions must insert: %v", err)
	}

	row, raw = rawRowAt(t, time.Date(2027, 1, 15, 12, 0, 0, 0, time.UTC))
	err = pg.AppendVerification(ctx, row, raw)
	if err == nil {
		t.Fatal("a row dated past the last legacy partition must be REJECTED on the pre-0014 schema — the cliff did not reproduce")
	}
	if got := pgCode(err); got != "23514" {
		t.Fatalf("want SQLSTATE 23514 (no partition for row), got %q: %v", got, err)
	}
}

// ── the guard: green after a real tick ──────────────────────────────────────────────

func TestPGRawPartitions_MaintenanceKeepsNMonthsAhead(t *testing.T) {
	pg, dsn := scratchPG(t, 0)
	ctx := context.Background()
	now := dbNow(t, pg)
	ageTable(t, dsn, now) // a database whose maintenance has been dead for months

	// The guard must be able to SEE the cliff on the real table before the fix runs.
	before, err := testpg.Parts(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if testpg.HeadroomError(before, now, partitionsAhead) == nil {
		t.Fatal("precondition: the aged table must FAIL the headroom guard, otherwise this test proves nothing")
	}

	created, err := MaintainRawPartitionsOnce(ctx, pg, partitionsAhead)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if created != partitionsAhead {
		t.Fatalf("first tick on an aged table should create exactly %d partitions (current month already exists), created %d", partitionsAhead, created)
	}
	after, err := testpg.Parts(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := testpg.HeadroomError(after, now, partitionsAhead); err != nil {
		t.Fatalf("after a tick the guard must pass: %v", err)
	}

	// Idempotent.
	if again, err := MaintainRawPartitionsOnce(ctx, pg, partitionsAhead); err != nil || again != 0 {
		t.Fatalf("second tick must be a no-op, got created=%d err=%v", again, err)
	}

	// The headroom is real and finite: the last covered instant inserts; the first uncovered one does not.
	lastCovered := testpg.MonthStart(now).AddDate(0, partitionsAhead+1, 0).Add(-time.Second)
	row, raw := rawRowAt(t, lastCovered)
	if err := pg.AppendVerification(ctx, row, raw); err != nil {
		t.Fatalf("a row at the last covered instant (%s) must insert: %v", lastCovered.Format(time.RFC3339), err)
	}
	firstUncovered := testpg.MonthStart(now).AddDate(0, partitionsAhead+1, 0)
	row, raw = rawRowAt(t, firstUncovered)
	if err := pg.AppendVerification(ctx, row, raw); pgCode(err) != "23514" {
		t.Fatalf("a row at the first uncovered instant (%s) must be rejected with 23514, got: %v", firstUncovered.Format(time.RFC3339), err)
	}
}

// ── the guard: RED when maintenance does nothing ────────────────────────────────────

type noopPartitioner struct{ calls int }

func (n *noopPartitioner) EnsureRawPartitions(context.Context, int) (int, error) {
	n.calls++
	return 0, nil
}

// TestPGRawPartitions_NoopMaintenanceLeavesTheCliff is the standing paired control: if the tick does
// nothing, the headroom guard MUST fail. A guard that passes here would be a green that cannot go red.
func TestPGRawPartitions_NoopMaintenanceLeavesTheCliff(t *testing.T) {
	pg, dsn := scratchPG(t, 0)
	ctx := context.Background()
	now := dbNow(t, pg)
	ageTable(t, dsn, now)

	np := &noopPartitioner{}
	if _, err := MaintainRawPartitionsOnce(ctx, np, partitionsAhead); err != nil {
		t.Fatal(err)
	}
	if np.calls != 1 {
		t.Fatalf("the tick must actually call the partitioner exactly once, called %d", np.calls)
	}
	parts, err := testpg.Parts(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	err = testpg.HeadroomError(parts, now, partitionsAhead)
	if err == nil {
		t.Fatal("with maintenance doing nothing the headroom guard must FAIL — it passed, so it cannot detect the cliff")
	}
	t.Logf("guard correctly red with maintenance disabled: %v", err)
}

func TestMaintainRawPartitionsOnce_RefusesZeroHeadroom(t *testing.T) {
	np := &noopPartitioner{}
	for _, n := range []int{0, -1} {
		if _, err := MaintainRawPartitionsOnce(context.Background(), np, n); err == nil {
			t.Errorf("monthsAhead=%d must be refused: zero headroom IS the cliff", n)
		}
	}
	if np.calls != 0 {
		t.Errorf("a refused tick must not reach the partitioner, calls=%d", np.calls)
	}
	// Control: a legal N does reach it.
	if _, err := MaintainRawPartitionsOnce(context.Background(), np, 1); err != nil || np.calls != 1 {
		t.Errorf("monthsAhead=1 must call the partitioner once: calls=%d err=%v", np.calls, err)
	}
}

// ── least privilege: the runtime door is narrow ─────────────────────────────────────

func inRuntimeTx(t *testing.T, dsn string, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE verify_app`); err != nil {
		t.Fatalf("SET LOCAL ROLE verify_app: %v", err)
	}
	fn(ctx, tx)
}

func TestPGRawPartitions_RuntimeDoorIsNarrow(t *testing.T) {
	_, dsn := scratchPG(t, 0)

	// Control: the role switch really drops privilege (same discipline as pg_grants_test.go).
	inRuntimeTx(t, dsn, func(ctx context.Context, tx pgx.Tx) {
		var who string
		var super bool
		if err := tx.QueryRow(ctx, `SELECT current_user, (SELECT rolsuper FROM pg_roles WHERE rolname = current_user)`).Scan(&who, &super); err != nil {
			t.Fatal(err)
		}
		if who != "verify_app" || super {
			t.Fatalf("control: expected non-superuser verify_app, got %s super=%v", who, super)
		}
	})

	// Allowed: the one door.
	inRuntimeTx(t, dsn, func(ctx context.Context, tx pgx.Tx) {
		var n int
		if err := tx.QueryRow(ctx, `SELECT verify.ensure_verification_raw_partitions(1)`).Scan(&n); err != nil {
			t.Fatalf("verify_app must be able to call ensure_verification_raw_partitions: %v", err)
		}
	})

	denied := []struct{ name, sql, want string }{
		{"core with caller-supplied time", `SELECT verify.ensure_verification_raw_partitions_at(now(), 1)`, "42501"},
		{"raw CREATE PARTITION", `CREATE TABLE verify.verification_raw_2099_01 PARTITION OF verify.verification_raw FOR VALUES FROM ('2099-01-01') TO ('2099-02-01')`, "42501"},
		{"DROP a partition", `DROP TABLE verify.verification_raw_2026_07`, "42501"},
		{"DELETE rows", `DELETE FROM verify.verification_raw`, "42501"},
		{"UPDATE rows", `UPDATE verify.verification_raw SET raw = NULL`, "42501"},
		{"months_ahead out of range", `SELECT verify.ensure_verification_raw_partitions(121)`, "22023"},
	}
	for _, d := range denied {
		d := d
		inRuntimeTx(t, dsn, func(ctx context.Context, tx pgx.Tx) {
			_, err := tx.Exec(ctx, d.sql)
			if got := pgCode(err); got != d.want {
				t.Errorf("%s: want SQLSTATE %s, got %q (err=%v)", d.name, d.want, got, err)
			}
		})
	}

	// New partitions carry exactly the grants 0005 gave the legacy ones: runtime INSERT+SELECT, never UPDATE/DELETE.
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `SELECT verify.ensure_verification_raw_partitions_at(now(), 2)`); err != nil {
		t.Fatal(err)
	}
	var sel, ins, upd, del bool
	if err := conn.QueryRow(ctx, `
		SELECT has_table_privilege('verify_app', p, 'SELECT'), has_table_privilege('verify_app', p, 'INSERT'),
		       has_table_privilege('verify_app', p, 'UPDATE'), has_table_privilege('verify_app', p, 'DELETE')
		FROM (SELECT 'verify.verification_raw_' || to_char((date_trunc('month', now() AT TIME ZONE 'UTC') + interval '2 months'), 'YYYY_MM') AS p) s`).
		Scan(&sel, &ins, &upd, &del); err != nil {
		t.Fatal(err)
	}
	if !sel || !ins || upd || del {
		t.Fatalf("new partition grants for verify_app must be SELECT+INSERT only, got select=%v insert=%v update=%v delete=%v", sel, ins, upd, del)
	}
}

// ── UTC month alignment ─────────────────────────────────────────────────────────────

// TestPGRawPartitions_BoundsAreUTCMonthAligned: created_at is timestamptz, so a bound is an instant. If
// the bounds followed the CALLING session's time zone a Tokyo session would create a partition that
// starts at 15:00Z on the 28th — leaving a 9-hour gap at every month edge. The function pins UTC.
func TestPGRawPartitions_BoundsAreUTCMonthAligned(t *testing.T) {
	_, dsn := scratchPG(t, 0)
	ctx := context.Background()

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("tzdata unavailable for the control: %v", err)
	}
	// Control: the zone-dependent answer is genuinely different from the UTC one.
	if time.Date(2027, 3, 1, 0, 0, 0, 0, tokyo).Equal(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("control invalid: Tokyo and UTC month starts are equal")
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `SET TIME ZONE 'Asia/Tokyo'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT verify.ensure_verification_raw_partitions_at('2027-03-10 12:00:00+00', 2)`); err != nil {
		t.Fatal(err)
	}
	parts, err := testpg.Parts(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]time.Time{
		"verification_raw_2027_03": {time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)},
		"verification_raw_2027_04": {time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)},
		"verification_raw_2027_05": {time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)},
	}
	found := 0
	for _, p := range parts {
		if w, ok := want[p.Name]; ok {
			found++
			if !p.From.Equal(w[0]) || !p.To.Equal(w[1]) {
				t.Errorf("%s bounds = [%s, %s), want exact UTC month [%s, %s)", p.Name, p.From, p.To, w[0], w[1])
			}
		}
	}
	if found != len(want) {
		t.Fatalf("expected %d partitions for 2027-03..05, found %d (parts=%v)", len(want), found, parts)
	}
}

// ── concurrency ─────────────────────────────────────────────────────────────────────

// TestPGRawPartitions_ConcurrentTicksDoNotCollide: two control-plane instances (or the loop and an
// admin) ticking at once must both succeed, and between them create exactly N partitions. Without the
// advisory lock in the function the loser fails with "relation already exists".
func TestPGRawPartitions_ConcurrentTicksDoNotCollide(t *testing.T) {
	pg, dsn := scratchPG(t, 0)
	now := dbNow(t, pg)
	ageTable(t, dsn, now)

	var wg sync.WaitGroup
	start := make(chan struct{})
	created := make([]int, 4)
	errs := make([]error, 4)
	for i := range created {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			created[i], errs[i] = MaintainRawPartitionsOnce(context.Background(), pg, partitionsAhead)
		}(i)
	}
	close(start)
	wg.Wait()

	total := 0
	for i := range created {
		if errs[i] != nil {
			t.Fatalf("concurrent tick %d failed: %v", i, errs[i])
		}
		total += created[i]
	}
	if total != partitionsAhead {
		t.Fatalf("4 concurrent ticks must create exactly %d partitions between them, created %d (per tick %v)", partitionsAhead, total, created)
	}
}
