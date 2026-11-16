package main

// Tests of the command: its usage errors, its exit codes, its output format, and that no credential reaches either output stream. The ones that need
// Postgres run whenever TM_TEST_DATABASE_URL is set (CI always sets it); the rest run everywhere.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-verify/control-plane/internal/migrate"
	"github.com/coldmail-verify/control-plane/internal/testpg"
)

func env(dsn, dir string) func(string) string {
	m := map[string]string{envDSN: dsn, envDir: dir}
	return func(k string) string { return m[k] }
}

// runCmd runs the command under the cluster-wide migration lock (it renames roles) and returns its exit code and both streams.
func runCmd(t *testing.T, getenv func(string) string, args ...string) (int, string, string) {
	t.Helper()
	unlock := testpg.LockMigrations(t)
	defer unlock()
	var out, errb bytes.Buffer
	code := run(args, getenv, &out, &errb)
	return code, out.String(), errb.String()
}

func copyDir(t *testing.T) string {
	t.Helper()
	src, dst := testpg.MigrationsDir(), t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
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
	}
	return dst
}

// TestRun_Usage: anything but exactly `up` or `status` is a usage error (64) that says how to call it, and nothing is attempted.
func TestRun_Usage(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"down"}, {"up", "status"}, {"UP"}, {"--help"}} {
		var out, errb bytes.Buffer
		if code := run(args, env("postgres://x@127.0.0.1:1/x", "migrations"), &out, &errb); code != exitUsage {
			t.Errorf("args %q: exit %d, want %d", args, code, exitUsage)
		}
		if !strings.Contains(errb.String(), "usage: migrate up|status") || out.Len() != 0 {
			t.Errorf("args %q: stderr %q stdout %q, want only the usage text on stderr", args, errb.String(), out.String())
		}
	}
	// CONTROL: the two real subcommands are NOT usage errors (they fail later, on the unreachable database).
	for _, sub := range []string{"up", "status"} {
		var out, errb bytes.Buffer
		if code := run([]string{sub}, env("postgres://x@127.0.0.1:1/x?connect_timeout=2", testpg.MigrationsDir()), &out, &errb); code == exitUsage || code == exitOK {
			t.Errorf("%s against an unreachable database: exit %d, want a runtime error", sub, code)
		}
	}
}

// TestRun_MissingConfig: no DSN is a usage error that NAMES the variable (and says it is the migrating identity, not verify_app); an unreadable
// directory is a plain error that names the directory.
func TestRun_MissingConfig(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"up"}, env("", ""), &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "TM_MIGRATE_DATABASE_URL is not set") {
		t.Errorf("no DSN: exit %d, stderr %q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	if code := run([]string{"status"}, env("postgres://x@127.0.0.1:1/x", missing), &out, &errb); code != exitError || !strings.Contains(errb.String(), missing) {
		t.Errorf("a missing migrations directory: exit %d, stderr %q, want exit %d naming the directory", code, errb.String(), exitError)
	}
	// An empty directory is refused (exit 2), not "nothing to do": a run over nothing proves nothing.
	out.Reset()
	errb.Reset()
	if code := run([]string{"up"}, env("postgres://x@127.0.0.1:1/x", t.TempDir()), &out, &errb); code != exitRefused || !strings.Contains(errb.String(), "no migration files") {
		t.Errorf("an empty migrations directory: exit %d, stderr %q, want exit %d", code, errb.String(), exitRefused)
	}
}

// TestRun_NeverPrintsTheCredential: whatever fails (a DSN the driver cannot parse, a refused connection), neither stream carries the password, in the
// three spellings pgx v5.10.0 does not redact itself (a query parameter, userinfo in a URL it cannot parse, key = value with spaces).
func TestRun_NeverPrintsTheCredential(t *testing.T) {
	const canary = "canaryMIGRATEpw8841"
	dsns := map[string]string{
		"password as a query parameter, unparseable sslmode": "postgres://u@127.0.0.1:1/db?password=" + canary + "&sslmode=bogus",
		"password in userinfo, unparseable port":             "postgres://u:" + canary + "@127.0.0.1:notaport/db",
		"password in userinfo, refused connection":           "postgres://u:" + canary + "@127.0.0.1:1/db?connect_timeout=2",
		"key = value with spaces":                            "host=127.0.0.1 port=1 dbname=db user=u password = " + canary + " sslmode=bogus",
		"password as a query parameter, refused connection":  "postgres://u@127.0.0.1:1/db?password=" + canary + "&connect_timeout=2",
	}
	for name, dsn := range dsns {
		for _, sub := range []string{"up", "status"} {
			var out, errb bytes.Buffer
			code := run([]string{sub}, env(dsn, testpg.MigrationsDir()), &out, &errb)
			if code == exitOK {
				t.Errorf("%s / %s: a bad DSN exited 0", name, sub)
			}
			if strings.Contains(out.String()+errb.String(), canary) {
				t.Errorf("%s / %s: the credential reached the output:\n%s%s", name, sub, out.String(), errb.String())
			}
		}
	}
	// CONTROL: the suppression is what protects the credential. The driver's OWN parse error for the query-parameter shape carries it (ADR-0016, measured at
	// the pinned pgx v5.10.0), so the scan above would have caught a command that printed the driver's message.
	if _, err := pgx.ParseConfig(dsns["password as a query parameter, unparseable sslmode"]); err == nil || !strings.Contains(err.Error(), canary) {
		t.Errorf("CONTROL: pgx's own parse error no longer carries the credential (%v): re-read ADR-0016 and this test before trusting the suppression either way", err)
	}
}

