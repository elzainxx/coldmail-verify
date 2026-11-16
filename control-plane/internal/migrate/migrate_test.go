package migrate

// Tests of the runner's decisions that need no database: reading and validating the migration directory, and turning (files, database state) into a
// plan or a refusal. They run everywhere. The tests that need Postgres (a real fresh install, a real upgrade, the lock, a failing file) are in
// migrate_db_test.go and fingerprint_test.go and run whenever TM_TEST_DATABASE_URL is set, which CI always does.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/testpg"
)

// synth builds n synthetic migration files 0001..n.
func synth(n int) []File {
	files := make([]File, n)
	for i := range files {
		v := fmt.Sprintf("%04d", i+1)
		sql := "-- synthetic " + v + "\n"
		sum := sha256.Sum256([]byte(sql))
		files[i] = File{Version: v, Name: v + "_synthetic.sql", Checksum: hex.EncodeToString(sum[:]), SQL: sql}
	}
	return files
}

func writeDir(t *testing.T, names map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func asRefusal(t *testing.T, err error) *RefusalError {
	t.Helper()
	var r *RefusalError
	if !errors.As(err, &r) {
		t.Fatalf("want a *RefusalError, got %T: %v", err, err)
	}
	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// the migration directory
// ─────────────────────────────────────────────────────────────────────────────

// frozen is the sha256 of migrations 0001..0016 as released (CRLF normalised, as the runner hashes them). Migrations are forward-only: a recorded
// checksum that differs from the file on disk makes the runner REFUSE on every database that already ran it, so editing one of these files is not a
// fix, it is an outage with a delay. This fails the moment a byte changes, before any database sees it. A new migration adds a file; it never edits one.
var frozen = map[string]string{
	"0001": "1f171dc7b7882a5e701a91f8e2a08e6a4bd7e56d12c61ef0660d52ac9d5a1a4f",
	"0002": "a374d225d4fd2c94b4f166e48e8b467ad48f82af0c54cb7cc4f2a7c2986e09ca",
	"0003": "8a80f5cd8119bab921d98eb11d9b9bd0102ebbc0e1e951026b23f34826389b62",
	"0004": "3754dd15cfc8746b00f6b2b64f30ff1c29544b811367079ac165e7a2d4e21b80",
	"0005": "b12f4b77aff71372c0e126d1e0863c5a4a0a7216b416887f779fc13fe322ecf8",
	"0006": "e9328a95941b11d2b148ce6463b0cd162283551f3807fab3c98ed0dbd9beb6b0",
	"0007": "218e905096986abe1813a1e9cbc5896bb9d34ef8823b5b7d9d4e6a406bf78786",
	"0008": "7feb453df93978b5d2b68a35f07485dc5aeff40c8e4473fc864125e5d97a4f88",
	"0009": "a38534be7e53e677616b84a0067d85dd52b92e259ac1e84ebf92dc5916c9d192",
	"0010": "a162d0a5e4cc832a3c09824b8a2c32d692638f2fc51ea8106fb3ed5437efc027",
	"0011": "a2435a1e419dc6b671c78abf6092549ede9a8af0cc9d09cb1bb88fa64e8dad97",
	"0012": "833f923e8a1cc9d0451c456e50269038ff1b2d1c189add4c65e7fd471e1a36e2",
	"0013": "aba6caad0f8634262e64524e7fa83394bcae0c251f3caab990b2d9ffac117bd9",
	"0014": "cac68e65a69d01fd5a294c97bcd8c861e2f09f16097c10d00e8e15f4a701066a",
	"0015": "818ff015670ee97165ad264ff8773eb5af7d4fc6cbfd1debcfa834a241cec653",
	"0016": "3c4092d8dc6665393bbad112a58fdbcc7a2728c9483b27b92bb254e259f25079",
}

// frozenViolations lists every released migration that is edited or gone.
func frozenViolations(files []File) []string {
	var out []string
	have := map[string]File{}
	for _, f := range files {
		have[f.Version] = f
	}
	for v, want := range frozen {
		f, ok := have[v]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("migration %s, released and pinned, is gone", v))
		case f.Checksum != want:
			out = append(out, fmt.Sprintf("%s was EDITED after release: sha256 %s, released as %s. Migrations are forward-only (never edit 0001..0016): revert the edit and put the change in a new migration", f.Name, f.Checksum, want))
		}
	}
	sort.Strings(out)
	return out
}

