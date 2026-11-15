package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/idgen"
	"github.com/jackc/pgx/v5"
)

// =============================================================================
// Grant enforcement, actually enforced (fix-plan item 3 / D13)
// =============================================================================
//
// THE PROBLEM THIS FILE SOLVES. `ci.yml` claims a real Postgres exists so the append-only tests
// "actually RUN in CI … proving append-only on every push". Two tests carried that claim:
// TestPGSendOutcome_RuntimeCannotDelete and TestPGRateBudget_DeleteDeniedForRuntime. Both were
// written as "attempt the DELETE; if it SUCCEEDS, t.Skip('connected as a privileged role')". CI
// connects as superuser `postgres`, so the DELETE always succeeded, so both tests ALWAYS SKIPPED.
// The append-only posture was documented as proven and was not — the dangerous kind of failure,
// because a control nobody re-checks gets trusted.
//
// THE MECHANISM: `SET LOCAL ROLE verify_app` inside a transaction. Three reasons this is the right
// tool for the GRANT matrix here (the shared test DSN stays a superuser, so a login as verify_app is not
// available to these tests; TestRuntimeAsVerifyApp covers the real LOGIN path separately):
//
//	(1) The shared test DSN (TM_TEST_DATABASE_URL) is the superuser `postgres`, which also seeds and cleans
//	    up. verify_app (formerly cp_runtime, migration 0016) is a LOGIN role with no password in the
//	    migrations; setting one on the shared database's cluster-wide role from inside this file would
//	    race the other test packages, so the grant matrix is read through a role switch instead.
//	(2) SET ROLE drops superuser: privilege checks are made against the CURRENT role, so a
//	    superuser session that has SET ROLE to a non-superuser is genuinely subject to the grants.
//	    Verified empirically, not assumed — see the anti-vacuity control below, which proves the
//	    role switch actually bites by checking a privilege verify_app DOES hold and one it does not.
//	(3) SET LOCAL is transaction-scoped, so the role reverts on commit/rollback and cannot leak into
//	    a later test on a pooled connection. The role switch happens on an explicitly acquired
//	    connection, and the transaction is always rolled back.
//
// WHY NOT DELETE THE CLAIM INSTEAD (the plan's other option): the grants are real and worth
// proving. They are the whole of M1's Door-2 append-only enforcement — "enforced by revoked grants,
// not convention" (PROJECT_RULES.md Door 2). A test that can actually fail is worth more than an
// honest comment saying nothing is checked.
//
// FAIL-VERIFY, per test:
//   - Delete `REVOKE UPDATE, DELETE ON send_outcomes FROM cp_runtime` (0012:186; 0016 renames the role) and re-migrate →
//     TestPGGrants_RuntimeCannotDeleteSendOutcomes fails (the DELETE succeeds).
//   - Delete `REVOKE DELETE ON rate_budgets FROM cp_runtime` (0008:78; 0016 renames the role) → the rate_budgets case fails.
//   - Delete `REVOKE UPDATE, DELETE ON verifications FROM cp_runtime` (0005:39; 0016 renames the role) → the LOG case fails.
//     That one is Door 2 itself.
//   - Change asRuntime's `SET LOCAL ROLE verify_app` to `SET LOCAL ROLE postgres` (i.e. don't
//     actually drop privilege) → TestPGGrants_RoleSwitchActuallyBites fails, and so does every
//     denial case above. This is the control that makes the others non-vacuous: without it, a
//     silently-ineffective role switch would let all the "denied" assertions pass for the wrong
//     reason, which is the exact bug the old Skip-on-success tests had.

// asRuntime runs fn inside a transaction whose role is verify_app, then ALWAYS rolls back. The
// returned error is fn's, so a caller asserts on the privilege outcome rather than on plumbing.
//
// It takes a dedicated connection from the pool (not the pool's Exec) because SET LOCAL ROLE binds
// to a session: issuing it through the pool would leave which connection got it up to chance.
func asRuntime(t *testing.T, pg *PG, fn func(pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	conn, err := pg.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// ALWAYS rolled back: nothing this transaction touches is allowed to persist, and the LOCAL role
	// dies with it. A committed grant test would be a test that mutates the shared CI database.
	defer tx.Rollback(ctx) //nolint:errcheck // the rollback IS the cleanup; a post-rollback error is moot

	if _, err := tx.Exec(ctx, `SET LOCAL ROLE verify_app`); err != nil {
		// Not a skip. If the role is missing, the migrations did not run, and pretending that is
		// "not exercised" is how D13 happened. The DSN is present (testPG got past its own gate),
		// so a migrated database is a requirement, not a maybe.
		t.Fatalf("SET LOCAL ROLE verify_app: %v — the migrations (0005_grants.sql) must be applied to TM_TEST_DATABASE_URL", err)
	}
	// Confirm the switch took effect before asserting anything about privileges.
	var who string
	if err := tx.QueryRow(ctx, `SELECT current_user`).Scan(&who); err != nil {
		t.Fatalf("current_user: %v", err)
	}
	if who != "verify_app" {
		t.Fatalf("role switch did not take: current_user=%q, want verify_app — every denial below would be vacuous", who)
	}
	return fn(tx)
}

// denied asserts an operation failed with a PERMISSION error specifically — not merely that it
// failed. A typo'd table name also errors, and would otherwise read as "the grant held".
func denied(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s SUCCEEDED as verify_app — the grant is not enforced (this is the D13 claim being false)", what)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Fatalf("%s failed, but NOT with permission denied: %v — the grant may not be what stopped it", what, err)
	}
}

