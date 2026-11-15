package store

// Tests for migration 0016 (module 1 moves from schema public into schema verify; cp_runtime/cp_admin become verify_app/verify_admin;
// verify_owner owns everything). They live in package store, have no build tag, and RUN whenever TM_TEST_DATABASE_URL is set (they skip only
// when it is unset, like every PG-gated test here); each builds its own throwaway database(s) with testpg.Scratch and needs nothing else.
//
// ROLES ARE CLUSTER-WIDE: every read of cluster-wide role state below happens under testpg.LockMigrations, so a Scratch running in another test
// binary cannot be half-way through renaming a role while it is read.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-verify/control-plane/internal/testpg"
)

// module1Tables is the explicit list 0016 moves (never "everything in public": public is shared with the platform).
var module1Tables = []string{
	"verifications", "verification_raw", "cache", "catchall_domains", "disposable_domains", "spam_trap_domains",
	"nodes", "policies", "send_outcomes", "verification_outcomes", "node_audits", "node_warmup", "rate_budgets",
	"work_items", "jobs", "work_item_attempts", "api_callers", "external_calls", "calibration", "recalibration_runs",
	"outcome_ingest_cursor",
}

// superConn opens a superuser connection to a scratch database (the DSN testpg.Scratch returned). It is separate from any store pool.
func superConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect (superuser) %s: %v", dsn, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func queryStrings(t *testing.T, conn *pgx.Conn, sql string, args ...any) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan %q: %v", sql, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", sql, err)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// (a) nothing of module 1 is left in public; everything in verify is owned by verify_owner
// ─────────────────────────────────────────────────────────────────────────────

// schemaFindings is what a database looks like to the question "did module 1 leave public, and who owns what it left in verify".
type schemaFindings struct {
	PublicRelations []string // any relkind: tables, partitions, indexes, sequences, views, composite types, ...
	PublicFunctions []string // functions, procedures, aggregates (extension members excluded)
	PublicTypes     []string // pg_type rows (a table's row type and array type included), extension members excluded

	VerifySchemaExists bool
	VerifySchemaOwner  string
	NotOwnedByOwner    []string // relations in verify whose owner is not verify_owner (any relkind)
	TypesNotOwned      []string // types in verify whose owner is not verify_owner
	Tables, Partitions int      // relkind 'r' (partitions included) / of which children of verification_raw, as found in verify
	Indexes            int      // relkind 'i' + 'I' in verify
	Sequences          int
	Funcs              map[string]funcFinding // by proname, in verify
}

type funcFinding struct {
	Owner    string
	Definer  bool
	Identity string
}