// TestLoadFiles_RealDirectory loads the repository's own migrations: a contiguous sequence from 0001, the tracker migration at its constant, valid
// checksums, and 0001..0016 byte-for-byte as released. It fails if a file is edited, renamed, renumbered or removed.
func TestLoadFiles_RealDirectory(t *testing.T) {
	files, err := LoadFiles(testpg.MigrationsDir())
	if err != nil {
		t.Fatalf("LoadFiles(%s): %v", testpg.MigrationsDir(), err)
	}
	if len(files) < 17 {
		t.Fatalf("found %d migration files, want at least 17 (0001..0017): an empty or truncated directory proves nothing", len(files))
	}
	for i, f := range files {
		if want := fmt.Sprintf("%04d", i+1); f.Version != want {
			t.Errorf("file %d is migration %s, want %s (the numbering must be contiguous from 0001)", i, f.Version, want)
		}
		if len(f.Checksum) != 64 || strings.Trim(f.Checksum, "0123456789abcdef") != "" {
			t.Errorf("%s: checksum %q is not 64 lowercase hex digits (the tracker's CHECK would reject it)", f.Name, f.Checksum)
		}
		if strings.Contains(f.SQL, "\r") {
			t.Errorf("%s: SQL still holds a carriage return after normalisation", f.Name)
		}
		raw, err := os.ReadFile(filepath.Join(testpg.MigrationsDir(), f.Name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(strings.ReplaceAll(string(raw), "\r\n", "\n")))
		if got := hex.EncodeToString(sum[:]); got != f.Checksum {
			t.Errorf("%s: LoadFiles says %s, an independent sha256 of the CRLF-normalised bytes says %s", f.Name, f.Checksum, got)
		}
	}
	for _, v := range frozenViolations(files) {
		t.Error(v)
	}
	if files[16].Version != TrackerVersion || files[16].Name != "0017_schema_migrations.sql" {
		t.Errorf("file 17 is %s (%s): the runner's TrackerVersion constant %s must name the migration that creates verify.schema_migrations", files[16].Name, files[16].Version, TrackerVersion)
	}
	// 0017 must say what the runner relies on.
	for _, want := range []string{"verify.schema_migrations", "OWNER TO verify_owner", "GRANT SELECT ON verify.schema_migrations TO verify_admin", "IF NOT EXISTS"} {
		if !strings.Contains(files[16].SQL, want) {
			t.Errorf("%s does not contain %q", files[16].Name, want)
		}
	}
	// PAIRED CONTROL for the freeze check: the same comparison over a copy of the directory with one byte of 0003 changed and 0009 removed must fail
	// and name both.
	dir := copyMigrations(t)
	b, err := os.ReadFile(filepath.Join(dir, files[2].Name))
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0x01
	if err := os.WriteFile(filepath.Join(dir, files[2].Name), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, files[8].Name)); err != nil {
		t.Fatal(err)
	}
	// Removing 0009 leaves a gap, which LoadFiles refuses; the freeze check needs the files it can still read, so hash them directly.
	var mutated []File
	for _, f := range files {
		if f.Version == "0009" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, f.Name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(strings.ReplaceAll(string(raw), "\r\n", "\n")))
		mutated = append(mutated, File{Version: f.Version, Name: f.Name, Checksum: hex.EncodeToString(sum[:])})
	}
	got := strings.Join(frozenViolations(mutated), "\n")
	if !strings.Contains(got, files[2].Name+" was EDITED") || !strings.Contains(got, "migration 0009, released and pinned, is gone") {
		t.Errorf("CONTROL: the freeze check did not name the edited 0003 and the missing 0009: %q", got)
	}
}

