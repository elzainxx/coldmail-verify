package migrate

// The catalog fingerprint: one hash over everything about schemas verify and public that a migration can change, built so that "a fresh install ends in
// the same state as an upgraded one" is a comparison of two values and not an opinion. It is computed from the system catalogs, never from the migration
// files, so it cannot agree with itself by construction.
//
// WHAT IT COVERS, for the schemas verify and public (objects that belong to an extension are left out):
//   - the schemas: owner and ACL
//   - every relation: kind (table, partition, index, ...), owner, partition bound and key, persistence
//   - every column: position, name, type, nullability, default, generated/identity, and every column-level ACL entry (0016 grants UPDATE (is_default))
//   - every constraint (definition, validated, deferrable) and every index definition
//   - every relation ACL entry: grantee, privilege, grantable (a NULL ACL is expanded to its default, so "no ACL" and "the default ACL" are one thing)
//   - every function: owner, kind, SECURITY DEFINER, volatility, md5 of pg_get_functiondef, and every ACL entry
// And, as a SECOND hash, the tracker's (version, checksum) pairs. The tracker's volatile columns (applied_at, applied_by) and `how` are not part of it:
// they differ between a database built from nothing ('applied') and one that was adopted, by design, and say nothing about the schema.
//
// Row DATA is not fingerprinted (a table's contents are not its schema); the tests that care about data read it directly.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type fingerprint struct {
	Catalog      string   // hash over Facts
	Tracker      string   // hash over TrackerFacts
	Facts        []string // sorted catalog facts, one line each
	TrackerFacts []string // sorted "tracker|version|checksum"
	Objects      int      // relations (tables, partitions, indexes) + functions among the facts
}

const (
	notExtRel  = `NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')`
	notExtProc = `NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')`
	grantee    = `(CASE x.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(x.grantee) END)`
	inSchemas  = `$1::text[]`
)

// catalogQueries each return one text column: one fact per row.
var catalogQueries = []struct{ name, sql string }{
	{"schema", `SELECT 'schema|' || n.nspname || '|owner=' || pg_get_userbyid(n.nspowner)
	              FROM pg_namespace n WHERE n.nspname::text = ANY(` + inSchemas + `)`},
	{"schema acl", `SELECT 'schemaacl|' || n.nspname || '|' || ` + grantee + ` || '|' || x.privilege_type || '|' || x.is_grantable::text
	              FROM pg_namespace n CROSS JOIN LATERAL aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) x
	             WHERE n.nspname::text = ANY(` + inSchemas + `)`},
	{"relation", `SELECT 'rel|' || n.nspname || '.' || c.relname || '|kind=' || c.relkind::text || '|owner=' || pg_get_userbyid(c.relowner)
	                    || '|partition=' || c.relispartition::text || '|bound=' || COALESCE(pg_get_expr(c.relpartbound, c.oid), '')
	                    || '|partkey=' || (CASE WHEN c.relkind = 'p' THEN pg_get_partkeydef(c.oid) ELSE '' END)
	                    || '|persistence=' || c.relpersistence::text
	              FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	             WHERE n.nspname::text = ANY(` + inSchemas + `) AND ` + notExtRel},
	{"column", `SELECT 'col|' || n.nspname || '.' || c.relname || '|pos=' || (row_number() OVER (PARTITION BY c.oid ORDER BY a.attnum))::text
	                  || '|' || a.attname || '|type=' || format_type(a.atttypid, a.atttypmod) || '|notnull=' || a.attnotnull::text
	                  || '|default=' || COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
	                  || '|generated=' || COALESCE(NULLIF(a.attgenerated::text, ''), '-') || '|identity=' || COALESCE(NULLIF(a.attidentity::text, ''), '-')
	              FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	              JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
	              LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
	             WHERE n.nspname::text = ANY(` + inSchemas + `) AND c.relkind IN ('r', 'p', 'v', 'm', 'f') AND ` + notExtRel},
	{"column acl", `SELECT 'colacl|' || n.nspname || '.' || c.relname || '.' || a.attname || '|' || ` + grantee + ` || '|' || x.privilege_type || '|' || x.is_grantable::text
	              FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	              JOIN pg_attribute a ON a.attrelid = c.oid AND a.attacl IS NOT NULL AND NOT a.attisdropped
	              CROSS JOIN LATERAL aclexplode(a.attacl) x
	             WHERE n.nspname::text = ANY(` + inSchemas + `) AND c.relkind IN ('r', 'p', 'v', 'm', 'f') AND ` + notExtRel},
	{"constraint", `SELECT 'con|' || n.nspname || '.' || c.relname || '|' || k.conname || '|type=' || k.contype::text || '|' || pg_get_constraintdef(k.oid, true)
	                      || '|validated=' || k.convalidated::text || '|deferrable=' || k.condeferrable::text || '|deferred=' || k.condeferred::text
	              FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
	             WHERE n.nspname::text = ANY(` + inSchemas + `) AND ` + notExtRel},
	{"index", `SELECT 'idx|' || schemaname || '.' || indexname || '|' || indexdef FROM pg_indexes WHERE schemaname::text = ANY(` + inSchemas + `)`},
	{"relation acl", `SELECT 'acl|' || n.nspname || '.' || c.relname || '|' || ` + grantee + ` || '|' || x.privilege_type || '|' || x.is_grantable::text
	              FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	              CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl, acldefault((CASE WHEN c.relkind = 'S' THEN 's' ELSE 'r' END)::"char", c.relowner))) x
	             WHERE n.nspname::text = ANY(` + inSchemas + `) AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S') AND ` + notExtRel},
	{"function", `SELECT 'fn|' || n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')|owner=' || pg_get_userbyid(p.proowner)
	                    || '|kind=' || p.prokind::text || '|secdef=' || p.prosecdef::text || '|volatility=' || p.provolatile::text
	                    || '|md5=' || md5(pg_get_functiondef(p.oid))
	              FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
	             WHERE n.nspname::text = ANY(` + inSchemas + `) AND p.prokind IN ('f', 'p') AND ` + notExtProc},
	{"function acl", `SELECT 'fnacl|' || n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')|' || ` + grantee + ` || '|' || x.privilege_type || '|' || x.is_grantable::text
	              FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
	              CROSS JOIN LATERAL aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) x
	             WHERE n.nspname::text = ANY(` + inSchemas + `) AND p.prokind IN ('f', 'p') AND ` + notExtProc},
}

