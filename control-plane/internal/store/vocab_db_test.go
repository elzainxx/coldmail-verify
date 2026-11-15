package store

// The vocabulary's own comment says "Storage-layer CHECK constraints (P1 §4.1) must mirror these; adding a value = update here + an
// additive CHECK migration" — and until ADR-0021 nothing verified it. A reason the code can emit but the database rejects fails at the
// worst moment (an INSERT into the append-only log, after the SMTP session has already happened). This reads the CHECK definitions
// back from Postgres and requires them to equal vocab.Reasons(), for BOTH tables that carry a reason. Runs in a throwaway database
// (testpg.Scratch) and SKIPS honestly without TM_TEST_DATABASE_URL, like every PG-gated test here.

import (
	"context"
	"regexp"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-verify/control-plane/internal/testpg"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

var quotedText = regexp.MustCompile(`'([a-z_]+)'::text`)

func checkReasons(t *testing.T, dsn, table string) []string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	var def string
	if err := conn.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'valid_reason' AND conrelid = $1::regclass`, table).Scan(&def); err != nil {
		t.Fatalf("read valid_reason on %s: %v", table, err)
	}
	var out []string
	for _, m := range quotedText.FindAllStringSubmatch(def, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func vocabReasons() []string {
	var out []string
	for _, r := range vocab.Reasons() {
		out = append(out, string(r))
	}
	sort.Strings(out)
	return out
}

func diff(a, b []string) (onlyA, onlyB []string) {
	in := func(s []string) map[string]bool {
		m := map[string]bool{}
		for _, x := range s {
			m[x] = true
		}
		return m
	}
	ma, mb := in(a), in(b)
	for _, x := range a {
		if !mb[x] {
			onlyA = append(onlyA, x)
		}
	}
	for _, x := range b {
		if !ma[x] {
			onlyB = append(onlyB, x)
		}
	}
	return
}

func TestVocabMirrorsTheDatabaseCheckConstraints(t *testing.T) {
	dsn := testpg.Scratch(t, 0) // every migration; since 0016 the tables are in schema verify
	for _, table := range []string{"verify.verifications", "verify.cache"} {
		inDB := checkReasons(t, dsn, table)
		onlyVocab, onlyDB := diff(vocabReasons(), inDB)
		if len(onlyVocab) > 0 || len(onlyDB) > 0 {
			t.Errorf("%s.valid_reason drifted from vocab.Reasons(): in the code but REJECTED by the database: %v; in the database but not in the code: %v", table, onlyVocab, onlyDB)
		}
	}
}

// CONTROL: the check can fail. Through migration 0014 — before ADR-0021 — the database does NOT know non_public_mx, so the mirror above
// would have been red; 0015 is exactly what turns it green.
func TestVocabMirror_ControlBeforeMigration0015TheReasonIsRejected(t *testing.T) {
	dsn := testpg.Scratch(t, 14) // a real 0014-state database: 0016 has not run, so the tables are still in public
	for _, table := range []string{"public.verifications", "public.cache"} {
		onlyVocab, _ := diff(vocabReasons(), checkReasons(t, dsn, table))
		if len(onlyVocab) != 1 || onlyVocab[0] != string(vocab.ReasonNonPublicMX) {
			t.Fatalf("CONTROL FAILED: through 0014, %s should reject exactly non_public_mx, got %v — the mirror check would not detect a missing migration", table, onlyVocab)
		}
	}
}