// copyMigrations copies the repository's *.sql migrations into a fresh temporary directory, for tests that must change one.
func copyMigrations(t testing.TB) string {
	t.Helper()
	src, dst := testpg.MigrationsDir(), t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	copied := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
		copied++
	}
	if copied < 17 {
		t.Fatalf("copied only %d migration files from %s", copied, src)
	}
	return dst
}

// TestLoadFiles_Refusals feeds LoadFiles directories it must not act on: each is refused with a message that names the problem, and a valid directory
// (the control) is accepted.
func TestLoadFiles_Refusals(t *testing.T) {
	ok := "SELECT 1;\n"
	cases := []struct {
		name  string
		files map[string]string
		want  []string // every one must appear in the refusal
	}{
		{"zero files", map[string]string{}, []string{"no migration files"}},
		{"only a README", map[string]string{"README.md": "x"}, []string{"no migration files"}},
		{"a gap", map[string]string{"0001_a.sql": ok, "0002_b.sql": ok, "0004_d.sql": ok}, []string{"migration 0003 is missing", "0004"}},
		{"does not start at 0001", map[string]string{"0002_b.sql": ok, "0003_c.sql": ok}, []string{"migration 0001 is missing"}},
		{"a duplicate number", map[string]string{"0001_a.sql": ok, "0002_b.sql": ok, "0002_c.sql": ok}, []string{"migration 0002 has 2 files", "0002_b.sql", "0002_c.sql"}},
		{"a stray sql file", map[string]string{"0001_a.sql": ok, "0002-b.sql": ok}, []string{"0002-b.sql", "never run"}},
		{"a stray sql file with no number", map[string]string{"0001_a.sql": ok, "extra.sql": ok}, []string{"extra.sql", "never run"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadFiles(writeDir(t, c.files))
			if err == nil {
				t.Fatal("LoadFiles accepted a directory it must refuse")
			}
			msg := asRefusal(t, err).Error()
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("refusal %q does not contain %q", msg, w)
				}
			}
		})
	}
	// CONTROL: the same helper, a valid directory.
	files, err := LoadFiles(writeDir(t, map[string]string{"0001_a.sql": ok, "0002_b.sql": ok, "README.md": "ignored", "notes.txt": "ignored"}))
	if err != nil || len(files) != 2 || files[0].Version != "0001" || files[1].Name != "0002_b.sql" {
		t.Fatalf("CONTROL: a valid directory was not accepted as such: %v %+v", err, files)
	}
	// A missing directory is an error, not a refusal about the numbering.
	if _, err := LoadFiles(filepath.Join(t.TempDir(), "nope")); err == nil || errors.As(err, new(*RefusalError)) {
		t.Errorf("a missing directory must be a plain error, got %v", err)
	}
}