func collectFindings(t *testing.T, conn *pgx.Conn) schemaFindings {
	t.Helper()
	ctx := context.Background()
	var f schemaFindings
	f.PublicRelations = queryStrings(t, conn,
		`SELECT c.relname || ' (relkind ' || c.relkind::text || ')' FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace ORDER BY 1`)
	f.PublicFunctions = queryStrings(t, conn,
		`SELECT p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')' FROM pg_proc p
		  WHERE p.pronamespace = 'public'::regnamespace
		    AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
		  ORDER BY 1`)
	f.PublicTypes = queryStrings(t, conn,
		`SELECT t.typname FROM pg_type t
		  WHERE t.typnamespace = 'public'::regnamespace
		    AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
		  ORDER BY 1`)

	var owner *string
	if err := conn.QueryRow(ctx,
		`SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = 'verify'`).Scan(&owner); err == nil {
		f.VerifySchemaExists = true
		f.VerifySchemaOwner = *owner
	} else if err != pgx.ErrNoRows {
		t.Fatalf("read schema verify: %v", err)
	}
	if !f.VerifySchemaExists {
		return f
	}
	f.NotOwnedByOwner = queryStrings(t, conn,
		`SELECT c.relname || ' (relkind ' || c.relkind::text || ', owner ' || pg_get_userbyid(c.relowner) || ')' FROM pg_class c
		  WHERE c.relnamespace = 'verify'::regnamespace AND pg_get_userbyid(c.relowner) <> 'verify_owner' ORDER BY 1`)
	f.TypesNotOwned = queryStrings(t, conn,
		`SELECT t.typname || ' (owner ' || pg_get_userbyid(t.typowner) || ')' FROM pg_type t
		  WHERE t.typnamespace = 'verify'::regnamespace AND pg_get_userbyid(t.typowner) <> 'verify_owner' ORDER BY 1`)
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE c.relkind IN ('r','p')),
		       count(*) FILTER (WHERE c.relispartition),
		       count(*) FILTER (WHERE c.relkind IN ('i','I')),
		       count(*) FILTER (WHERE c.relkind = 'S')
		FROM pg_class c WHERE c.relnamespace = 'verify'::regnamespace`).Scan(&f.Tables, &f.Partitions, &f.Indexes, &f.Sequences); err != nil {
		t.Fatalf("count verify objects: %v", err)
	}
	f.Funcs = map[string]funcFinding{}
	rows, err := conn.Query(ctx, `
		SELECT p.proname, pg_get_userbyid(p.proowner), p.prosecdef, pg_get_function_identity_arguments(p.oid)
		FROM pg_proc p WHERE p.pronamespace = 'verify'::regnamespace`)
	if err != nil {
		t.Fatalf("read verify functions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var ff funcFinding
		if err := rows.Scan(&name, &ff.Owner, &ff.Definer, &ff.Identity); err != nil {
			t.Fatalf("scan verify function: %v", err)
		}
		f.Funcs[name] = ff
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("verify functions: %v", err)
	}
	return f
}

// problems lists everything wrong with a database that has been migrated through 0016. The same function judges the control database (through
// 0015), where it MUST find problems: that is what proves the check can fail.
func (f schemaFindings) problems() []string {
	var p []string
	if len(f.PublicRelations) > 0 {
		p = append(p, fmt.Sprintf("public still holds %d relation(s): %s", len(f.PublicRelations), strings.Join(f.PublicRelations, ", ")))
	}
	if len(f.PublicFunctions) > 0 {
		p = append(p, fmt.Sprintf("public still holds %d function(s): %s", len(f.PublicFunctions), strings.Join(f.PublicFunctions, ", ")))
	}
	if len(f.PublicTypes) > 0 {
		p = append(p, fmt.Sprintf("public still holds %d type(s): %s", len(f.PublicTypes), strings.Join(f.PublicTypes, ", ")))
	}
	if !f.VerifySchemaExists {
		p = append(p, "schema verify does not exist")
		return p
	}
	if f.VerifySchemaOwner != "verify_owner" {
		p = append(p, "schema verify is owned by "+f.VerifySchemaOwner+", want verify_owner")
	}
	if len(f.NotOwnedByOwner) > 0 {
		p = append(p, "relations in verify not owned by verify_owner: "+strings.Join(f.NotOwnedByOwner, ", "))
	}
	if len(f.TypesNotOwned) > 0 {
		p = append(p, "types in verify not owned by verify_owner: "+strings.Join(f.TypesNotOwned, ", "))
	}
	for name, want := range map[string]bool{"ensure_verification_raw_partitions": true, "ensure_verification_raw_partitions_at": false} {
		ff, ok := f.Funcs[name]
		switch {
		case !ok:
			p = append(p, "function verify."+name+" does not exist")
		case ff.Owner != "verify_owner":
			p = append(p, fmt.Sprintf("function verify.%s is owned by %s, want verify_owner", name, ff.Owner))
		case ff.Definer != want:
			p = append(p, fmt.Sprintf("function verify.%s SECURITY DEFINER=%v, want %v", name, ff.Definer, want))
		}
	}
	return p
}

// TestVerifySchema_NothingLeftInPublic proves 0016 leaves ZERO relations, functions and types of module 1 in public and that verify is owned by
// verify_owner throughout. It fails if a table, partition, index or function stays in (or returns to) public, or any object in verify has another
// owner. Its paired control runs the same judgement on a database migrated only through 0015, which must be found guilty on every count.
func TestVerifySchema_NothingLeftInPublic(t *testing.T) {
	dsn := testpg.Scratch(t, 0)
	f := collectFindings(t, superConn(t, dsn))
	if probs := f.problems(); len(probs) > 0 {
		t.Errorf("after the full migration chain:\n  %s", strings.Join(probs, "\n  "))
	}

	// The checks above must not pass because nothing was there at all: verify holds what module 1 is made of.
	if f.Tables < len(module1Tables)+3 { // 21 tables + at least the 3 legacy partitions
		t.Errorf("verify holds %d tables+partitions, want at least %d", f.Tables, len(module1Tables)+3)
	}
	if f.Partitions < 3 {
		t.Errorf("verify holds %d partitions of verification_raw, want at least the 3 legacy ones", f.Partitions)
	}
	if f.Indexes < len(module1Tables) {
		t.Errorf("verify holds %d indexes, want at least one per table (%d)", f.Indexes, len(module1Tables))
	}
	if f.Sequences != 0 {
		t.Errorf("verify holds %d sequences; module 1 has none (every id is app-generated) — the ownership assertion covers sequences only if one appears", f.Sequences)
	}
	have := map[string]bool{}
	for _, name := range queryStrings(t, superConn(t, dsn), `SELECT relname FROM pg_class WHERE relnamespace = 'verify'::regnamespace AND relkind IN ('r','p')`) {
		have[name] = true
	}
	for _, name := range module1Tables {
		if !have[name] {
			t.Errorf("table verify.%s does not exist", name)
		}
	}

	// PAIRED CONTROL: the same judgement on a 0001..0015 database. Everything must still be in public, there is no schema verify, and the
	// functions are in public — so problems() must name each of them.
	cdsn := testpg.Scratch(t, 15)
	cf := collectFindings(t, superConn(t, cdsn))
	cprobs := cf.problems()
	joined := strings.Join(cprobs, "\n")
	for _, want := range []string{"public still holds", "verification_raw", "ensure_verification_raw_partitions", "schema verify does not exist"} {
		if !strings.Contains(joined, want) {
			t.Errorf("CONTROL: on a database migrated only through 0015 the check should report %q, got:\n%s", want, joined)
		}
	}
	if len(cf.PublicRelations) < len(module1Tables) || len(cf.PublicFunctions) != 2 || len(cf.PublicTypes) == 0 {
		t.Errorf("CONTROL: through 0015 public should hold >= %d relations, exactly the 2 partition functions and the tables' types; got %d relations, %d functions, %d types",
			len(module1Tables), len(cf.PublicRelations), len(cf.PublicFunctions), len(cf.PublicTypes))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// (b) roles and schema privileges
// ─────────────────────────────────────────────────────────────────────────────

type roleAttrs struct {
	Name                                                       string
	Login, Super, CreateRole, CreateDB, Replication, BypassRLS bool
}

func readRole(t *testing.T, conn *pgx.Conn, name string) (roleAttrs, bool) {
	t.Helper()
	r := roleAttrs{Name: name}
	err := conn.QueryRow(context.Background(),
		`SELECT rolcanlogin, rolsuper, rolcreaterole, rolcreatedb, rolreplication, rolbypassrls FROM pg_roles WHERE rolname = $1`, name).
		Scan(&r.Login, &r.Super, &r.CreateRole, &r.CreateDB, &r.Replication, &r.BypassRLS)
	if err == pgx.ErrNoRows {
		return r, false
	}
	if err != nil {
		t.Fatalf("read role %s: %v", name, err)
	}
	return r, true
}

// TestVerifySchema_RolesAndSchema proves the three roles and the schema ACL: verify_app is the only LOGIN role, is unprivileged and owns
// nothing; verify_owner and verify_admin cannot log in; cp_runtime/cp_admin are gone; PUBLIC cannot even see schema verify; verify_app and
// verify_admin can use it but not create in it. It fails if any attribute drifts, if an old role survives, or if PUBLIC regains USAGE. Control:
// a brand-new role that holds nothing must be REFUSED usage (so the privilege functions can answer false), while verify_app is granted it.
func TestVerifySchema_RolesAndSchema(t *testing.T) {
	dsn := testpg.Scratch(t, 0)
	conn := superConn(t, dsn)

	// Role state is cluster-wide and migrations in other test binaries rename/create/drop roles: read it only while none is running.
	unlock := testpg.LockMigrations(t)
	defer unlock()
	ctx := context.Background()

	app, ok := readRole(t, conn, "verify_app")
	if !ok {
		t.Fatal("role verify_app does not exist")
	}
	if !app.Login {
		t.Error("verify_app must be LOGIN (it is the control plane's runtime identity)")
	}
	if app.Super || app.CreateRole || app.CreateDB || app.Replication || app.BypassRLS {
		t.Errorf("verify_app must be an unprivileged role, got %+v", app)
	}
	for _, name := range []string{"verify_owner", "verify_admin"} {
		r, ok := readRole(t, conn, name)
		if !ok {
			t.Errorf("role %s does not exist", name)
			continue
		}
		if r.Login {
			t.Errorf("%s must be NOLOGIN", name)
		}
		if r.Super || r.CreateRole || r.CreateDB || r.Replication || r.BypassRLS {
			t.Errorf("%s must be an unprivileged role, got %+v", name, r)
		}
	}
	for _, name := range []string{"cp_runtime", "cp_admin"} {
		if _, exists := readRole(t, conn, name); exists {
			t.Errorf("role %s still exists after 0016 (it must have been renamed or dropped)", name)
		}
	}

	// verify_app owns nothing, in any database of the cluster (pg_shdepend 'o' = owner dependency).
	var owned int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_shdepend WHERE refclassid = 'pg_authid'::regclass AND deptype = 'o'
		   AND refobjid = (SELECT oid FROM pg_roles WHERE rolname = 'verify_app')`).Scan(&owned); err != nil {
		t.Fatalf("verify_app ownership: %v", err)
	}
	if owned != 0 {
		t.Errorf("verify_app owns %d object(s); it must own nothing", owned)
	}
	// ... and verify_owner owns the schema, which makes it the only role that can CREATE in it without a grant.
	var schemaOwner string
	if err := conn.QueryRow(ctx, `SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = 'verify'`).Scan(&schemaOwner); err != nil || schemaOwner != "verify_owner" {
		t.Errorf("schema verify owner = %q (err %v), want verify_owner", schemaOwner, err)
	}

	// PUBLIC: no ACL entry for it (grantee 0 = PUBLIC) ...
	var publicEntries int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_namespace n, LATERAL aclexplode(n.nspacl) a WHERE n.nspname = 'verify' AND a.grantee = 0`).Scan(&publicEntries); err != nil {
		t.Fatalf("schema ACL: %v", err)
	}
	if publicEntries != 0 {
		t.Errorf("PUBLIC holds %d privilege(s) on schema verify; it must hold none", publicEntries)
	}
	// ... and behaviourally: a role that is granted nothing sees no USAGE (it would inherit PUBLIC's if there were any). The probe role is
	// cluster-wide, so it is created and dropped inside the migration lock and its name cannot collide with another run.
	probe := "verify_probe_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	probe = probe[:min(len(probe), 60)]
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{probe}.Sanitize()); err != nil {
		t.Fatalf("drop stale probe role: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE ROLE "+pgx.Identifier{probe}.Sanitize()+" NOLOGIN"); err != nil {
		t.Fatalf("create probe role: %v", err)
	}
	defer conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgx.Identifier{probe}.Sanitize()) //nolint:errcheck

	priv := func(role, schema, p string) bool {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT has_schema_privilege($1, $2, $3)`, role, schema, p).Scan(&ok); err != nil {
			t.Fatalf("has_schema_privilege(%s,%s,%s): %v", role, schema, p, err)
		}
		return ok
	}
	if priv(probe, "verify", "USAGE") {
		t.Error("an unrelated role holds USAGE on schema verify: PUBLIC must not have it")
	}
	if !priv("verify_app", "verify", "USAGE") || !priv("verify_admin", "verify", "USAGE") {
		t.Error("verify_app and verify_admin must hold USAGE on schema verify")
	}
	if priv("verify_app", "verify", "CREATE") || priv("verify_admin", "verify", "CREATE") {
		t.Error("neither verify_app nor verify_admin may CREATE in schema verify (DDL belongs to verify_owner)")
	}
	if !priv("verify_owner", "verify", "CREATE") {
		t.Error("verify_owner must be able to CREATE in schema verify")
	}
	// CONTROL (the three-way split above is only evidence if the functions can say both yes and no): the probe role was refused USAGE, verify_app
	// was granted it, and verify_app was refused CREATE while verify_owner was granted it.
}