var statusLineRE = regexp.MustCompile(`^[0-9]{4} (applied|adopted|pending) [0-9a-f]{8}$`)

// TestRun_UpAndStatus drives the real command against a real empty database: `up` exits 0 and prints the summary line in the documented format, a
// second `up` is a no-op in the same format, and `status` prints one well-formed line per file, all applied.
func TestRun_UpAndStatus(t *testing.T) {
	dir := testpg.MigrationsDir()
	files, err := migrate.LoadFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := len(files)
	getenv := env(testpg.Empty(t), dir)

	code, out, errs := runCmd(t, getenv, "up")
	if want := fmt.Sprintf("migrate: applied %d, adopted 0, already applied 0, total %d", n, n); code != exitOK || !strings.Contains(out, want+"\n") || errs != "" {
		t.Fatalf("first up: exit %d, stdout %q, stderr %q, want exit 0 and the line %q", code, out, errs, want)
	}
	code, out, errs = runCmd(t, getenv, "up")
	if want := fmt.Sprintf("migrate: applied 0, adopted 0, already applied %d, total %d", n, n); code != exitOK || !strings.Contains(out, want+"\n") || errs != "" {
		t.Fatalf("second up: exit %d, stdout %q, stderr %q, want exit 0 and the line %q", code, out, errs, want)
	}
	if strings.Contains(out, "applying") {
		t.Errorf("the second up announced an application: %q", out)
	}

	code, out, errs = runCmd(t, getenv, "status")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != exitOK || errs != "" || len(lines) != n {
		t.Fatalf("status: exit %d, %d line(s), stderr %q, want exit 0 and %d line(s)", code, len(lines), errs, n)
	}
	for i, l := range lines {
		if !statusLineRE.MatchString(l) || !strings.HasPrefix(l, files[i].Version+" applied "+files[i].Short()) {
			t.Errorf("status line %d = %q, want %q", i, l, files[i].Version+" applied "+files[i].Short())
		}
	}
}

// TestRun_ExitCodes: a refusal (a changed file) exits 2 and says nothing was changed; a failing migration exits 1 and says what committed before it.
// Status of a refused history still prints its lines.
func TestRun_ExitCodes(t *testing.T) {
	dir := testpg.MigrationsDir()
	files, err := migrate.LoadFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	dsn := testpg.Empty(t)
	if code, out, errs := runCmd(t, env(dsn, dir), "up"); code != exitOK {
		t.Fatalf("setup up: exit %d\n%s%s", code, out, errs)
	}

	changed := copyDir(t)
	b, err := os.ReadFile(filepath.Join(changed, files[4].Name))
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0x01
	if err := os.WriteFile(filepath.Join(changed, files[4].Name), b, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runCmd(t, env(dsn, changed), "up")
	if code != exitRefused || !strings.Contains(errs, "migrate: refused: migration 0005 changed after it was applied") || !strings.Contains(errs, "nothing was changed") || out != "" {
		t.Errorf("a changed file: exit %d, stdout %q, stderr %q, want exit %d, a refusal naming 0005, and nothing on stdout", code, out, errs, exitRefused)
	}
	code, out, errs = runCmd(t, env(dsn, changed), "status")
	if code != exitRefused || len(strings.Split(strings.TrimRight(out, "\n"), "\n")) != len(files) || !strings.Contains(out, "CHANGED") || !strings.Contains(errs, "refused") {
		t.Errorf("status of a changed file: exit %d, stdout %q, stderr %q, want exit %d, every line, a CHANGED note and the refusal", code, out, errs, exitRefused)
	}

	broken := copyDir(t)
	if err := os.WriteFile(filepath.Join(broken, fmt.Sprintf("%04d_broken.sql", len(files)+1)), []byte("THIS IS NOT SQL;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := testpg.Empty(t)
	code, out, errs = runCmd(t, env(fresh, broken), "up")
	if code != exitError || !strings.Contains(errs, "SQLSTATE 42601") || !strings.Contains(errs, fmt.Sprintf("after applying %d and adopting 0", len(files))) {
		t.Errorf("a failing migration: exit %d, stderr %q, want exit %d, the SQLSTATE and what committed before it", code, errs, exitError)
	}
}