// TestLoadFiles_CRLFIsNormalised pins the checksum rule: the same migration checked out with CRLF (a Windows tree with autocrlf) and with LF hashes to
// the same value and executes the same text, and any other byte changes the hash.
func TestLoadFiles_CRLFIsNormalised(t *testing.T) {
	lf := "CREATE TABLE a (x int);\n-- comment\nSELECT 1;\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	a, err := LoadFiles(writeDir(t, map[string]string{"0001_a.sql": lf}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadFiles(writeDir(t, map[string]string{"0001_a.sql": crlf}))
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Checksum != b[0].Checksum || a[0].SQL != b[0].SQL || a[0].SQL != lf {
		t.Errorf("CRLF and LF checkouts must hash and run identically: %s vs %s, %q vs %q", a[0].Checksum, b[0].Checksum, a[0].SQL, b[0].SQL)
	}
	c, err := LoadFiles(writeDir(t, map[string]string{"0001_a.sql": strings.Replace(lf, "int", "bigint", 1)}))
	if err != nil {
		t.Fatal(err)
	}
	if c[0].Checksum == a[0].Checksum {
		t.Error("CONTROL: a different file hashed to the same checksum")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the plan
// ─────────────────────────────────────────────────────────────────────────────

func recordedRows(files []File, how string) []row { return rowsFor(files, how) }

// replaysBefore0017 lists the operations of a plan that EXECUTE a migration numbered below 0017.
func replaysBefore0017(p plan) []string {
	var out []string
	for _, o := range p.Ops {
		if o.Run && o.File.Version < TrackerVersion {
			out = append(out, o.File.Version)
		}
	}
	return out
}

// TestBuildPlan_Routes pins what each starting state turns into, with the counts the summary line reports.
func TestBuildPlan_Routes(t *testing.T) {
	files := synth(17)

	t.Run("(d) an empty database: every file in order, tracker rows only with 0017", func(t *testing.T) {
		p, err := buildPlan(files, dbState{})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Ops) != 17 {
			t.Fatalf("%d ops, want 17", len(p.Ops))
		}
		for i, o := range p.Ops {
			if !o.Run || o.File.Version != files[i].Version {
				t.Errorf("op %d = %+v, want a run of %s", i, o, files[i].Version)
			}
			if i < 16 && len(o.Rows) != 0 {
				t.Errorf("op %d (%s) records %d rows: the tracker table does not exist yet", i, o.File.Version, len(o.Rows))
			}
		}
		last := p.Ops[16]
		if len(last.Rows) != 17 {
			t.Fatalf("0017 records %d rows, want 17 (0001..0017)", len(last.Rows))
		}
		for i, r := range last.Rows {
			if r.Version != files[i].Version || r.Checksum != files[i].Checksum || r.How != "applied" {
				t.Errorf("row %d = %+v, want %s applied with that file's checksum", i, r, files[i].Version)
			}
		}
		if p.AlreadyApplied != 0 {
			t.Errorf("AlreadyApplied = %d", p.AlreadyApplied)
		}
	})

	t.Run("(c) the old loop ran 0016: run 0017 only, adopt 0001..0016", func(t *testing.T) {
		p, err := buildPlan(files, dbState{VerifyExists: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Ops) != 1 || !p.Ops[0].Run || p.Ops[0].File.Version != "0017" {
			t.Fatalf("plan = %+v, want exactly one op that runs 0017", p.Ops)
		}
		rows := p.Ops[0].Rows
		if len(rows) != 17 {
			t.Fatalf("%d rows, want 17", len(rows))
		}
		for i, r := range rows {
			want := "adopted"
			if i == 16 {
				want = "applied"
			}
			if r.How != want || r.Version != files[i].Version {
				t.Errorf("row %d = %+v, want %s %s", i, r, files[i].Version, want)
			}
		}
	})

	t.Run("(b) the old loop took the database through 0017: an empty tracker is adopted, nothing runs", func(t *testing.T) {
		p, err := buildPlan(files, dbState{TrackerExists: true, VerifyExists: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Ops) != 1 || p.Ops[0].Run || len(p.Ops[0].Rows) != 17 {
			t.Fatalf("plan = %+v, want one adoption of 17 rows that runs nothing", p.Ops)
		}
		for _, r := range p.Ops[0].Rows {
			if r.How != "adopted" {
				t.Errorf("row %+v must be adopted", r)
			}
		}
	})

	t.Run("(a) tracked and complete: nothing to do", func(t *testing.T) {
		p, err := buildPlan(files, dbState{TrackerExists: true, VerifyExists: true, Rows: recordedRows(files, "applied")})
		if err != nil || len(p.Ops) != 0 || p.AlreadyApplied != 17 {
			t.Fatalf("plan = %+v err = %v, want no ops and 17 already applied", p, err)
		}
	})

	t.Run("(a) tracked, with three newer files: exactly those, in order, each recorded", func(t *testing.T) {
		more := synth(20)
		p, err := buildPlan(more, dbState{TrackerExists: true, VerifyExists: true, Rows: recordedRows(more[:17], "adopted")})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Ops) != 3 || p.AlreadyApplied != 17 {
			t.Fatalf("plan = %+v, want 3 ops after 17 already applied", p)
		}
		for i, o := range p.Ops {
			if want := fmt.Sprintf("%04d", 18+i); !o.Run || o.File.Version != want || len(o.Rows) != 1 || o.Rows[0].How != "applied" {
				t.Errorf("op %d = %+v, want %s run and recorded as applied", i, o, want)
			}
		}
	})
}

// TestBuildPlan_NeverReplaysAfter0016 is the F23 hazard as a property: in every state where 0016 has run, no operation EXECUTES a migration numbered
// below 0017, because 0001..0015 are unqualified and would create empty module-1 tables in public. The paired control feeds the same check the one
// state where replay is correct, and it must find them.
func TestBuildPlan_NeverReplaysAfter0016(t *testing.T) {
	files := synth(20)
	states := map[string]dbState{
		"0016 by the old loop, no tracker":    {VerifyExists: true},
		"0017 by the old loop, empty tracker": {TrackerExists: true, VerifyExists: true},
		"tracked, complete":                   {TrackerExists: true, VerifyExists: true, Rows: recordedRows(files[:17], "applied")},
		"tracked, adopted":                    {TrackerExists: true, VerifyExists: true, Rows: recordedRows(files[:17], "adopted")},
		"tracked up to 0018":                  {TrackerExists: true, VerifyExists: true, Rows: recordedRows(files[:18], "applied")},
	}
	for name, st := range states {
		p, err := buildPlan(files, st)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if r := replaysBefore0017(p); len(r) != 0 {
			t.Errorf("%s: the plan EXECUTES migration(s) %v although 0016 already ran", name, r)
		}
	}
	// CONTROL: a database at 0015 or earlier (no verify schema) legitimately replays 0001..0016 — the check must see them.
	p, err := buildPlan(files, dbState{PublicVerifications: true})
	if err != nil {
		t.Fatal(err)
	}
	if r := replaysBefore0017(p); len(r) != 16 {
		t.Errorf("CONTROL: on a 0015-era database the plan replays %d file(s) below 0017, want 16: the check above cannot see a replay", len(r))
	}
}

// TestBuildPlan_Refusals: every unsafe history is refused with a message naming the problem, and the plan is empty (a refusal changes nothing).
func TestBuildPlan_Refusals(t *testing.T) {
	files := synth(18)
	tracked := func(mut func([]row) []row) dbState {
		return dbState{TrackerExists: true, VerifyExists: true, Rows: mut(recordedRows(files[:17], "applied"))}
	}
	short8 := func(s string) string { return s[:8] }

	cases := []struct {
		name string
		st   dbState
		want []string
	}{
		{"a recorded version with no file (the database is ahead)", tracked(func(r []row) []row {
			return append(r, row{Version: "0099", Checksum: files[0].Checksum, How: "applied"})
		}), []string{"0099", "no file", "ahead of this checkout"}},
		{"a recorded checksum that differs from the file", tracked(func(r []row) []row {
			r[4].Checksum = strings.Repeat("a", 64)
			return r
		}), []string{"migration 0005 changed after it was applied", "recorded aaaaaaaa", "on disk " + short8(files[4].Checksum)}},
		{"a hole: 0005 missing, later ones present", tracked(func(r []row) []row { return append(r[:4:4], r[5:]...) }),
			[]string{"migration 0005 is not applied but a later one (0006) is"}},
		{"two holes", tracked(func(r []row) []row { return append(append(append([]row{}, r[:2]...), r[3:6]...), r[7:]...) }),
			[]string{"migration 0003 is not applied", "migration 0007 is not applied"}},
		{"a hole AND a changed file: both are listed", tracked(func(r []row) []row {
			r[0].Checksum = strings.Repeat("b", 64)
			return append(r[:4:4], r[5:]...)
		}), []string{"migration 0001 changed", "migration 0005 is not applied"}},
		{"0001..0015 were re-run after 0016 (adoption, empty tracker)", dbState{TrackerExists: true, VerifyExists: true, PublicVerifications: true},
			[]string{"public.verifications", "re-run"}},
		{"0001..0015 were re-run after 0016 (adoption, no tracker)", dbState{VerifyExists: true, PublicVerifications: true},
			[]string{"public.verifications", "re-run"}},
		{"a tracker with no module-1 tables", dbState{TrackerExists: true}, []string{"verify.schema_migrations exists but verify.verifications does not"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := buildPlan(files, c.st)
			if err == nil {
				t.Fatalf("buildPlan accepted it: %+v", p)
			}
			msg := asRefusal(t, err).Error()
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("refusal %q does not contain %q", msg, w)
				}
			}
			if len(p.Ops) != 0 {
				t.Errorf("a refusal must carry no operations, got %d", len(p.Ops))
			}
		})
	}

	// The checkout is older than the tracker, or empty.
	if _, err := buildPlan(synth(16), dbState{}); err == nil || !strings.Contains(asRefusal(t, err).Error(), "0017") {
		t.Errorf("a directory without the tracker migration must be refused naming 0017, got %v", err)
	}
	if _, err := buildPlan(nil, dbState{}); err == nil {
		t.Error("an empty file list must be refused")
	}

	// CONTROL: the same fixtures without the defect are accepted, so the refusals above are about the defect and nothing else.
	if _, err := buildPlan(files, tracked(func(r []row) []row { return r })); err != nil {
		t.Errorf("CONTROL: a clean tracked history was refused: %v", err)
	}
	if _, err := buildPlan(files, dbState{TrackerExists: true, VerifyExists: true}); err != nil {
		t.Errorf("CONTROL: a clean empty-tracker adoption was refused: %v", err)
	}
}