// ─────────────────────────────────────────────────────────────────────────────
// (c) every privilege is carried over; exactly the declared two are added
// ─────────────────────────────────────────────────────────────────────────────

// privSet is a set of "kind|object|grantee|privilege|grantable" entries. Objects are named as they are AFTER 0016 (verify.<name>); the owner is
// the placeholder <owner> because it changes from the applying superuser to verify_owner; cp_runtime/cp_admin are shown as their new names.
type privSet map[string]bool

func mapRole(name string) string {
	switch name {
	case "cp_runtime":
		return "verify_app"
	case "cp_admin":
		return "verify_admin"
	}
	return name
}

// snapshotPrivileges reads every ACL entry on the module-1 tables (+ every partition of verification_raw), their columns, and the two partition
// functions, from schema `schema` (public before 0016, verify after).
func snapshotPrivileges(t *testing.T, conn *pgx.Conn, schema string) privSet {
	t.Helper()
	ctx := context.Background()
	out := privSet{}
	add := func(kind, object, grantee, priv string, grantable bool) {
		out[fmt.Sprintf("%s|%s|%s|%s|%v", kind, object, mapRole(grantee), priv, grantable)] = true
	}

	rel, err := conn.Query(ctx, `
		WITH rels AS (
		  SELECT c.oid, c.relname, c.relowner, c.relacl FROM pg_class c
		  WHERE c.relnamespace = $1::regnamespace AND c.relkind IN ('r','p')
		    AND (c.relname = ANY($2) OR c.relname LIKE 'verification\_raw\_%')
		)
		SELECT r.relname, CASE WHEN a.grantee = r.relowner THEN '<owner>' ELSE COALESCE(g.rolname, 'PUBLIC') END, a.privilege_type, a.is_grantable
		FROM rels r CROSS JOIN LATERAL aclexplode(r.relacl) a LEFT JOIN pg_roles g ON g.oid = a.grantee`, schema, module1Tables)
	if err != nil {
		t.Fatalf("snapshot tables in %s: %v", schema, err)
	}
	for rel.Next() {
		var name, grantee, priv string
		var grantable bool
		if err := rel.Scan(&name, &grantee, &priv, &grantable); err != nil {
			rel.Close()
			t.Fatalf("scan: %v", err)
		}
		add("table", "verify."+name, grantee, priv, grantable)
	}
	rel.Close()
	if err := rel.Err(); err != nil {
		t.Fatalf("snapshot tables in %s: %v", schema, err)
	}

	col, err := conn.Query(ctx, `
		SELECT c.relname, at.attname, CASE WHEN a.grantee = c.relowner THEN '<owner>' ELSE COALESCE(g.rolname, 'PUBLIC') END, a.privilege_type, a.is_grantable
		FROM pg_class c JOIN pg_attribute at ON at.attrelid = c.oid AND at.attacl IS NOT NULL
		CROSS JOIN LATERAL aclexplode(at.attacl) a LEFT JOIN pg_roles g ON g.oid = a.grantee
		WHERE c.relnamespace = $1::regnamespace AND c.relkind IN ('r','p')`, schema)
	if err != nil {
		t.Fatalf("snapshot columns in %s: %v", schema, err)
	}
	for col.Next() {
		var table, column, grantee, priv string
		var grantable bool
		if err := col.Scan(&table, &column, &grantee, &priv, &grantable); err != nil {
			col.Close()
			t.Fatalf("scan: %v", err)
		}
		add("column", "verify."+table+"."+column, grantee, priv, grantable)
	}
	col.Close()
	if err := col.Err(); err != nil {
		t.Fatalf("snapshot columns in %s: %v", schema, err)
	}

	fn, err := conn.Query(ctx, `
		SELECT p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')',
		       CASE WHEN a.grantee = p.proowner THEN '<owner>' ELSE COALESCE(g.rolname, 'PUBLIC') END, a.privilege_type, a.is_grantable
		FROM pg_proc p CROSS JOIN LATERAL aclexplode(p.proacl) a LEFT JOIN pg_roles g ON g.oid = a.grantee
		WHERE p.pronamespace = $1::regnamespace AND p.proname IN ('ensure_verification_raw_partitions', 'ensure_verification_raw_partitions_at')`, schema)
	if err != nil {
		t.Fatalf("snapshot functions in %s: %v", schema, err)
	}
	for fn.Next() {
		var sig, grantee, priv string
		var grantable bool
		if err := fn.Scan(&sig, &grantee, &priv, &grantable); err != nil {
			fn.Close()
			t.Fatalf("scan: %v", err)
		}
		add("function", "verify."+sig, grantee, priv, grantable)
	}
	fn.Close()
	if err := fn.Err(); err != nil {
		t.Fatalf("snapshot functions in %s: %v", schema, err)
	}
	return out
}