// TestPGGrants_RoleSwitchActuallyBites is the anti-vacuity control for this whole file: it proves
// SET LOCAL ROLE verify_app genuinely drops privilege, by checking BOTH directions — a privilege
// verify_app HOLDS (SELECT on verifications) still works, and one it does NOT (DELETE on
// verifications) is refused. One direction alone proves nothing: if the role switch silently failed
// we would still be superuser and everything would succeed; if it switched to a role with no
// privileges at all, everything would fail and the "denied" tests would pass for the wrong reason.
func TestPGGrants_RoleSwitchActuallyBites(t *testing.T) {
	pg := testPG(t)

	// Direction 1: a privilege verify_app HAS must still work (so we are not simply unprivileged).
	if err := asRuntime(t, pg, func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM verify.verifications`).Scan(&n)
	}); err != nil {
		t.Fatalf("verify_app must retain SELECT on verifications (0005: GRANT SELECT, INSERT): %v", err)
	}

	// Direction 2: a privilege it does NOT have must be refused (so superuser really was dropped).
	err := asRuntime(t, pg, func(tx pgx.Tx) error {
		_, e := tx.Exec(context.Background(), `DELETE FROM verify.verifications WHERE false`)
		return e
	})
	denied(t, "DELETE FROM verify.verifications", err)
}

// TestPGGrants_RuntimeCannotMutateTheLog is Door 2 itself: THE LOG is append-only, "enforced by
// revoked grants, not convention" (PROJECT_RULES.md Door 2, 0005_grants.sql:39). verify_app may
// INSERT and SELECT; UPDATE and DELETE are revoked, so a correction is a NEW ROW, never a mutation.
//
// This case did not exist before: the two tests D13 named covered send_outcomes and rate_budgets,
// the Phase-7 tables. The central claim — that the verifications LOG cannot be rewritten by the
// runtime — had no enforced test at all.
func TestPGGrants_RuntimeCannotMutateTheLog(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()

	denied(t, "UPDATE verify.verifications", asRuntime(t, pg, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE verify.verifications SET status='valid' WHERE false`)
		return e
	}))
	denied(t, "DELETE FROM verify.verifications", asRuntime(t, pg, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `DELETE FROM verify.verifications WHERE false`)
		return e
	}))
	// The raw sidecar holds the same posture (0005:57) — it is part of the append-only LOG.
	denied(t, "DELETE FROM verify.verification_raw", asRuntime(t, pg, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `DELETE FROM verify.verification_raw WHERE false`)
		return e
	}))
}

// TestPGGrants_RuntimeCannotDeleteSendOutcomes replaces TestPGSendOutcome_RuntimeCannotDelete, which
// always skipped. send_outcomes is append-only for the runtime; retention is verify_admin (DR-3).
func TestPGGrants_RuntimeCannotDeleteSendOutcomes(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()

	denied(t, "DELETE FROM verify.send_outcomes", asRuntime(t, pg, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `DELETE FROM verify.send_outcomes WHERE false`)
		return e
	}))
	// 0012:186 revokes UPDATE too — an outcome cannot be edited after the fact, only appended.
	denied(t, "UPDATE verify.send_outcomes", asRuntime(t, pg, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE verify.send_outcomes SET event='delivered' WHERE false`)
		return e
	}))
}

// TestPGGrants_RuntimeCannotDeleteRateBudgets replaces TestPGRateBudget_DeleteDeniedForRuntime,
// which always skipped. rate_budgets is the token-bucket counter: the runtime needs UPDATE (it
// spends tokens) but must NOT be able to DELETE a bucket — dropping a row would reset a node's
// budget to full, which is the rate limit removing itself. This is why UPDATE-allowed/DELETE-denied
// is the correct pair, and why asserting both is what makes the test meaningful.
func TestPGGrants_RuntimeCannotDeleteRateBudgets(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()

	// UPDATE is granted (0008:77) — the runtime spends tokens. Proven so the DELETE denial below is
	// visibly about DELETE, not about the table being unreachable.
	if err := asRuntime(t, pg, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE verify.rate_budgets SET tokens = tokens WHERE false`)
		return e
	}); err != nil {
		t.Fatalf("verify_app must retain UPDATE on rate_budgets (it spends tokens): %v", err)
	}
	denied(t, "DELETE FROM verify.rate_budgets", asRuntime(t, pg, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `DELETE FROM verify.rate_budgets WHERE false`)
		return e
	}))
}

// TestPGGrants_RuntimeCannotIssueKeys proves the privilege-escalation boundary on api_callers
// (0010:43, and 0013 re-REVOKEs it after adding user_id): the runtime READS keys to authenticate but
// cannot MINT one. A runtime that could INSERT into api_callers could issue itself a key for any
// platform user — which, now that jobs are owner-scoped (0013), would be a way around the tenancy
// fix rather than a separate concern.
func TestPGGrants_RuntimeCannotIssueKeys(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()

	// SELECT is granted: the hot auth path reads this table on every request.
	if err := asRuntime(t, pg, func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM verify.api_callers`).Scan(&n)
	}); err != nil {
		t.Fatalf("verify_app must retain SELECT on api_callers (the hot auth lookup): %v", err)
	}
	denied(t, "INSERT INTO verify.api_callers", asRuntime(t, pg, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO verify.api_callers (caller_id, key_sha256, not_before) VALUES ($1,$2,now())`,
			"caller_escalation_"+must(idgen.NewV7String(time.Now().UTC())), []byte("forged-key"))
		return e
	}))
	// And it cannot re-point an existing key at a different platform user (0013's own concern: that
	// would hand one tenant another tenant's jobs without touching the jobs table at all).
	denied(t, "UPDATE verify.api_callers", asRuntime(t, pg, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE verify.api_callers SET user_id = gen_random_uuid() WHERE false`)
		return e
	}))
}