// TestStatusLines pins the documented format and the three states, and that a changed file or a missing file is shown rather than hidden.
func TestStatusLines(t *testing.T) {
	files := synth(4)
	rows := recordedRows(files[:2], "applied")
	rows[1].How = "adopted"
	rows[1].Checksum = strings.Repeat("c", 64) // recorded differently from disk
	rows = append(rows, row{Version: "0042", Checksum: strings.Repeat("d", 64), How: "applied"})
	lines := statusLines(files, dbState{TrackerExists: true, Rows: rows})

	want := []string{
		"0001 applied " + files[0].Short(),
		"0002 adopted cccccccc  CHANGED: the file on disk is " + files[1].Short(),
		"0003 pending " + files[2].Short(),
		"0004 pending " + files[3].Short(),
		"0042 applied dddddddd  NO FILE: the database is ahead of this checkout",
	}
	if len(lines) != len(want) {
		t.Fatalf("%d lines, want %d: %v", len(lines), len(want), lines)
	}
	for i, l := range lines {
		if l.String() != want[i] {
			t.Errorf("line %d = %q, want %q", i, l.String(), want[i])
		}
	}
	// An untracked database reports every file as pending.
	for _, l := range statusLines(files, dbState{}) {
		if l.State != "pending" {
			t.Errorf("untracked database: %s is %s, want pending", l.Version, l.State)
		}
	}
}

// TestTracker_NothingAtRuntimeReadsIt: the tracker is the migrator's, not the service's (verify_app holds no privilege on it), so no Go file outside the
// runner may mention it. A runtime query against it would fail with 42501 in production while every test as a superuser passed. The control proves the
// scan can find the name where it is legitimately used.
func TestTracker_NothingAtRuntimeReadsIt(t *testing.T) {
	root := filepath.Join(testpg.MigrationsDir(), "..")
	var hits []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "schema_migrations") {
			rel, _ := filepath.Rel(root, path)
			hits = append(hits, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 40 {
		t.Fatalf("scanned only %d non-test Go files under %s: the walk is not looking at the module", scanned, root)
	}
	allowed := map[string]bool{"internal/migrate/migrate.go": true, "cmd/migrate/main.go": true}
	found := map[string]bool{}
	for _, h := range hits {
		found[h] = true
		if !allowed[h] {
			t.Errorf("%s mentions schema_migrations: the runtime login has no privilege on it, only the runner may touch it", h)
		}
	}
	if !found["internal/migrate/migrate.go"] {
		t.Errorf("CONTROL: the scan did not find the tracker in internal/migrate/migrate.go (hits: %v): it cannot see a violation either", hits)
	}
}