// declaredAdditions is the COMPLETE list of privileges 0016 adds (header of the migration): the runtime role may INSERT policies and flip
// is_default, because EnsurePolicy does both at startup.
var declaredAdditions = []string{
	"table|verify.policies|verify_app|INSERT|false",
	"column|verify.policies.is_default|verify_app|UPDATE|false",
}

// diffPrivs compares before and after. lost = in before, not in after (a privilege 0016 dropped); widened = in after, in neither before nor the
// declared additions (a privilege 0016 invented); additionsMissing = a declared addition that is not there.
func diffPrivs(before, after privSet, additions []string) (lost, widened, additionsMissing []string) {
	declared := map[string]bool{}
	for _, a := range additions {
		declared[a] = true
		if !after[a] {
			additionsMissing = append(additionsMissing, a)
		}
	}
	for e := range before {
		if !after[e] {
			lost = append(lost, e)
		}
	}
	for e := range after {
		if !before[e] && !declared[e] {
			widened = append(widened, e)
		}
	}
	sort.Strings(lost)
	sort.Strings(widened)
	sort.Strings(additionsMissing)
	return
}

// TestDiffPrivs_ReportsLostWidenedAndMissingAdditions is the unit test of the comparator: a dropped entry, an invented one and an absent declared
// addition must each be reported, and an exact match must report nothing. It fails if diffPrivs stops looking in any of the three directions.
func TestDiffPrivs_ReportsLostWidenedAndMissingAdditions(t *testing.T) {
	before := privSet{"table|verify.a|verify_app|SELECT|false": true, "table|verify.a|verify_app|INSERT|false": true}
	add := []string{"table|verify.a|verify_app|DELETE|false"}

	exact := privSet{"table|verify.a|verify_app|SELECT|false": true, "table|verify.a|verify_app|INSERT|false": true, "table|verify.a|verify_app|DELETE|false": true}
	if l, w, m := diffPrivs(before, exact, add); len(l)+len(w)+len(m) != 0 {
		t.Fatalf("an exact match must report nothing, got lost=%v widened=%v missing=%v", l, w, m)
	}
	dropped := privSet{"table|verify.a|verify_app|SELECT|false": true, "table|verify.a|verify_app|DELETE|false": true}
	if l, _, _ := diffPrivs(before, dropped, add); len(l) != 1 || l[0] != "table|verify.a|verify_app|INSERT|false" {
		t.Errorf("a dropped privilege must be reported as lost, got %v", l)
	}
	extra := privSet{"table|verify.a|verify_app|SELECT|false": true, "table|verify.a|verify_app|INSERT|false": true, "table|verify.a|verify_app|DELETE|false": true, "table|verify.a|verify_app|TRUNCATE|false": true}
	if _, w, _ := diffPrivs(before, extra, add); len(w) != 1 || w[0] != "table|verify.a|verify_app|TRUNCATE|false" {
		t.Errorf("an undeclared extra privilege must be reported as widened, got %v", w)
	}
	if _, _, m := diffPrivs(before, before, add); len(m) != 1 {
		t.Errorf("a declared addition that is absent must be reported, got %v", m)
	}
}