func queryFacts(ctx context.Context, conn *pgx.Conn, sql string, args ...any) ([]string, error) {
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// fingerprintOf reads the fingerprint of the database at dsn. It fails the test on any error, and on a fingerprint of zero objects: a hash of nothing
// would let two empty databases "agree".
func fingerprintOf(t testing.TB, dsn string) fingerprint {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("fingerprint: connect: %v", err)
	}
	defer conn.Close(ctx)
	// Pinned so the text of partition bounds, defaults and definitions cannot depend on the server's or the role's settings.
	for _, s := range []string{"SET TIME ZONE 'UTC'", "SET search_path = pg_catalog"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("fingerprint: %s: %v", s, err)
		}
	}
	var fp fingerprint
	for _, q := range catalogQueries {
		facts, err := queryFacts(ctx, conn, q.sql, []string{"verify", "public"})
		if err != nil {
			t.Fatalf("fingerprint: %s query: %v", q.name, err)
		}
		fp.Facts = append(fp.Facts, facts...)
	}
	sort.Strings(fp.Facts)
	var tracked bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('verify.schema_migrations') IS NOT NULL`).Scan(&tracked); err != nil {
		t.Fatalf("fingerprint: tracker exists: %v", err)
	}
	if tracked {
		if fp.TrackerFacts, err = queryFacts(ctx, conn, `SELECT 'tracker|' || version || '|' || checksum FROM verify.schema_migrations`); err != nil {
			t.Fatalf("fingerprint: tracker rows: %v", err)
		}
		sort.Strings(fp.TrackerFacts)
	}
	for _, f := range fp.Facts {
		if strings.HasPrefix(f, "rel|") || strings.HasPrefix(f, "fn|") {
			fp.Objects++
		}
	}
	fp.Catalog = hashFacts(fp.Facts)
	fp.Tracker = hashFacts(fp.TrackerFacts)

	// Anti-vacuity. module 1 is 26+ tables, partitions and their indexes plus 2 functions, and several hundred columns, constraints and ACL entries.
	if fp.Objects < 40 || len(fp.Facts) < 300 {
		t.Fatalf("fingerprint of %s covers only %d object(s) and %d fact(s): a hash of (almost) nothing proves nothing", redactDB(dsn), fp.Objects, len(fp.Facts))
	}
	return fp
}

func hashFacts(facts []string) string {
	sum := sha256.Sum256([]byte(strings.Join(facts, "\n")))
	return hex.EncodeToString(sum[:])
}

// redactDB names a scratch database in a message without its credentials.
func redactDB(dsn string) string {
	if i := strings.LastIndex(dsn, "/"); i >= 0 {
		return dsn[i+1:]
	}
	return "database"
}

// diffFacts returns what is only in a, and what is only in b.
func diffFacts(a, b []string) (onlyA, onlyB []string) {
	inA, inB := map[string]bool{}, map[string]bool{}
	for _, s := range a {
		inA[s] = true
	}
	for _, s := range b {
		inB[s] = true
	}
	for _, s := range a {
		if !inB[s] {
			onlyA = append(onlyA, s)
		}
	}
	for _, s := range b {
		if !inA[s] {
			onlyB = append(onlyB, s)
		}
	}
	return
}

// describeDiff renders the difference between two fingerprints for a failure message (first lines only).
func describeDiff(an, bn string, a, b fingerprint) string {
	var sb strings.Builder
	emit := func(prefix string, lines []string) {
		for i, l := range lines {
			if i == 25 {
				fmt.Fprintf(&sb, "  ... and %d more\n", len(lines)-25)
				return
			}
			fmt.Fprintf(&sb, "  %s %s\n", prefix, l)
		}
	}
	oa, ob := diffFacts(a.Facts, b.Facts)
	fmt.Fprintf(&sb, "catalog: %d fact(s) only in %s, %d only in %s\n", len(oa), an, len(ob), bn)
	emit("-", oa)
	emit("+", ob)
	ta, tb := diffFacts(a.TrackerFacts, b.TrackerFacts)
	fmt.Fprintf(&sb, "tracker: %d only in %s, %d only in %s\n", len(ta), an, len(tb), bn)
	emit("-", ta)
	emit("+", tb)
	return sb.String()
}

// requireSameCatalog fails with the named difference unless the two catalogs agree.
func requireSameCatalog(t testing.TB, an, bn string, a, b fingerprint) {
	t.Helper()
	if a.Catalog != b.Catalog {
		t.Errorf("the catalogs of %s and %s differ:\n%s", an, bn, describeDiff(an, bn, a, b))
	}
}

// requireSameTracker fails unless the two trackers hold the same (version, checksum) pairs.
func requireSameTracker(t testing.TB, an, bn string, a, b fingerprint) {
	t.Helper()
	if a.Tracker != b.Tracker {
		t.Errorf("the trackers of %s and %s differ:\n%s", an, bn, describeDiff(an, bn, a, b))
	}
}

// trackerRow is one verify.schema_migrations row with everything that identifies it as the SAME row: its transaction id (xmin) proves it was not rewritten.
type trackerRow struct {
	Version, Checksum, How, AppliedBy, Xmin string
	AppliedAtMicro                          int64
}

func trackerSnapshot(t testing.TB, dsn string) []trackerRow {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("tracker snapshot: connect: %v", err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT version, checksum, how, applied_by, applied_at, xmin::text FROM verify.schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("tracker snapshot: %v", err)
	}
	defer rows.Close()
	var out []trackerRow
	for rows.Next() {
		var r trackerRow
		var at time.Time
		if err := rows.Scan(&r.Version, &r.Checksum, &r.How, &r.AppliedBy, &at, &r.Xmin); err != nil {
			t.Fatalf("tracker snapshot: %v", err)
		}
		r.AppliedAtMicro = at.UnixMicro()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("tracker snapshot: %v", err)
	}
	return out
}

// publicLeftovers lists every relation and every function in schema public: in a scratch database that is whatever module 1 left there (the platform's
// own tables live in the platform's database, not here), so after a complete migration it must be empty.
func publicLeftovers(t testing.TB, dsn string) []string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	out, err := queryFacts(ctx, conn, `
		SELECT 'relation ' || c.relname FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace
		UNION ALL
		SELECT 'function ' || p.proname FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("read public: %v", err)
	}
	return out
}