// TestVerifySchema_PrivilegesCarriedOver snapshots every ACL entry on the 21 tables, their partitions and the two partition functions at 0015
// (in public, held by cp_runtime/cp_admin), applies 0016, snapshots again (in verify, held by verify_app/verify_admin) and requires after ==
// before + exactly the two declared additions. It fails if 0016 drops a grant (Door 2's append-only REVOKEs included, as a widening), widens
// one, or omits an addition. Control (a second database): REVOKE one grant and GRANT one extra after 0016 — diffPrivs must report both.
func TestVerifySchema_PrivilegesCarriedOver(t *testing.T) {
	dsn := testpg.Scratch(t, 15)
	conn := superConn(t, dsn)
	before := snapshotPrivileges(t, conn, "public")

	// The snapshot must hold the real thing, not an empty set that trivially "carries over".
	tables := map[string]bool{}
	for e := range before {
		if parts := strings.Split(e, "|"); parts[0] == "table" {
			tables[parts[1]] = true
		}
	}
	if len(tables) < len(module1Tables)+3 {
		t.Fatalf("before-snapshot covers %d tables, want >= %d (21 + the legacy partitions)", len(tables), len(module1Tables)+3)
	}
	for _, must := range []string{
		"table|verify.cache|verify_app|DELETE|false",         // the one DELETE the runtime holds
		"table|verify.verifications|verify_app|INSERT|false", // the append-only LOG
		"table|verify.verifications|verify_admin|TRUNCATE|false",
		"function|verify.ensure_verification_raw_partitions(p_months_ahead integer)|verify_app|EXECUTE|false",
	} {
		if !before[must] {
			t.Fatalf("before-snapshot lacks %q; the snapshot is not reading what it claims to", must)
		}
	}
	if before["table|verify.verifications|verify_app|UPDATE|false"] || before["table|verify.verifications|verify_app|DELETE|false"] {
		t.Fatal("before-snapshot says the runtime role can UPDATE/DELETE the LOG — the premise of Door 2 is already broken at 0015")
	}

	testpg.Apply(t, dsn, 16)
	after := snapshotPrivileges(t, conn, "verify")
	lost, widened, missing := diffPrivs(before, after, declaredAdditions)
	if len(lost) > 0 {
		t.Errorf("0016 DROPPED %d privilege(s): %v", len(lost), lost)
	}
	if len(widened) > 0 {
		t.Errorf("0016 ADDED %d undeclared privilege(s): %v", len(widened), widened)
	}
	if len(missing) > 0 {
		t.Errorf("0016 did not add the declared privilege(s): %v", missing)
	}
	if len(after) != len(before)+len(declaredAdditions) {
		t.Errorf("after-snapshot has %d entries, want before(%d)+%d", len(after), len(before), len(declaredAdditions))
	}

	// PAIRED CONTROL: break the migrated database two ways and require the comparison to say so.
	cdsn := testpg.Scratch(t, 15)
	cconn := superConn(t, cdsn)
	cbefore := snapshotPrivileges(t, cconn, "public")
	testpg.Apply(t, cdsn, 16)
	ctx := context.Background()
	if _, err := cconn.Exec(ctx, `REVOKE SELECT ON verify.cache FROM verify_app`); err != nil {
		t.Fatalf("control revoke: %v", err)
	}
	clost, cwidened, _ := diffPrivs(cbefore, snapshotPrivileges(t, cconn, "verify"), declaredAdditions)
	if len(clost) != 1 || clost[0] != "table|verify.cache|verify_app|SELECT|false" || len(cwidened) != 0 {
		t.Errorf("CONTROL: after REVOKE SELECT ON verify.cache the comparison must report exactly that entry as lost, got lost=%v widened=%v", clost, cwidened)
	}
	if _, err := cconn.Exec(ctx, `GRANT DELETE ON verify.verifications TO verify_app`); err != nil {
		t.Fatalf("control grant: %v", err)
	}
	clost, cwidened, _ = diffPrivs(cbefore, snapshotPrivileges(t, cconn, "verify"), declaredAdditions)
	if len(cwidened) != 1 || cwidened[0] != "table|verify.verifications|verify_app|DELETE|false" || len(clost) != 1 {
		t.Errorf("CONTROL: after GRANT DELETE ON verify.verifications the comparison must report it as widened (plus the earlier loss), got lost=%v widened=%v", clost, cwidened)
	}
}
