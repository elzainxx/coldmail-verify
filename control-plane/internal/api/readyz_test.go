package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/secrets"
	"github.com/coldmail-verify/control-plane/internal/store"
)

// READINESS MUST TRACK SERVING CAPABILITY (fix-plan item 10, contract §1.9.1-2).
//
// THE DEFECT THIS FILE EXISTS FOR. /readyz used to compute `ready` as `secrets.HasPepper()` and
// nothing else. A control-plane with a pepper and an unreachable store therefore answered
// `200 {"ready":true}` while every POST /v1/verify answered `503 {"error":"not ready"}`. The two
// predicates had drifted apart, and readiness is the one an orchestrator acts on: a load balancer
// keeps such a process in rotation and a deployment gate lets the rollout finish. The contract's
// wording is that readiness which does not track serving capability is *worse* than no readiness
// endpoint, and the reason is exactly this — it is not silence, it is a wrong answer that other
// machinery obeys.
//
// WHY A FAKE PINGER IS THE RIGHT TOOL HERE, unlike pg_tenancy_test.go. That file argues at length
// that a fake store cannot test a tenant predicate, because "a tenant predicate is a WHERE clause,
// and with a fake store there is no SQL for a missing WHERE clause to be visible in". The property
// HERE is different in kind: it is whether the handler CONSULTS its dependency and REPORTS what it
// said. A fake can express both outcomes of a ping — including the failing one, which a healthy real
// Postgres cannot be made to produce on demand — so the fake tests strictly more of the state space.
// The real-Postgres half of the claim (that *store.PG genuinely satisfies Pinger, and that a real
// dead pool reads as not-ready) is pinned separately by TestReadyz_RealPGPingIsTheRealDependency
// below, which is gated on TM_TEST_DATABASE_URL and runs in CI.
//
// FAIL-VERIFY — seven mutations, each applied to api.go, run, reverted, and hash-proven
// byte-identical afterwards (md5 e5dc6de4…). What each ACTUALLY fired, not what was predicted:
//
//	A. `ready := pepperOK` (revert to the original defect) → 4 tests: UnreachableDBIsNotReady,
//	   ChecksMapAgreesWithTheStatusWord/db_down, PingIsLivePerRequest, NilPingerReportsFalseNotAbsent.
//	   Predicted 2; the extra two are the redundancy working. Note the shape of the failures: three
//	   of them are "you said ready while a dependency was down", which is the defect verbatim.
//	B. `"db": true` (a literal, i.e. a check that cannot fail) → 4: UnreachableDBIsNotReady,
//	   ChecksMapReportsEachDependencySeparately/{db_down_only,both_down},
//	   ChecksMapAgreesWithTheStatusWord/db_down, NilPingerReportsFalseNotAbsent.
//	C. drop the context timeout → 1, PingIsGivenABoundedContext, and ONLY that one. Exactly the
//	   isolation wanted: the bound is a property no other assertion accidentally covers.
//	D. cache the ping in a struct field, computed once → 1, PingIsLivePerRequest.
//	E. drop checks{} entirely → 5 (every test that reads the map), each reporting the full body so
//	   the diagnostic names what was actually served.
//	F. drop `at` from /readyz → 1, CarriesTheContractEnvelope, on all three of its `at` assertions.
//	G. drop `config` → 2, and the second is the one that matters: TestFrontDoorDefer_SummaryNamesTheRung
//	   — a PRE-EXISTING test asserting the documented runbook grep (operator-runbook.md's
//	   `grep -o 'frontdoor=[^ "]*'`). Removing an operator field breaks a procedure someone runs
//	   during an incident, and that test is what says so.
//
// NOT COVERED BY ANY OF THE ABOVE, deliberately: dropping `.WithPinger(pg)` in main.go's buildAPI
// fails nothing in this package, because no unit test can see main's wiring. That is precisely why
// TestReadyz_NilPingerReportsFalseNotAbsent exists — the omission degrades to
// honestly-not-ready (`db:false`, 503) rather than to an absent check a dashboard reads as fine. The
// wiring failure mode is made SAFE rather than detectable, which is the achievable guarantee here.

// readyzTestPepper is the value the fixture loads. Named so the leak test can assert on the exact
// bytes it put in rather than on a copy of the literal that could drift apart from the fixture.
const readyzTestPepper = "readyz-test-pepper-value-long-enough"

// fakePinger is a Pinger whose answer the test controls, and which records what it was asked.
type fakePinger struct {
	err         error // what Ping returns
	calls       int   // how many times it was called (proves live-per-request)
	sawDeadline bool  // whether the ctx it received carried a deadline (proves the bound)
}

func (f *fakePinger) Ping(ctx context.Context) error {
	f.calls++
	if _, ok := ctx.Deadline(); ok {
		f.sawDeadline = true
	}
	return f.err
}

// readyzServer wires the minimum needed to exercise /readyz: a real Secrets with a real pepper (so
// the pepper check is genuinely true, not stubbed), and whatever Pinger the caller wants. now is
// pinned so `at` can be asserted exactly rather than "looks like a timestamp".
func readyzServer(t *testing.T, pepper bool, p Pinger) (*Server, time.Time) {
	t.Helper()
	src := fakeSource{}
	if pepper {
		// The exported constant, not the literal env name: the first draft of this file guessed the
		// name, loaded no pepper, and the guard below caught it. With the constant a rename cannot
		// silently turn every "pepper present" case into a "pepper absent" one — which would have
		// made the central test pass for the wrong reason, since no-pepper is ALSO not-ready.
		src[secrets.EnvPepper] = readyzTestPepper
	}
	sec, err := secrets.Load(src, false)
	if err != nil {
		t.Fatalf("secrets.Load: %v", err)
	}
	if sec.HasPepper() != pepper {
		t.Fatalf("fixture broken: wanted HasPepper()==%v, got %v — the pepper env key this test sets may have been renamed", pepper, sec.HasPepper())
	}
	at := time.Date(2026, 8, 5, 9, 30, 0, 0, time.UTC)
	srv := New(config.Defaults(), sec, nil)
	srv.now = func() time.Time { return at }
	if p != nil {
		srv = srv.WithPinger(p)
	}
	return srv, at
}

func getReadyz(t *testing.T, srv *Server) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/readyz body is not JSON (%v): %s", err, rec.Body.String())
	}
	return rec, body
}

// checksOf pulls checks{} out as a map[string]bool, failing loudly if it is absent or the wrong
// shape — an absent checks{} must never be silently read as "no failing checks".
func checksOf(t *testing.T, body map[string]any) map[string]bool {
	t.Helper()
	raw, ok := body["checks"]
	if !ok {
		t.Fatalf("/readyz has no checks{} — the contract §1.9 envelope requires it: %v", body)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("checks must be an object of name->bool, got %T (%v)", raw, raw)
	}
	out := map[string]bool{}
	for k, v := range m {
		b, ok := v.(bool)
		if !ok {
			t.Fatalf("checks[%q] must be a bool (a monitor branches on it), got %T (%v)", k, v, v)
		}
		out[k] = b
	}
	return out
}

// TestReadyz_UnreachableDBIsNotReady is THE defect. A pepper is loaded, so the OLD predicate says
// ready; the DB is unreachable, so the module cannot produce a verdict it can log (Door 2).
func TestReadyz_UnreachableDBIsNotReady(t *testing.T) {
	srv, _ := readyzServer(t, true, &fakePinger{err: errors.New("dial tcp 127.0.0.1:5432: connection refused")})
	rec, body := getReadyz(t, srv)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("pepper present but DB unreachable => 503, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if body["status"] != "not_ready" {
		t.Errorf("status must be not_ready, got %v", body["status"])
	}
	if body["ready"] != false {
		t.Errorf("the pre-existing `ready` boolean must agree: want false, got %v", body["ready"])
	}
	ch := checksOf(t, body)
	if ch["db"] {
		t.Errorf("checks.db must be false when the ping fails, got true — this is the defect verbatim")
	}
	if !ch["pepper"] {
		t.Errorf("checks.pepper must still be TRUE here, got false. If this fires the test is vacuous: it would be passing because the pepper is missing rather than because the DB is, and the pre-item-10 code would pass it too")
	}
}

// TestReadyz_BothDependenciesPresentIsReady is the CONTROL for the test above. Without it, a
// handleReadyz hardcoded to `not_ready` would pass every assertion in this file.
func TestReadyz_BothDependenciesPresentIsReady(t *testing.T) {
	srv, _ := readyzServer(t, true, &fakePinger{})
	rec, body := getReadyz(t, srv)

	if rec.Code != http.StatusOK {
		t.Errorf("pepper + reachable DB => 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if body["status"] != "ready" {
		t.Errorf("status must be ready, got %v", body["status"])
	}
	if body["ready"] != true {
		t.Errorf("`ready` must be true, got %v", body["ready"])
	}
	ch := checksOf(t, body)
	if !ch["db"] || !ch["pepper"] {
		t.Errorf("both checks must be true, got %v", ch)
	}
}

// TestReadyz_ChecksMapReportsEachDependencySeparately: checks{} is only useful if each entry tracks
// its OWN dependency. A body that ANDs everything into every field tells an operator that something
// is wrong but not what, which is the entire reason the map exists rather than one boolean.
func TestReadyz_ChecksMapReportsEachDependencySeparately(t *testing.T) {
	for _, tc := range []struct {
		name               string
		pepper             bool
		pingErr            error
		wantPepper, wantDB bool
	}{
		{"both up", true, nil, true, true},
		{"db down only", true, errors.New("down"), true, false},
		{"pepper missing only", false, nil, false, true},
		{"both down", false, errors.New("down"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := readyzServer(t, tc.pepper, &fakePinger{err: tc.pingErr})
			_, body := getReadyz(t, srv)
			ch := checksOf(t, body)
			if ch["pepper"] != tc.wantPepper {
				t.Errorf("checks.pepper: want %v, got %v (body %v)", tc.wantPepper, ch["pepper"], body)
			}
			if ch["db"] != tc.wantDB {
				t.Errorf("checks.db: want %v, got %v (body %v)", tc.wantDB, ch["db"], body)
			}
		})
	}
}

// TestReadyz_ChecksMapAgreesWithTheStatusWord: the envelope carries the same answer three ways
// (`status`, `ready`, and the AND of checks{}) plus the HTTP status. Any pair disagreeing is a
// monitor reading one thing and an orchestrator reading another off the same reply, which is a
// worse failure than either being wrong alone.
func TestReadyz_ChecksMapAgreesWithTheStatusWord(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pepper  bool
		pingErr error
	}{
		{"both up", true, nil},
		{"db down", true, errors.New("down")},
		{"pepper missing", false, nil},
		{"both down", false, errors.New("down")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := readyzServer(t, tc.pepper, &fakePinger{err: tc.pingErr})
			rec, body := getReadyz(t, srv)
			ch := checksOf(t, body)
			allOK := true
			for _, v := range ch {
				allOK = allOK && v
			}
			wantWord := "not_ready"
			wantCode := http.StatusServiceUnavailable
			if allOK {
				wantWord, wantCode = "ready", http.StatusOK
			}
			if body["status"] != wantWord {
				t.Errorf("checks%v => status %q, got %v", ch, wantWord, body["status"])
			}
			if body["ready"] != allOK {
				t.Errorf("checks%v => ready %v, got %v", ch, allOK, body["ready"])
			}
			if rec.Code != wantCode {
				t.Errorf("checks%v => HTTP %d, got %d", ch, wantCode, rec.Code)
			}
		})
	}
}

// TestReadyz_PingIsLivePerRequest: the ping must run on EVERY request, not once at boot. A DB that
// has gone away since startup is the case a cached answer misses, and it is the common one — the
// boot-time answer is almost always "fine", which is why caching it feels safe and is not.
func TestReadyz_PingIsLivePerRequest(t *testing.T) {
	p := &fakePinger{}
	srv, _ := readyzServer(t, true, p)

	if _, body := getReadyz(t, srv); body["status"] != "ready" {
		t.Fatalf("first call must be ready, got %v", body["status"])
	}
	if p.calls != 1 {
		t.Fatalf("Ping must be called once per request, got %d calls", p.calls)
	}

	// The DB goes away between requests. Nothing about the Server changes.
	p.err = errors.New("connection reset by peer")
	rec, body := getReadyz(t, srv)
	if body["status"] != "not_ready" || rec.Code != http.StatusServiceUnavailable {
		t.Errorf("after the DB went away: want not_ready/503, got %v/%d — the ping result is being cached", body["status"], rec.Code)
	}
	if p.calls != 2 {
		t.Errorf("Ping must be called again on the second request, got %d total calls", p.calls)
	}

	// And back: recovery must be observed too, or a transient blip would take the process out of
	// rotation permanently.
	p.err = nil
	if rec, body := getReadyz(t, srv); body["status"] != "ready" || rec.Code != http.StatusOK {
		t.Errorf("after recovery: want ready/200, got %v/%d", body["status"], rec.Code)
	}
}

// TestReadyz_PingIsGivenABoundedContext: readiness must answer even when the DB is WEDGED rather
// than down — connected, accepting, never replying. Without a deadline the handler blocks on the
// ping and /readyz itself hangs; a hanging readiness endpoint is read as "unknown" by some
// orchestrators and "still healthy" by others, so it is the ambiguous failure rather than a clean
// not-ready. Asserting the deadline is on the ctx (rather than timing a real 3s block) keeps this
// test fast and non-flaky while pinning the property that matters.
func TestReadyz_PingIsGivenABoundedContext(t *testing.T) {
	p := &fakePinger{}
	srv, _ := readyzServer(t, true, p)
	getReadyz(t, srv)
	if !p.sawDeadline {
		t.Error("the ctx handed to Ping must carry a deadline — an unbounded ping lets a wedged DB hang /readyz itself")
	}
}

// TestReadyz_NilPingerReportsFalseNotAbsent: when no DB is configured at all (buildAPI's early
// returns), the check must be present and FALSE. An OMITTED check is the dangerous shape: a
// dashboard iterating checks{} finds nothing failing and renders green for a process that cannot
// serve a single request. It also means forgetting `.WithPinger(pg)` in main.go fails CLOSED.
func TestReadyz_NilPingerReportsFalseNotAbsent(t *testing.T) {
	srv, _ := readyzServer(t, true, nil) // pepper fine, no pinger wired at all
	rec, body := getReadyz(t, srv)

	ch := checksOf(t, body)
	got, present := ch["db"]
	if !present {
		t.Fatalf("checks.db must be PRESENT even with no pinger wired — an absent check reads as 'nothing wrong': %v", body)
	}
	if got {
		t.Errorf("checks.db must be false with no pinger, got true")
	}
	if rec.Code != http.StatusServiceUnavailable || body["status"] != "not_ready" {
		t.Errorf("no DB wired => not_ready/503, got %v/%d", body["status"], rec.Code)
	}
}

// TestReadyz_CarriesTheContractEnvelope: status + checks{} + at, the three fields §1.9 requires,
// with `at` an RFC3339 timestamp of the CURRENT time rather than a fixed or empty string. `at` is
// what distinguishes "this module answered just now" from a cached or proxied reply.
func TestReadyz_CarriesTheContractEnvelope(t *testing.T) {
	srv, at := readyzServer(t, true, &fakePinger{})
	_, body := getReadyz(t, srv)

	for _, k := range []string{"status", "checks", "at"} {
		if _, ok := body[k]; !ok {
			t.Errorf("contract §1.9 requires %q on /readyz; body: %v", k, body)
		}
	}
	gotAt, _ := body["at"].(string)
	if gotAt != at.Format(time.RFC3339) {
		t.Errorf("at must be the request's own time in RFC3339: want %q, got %q", at.Format(time.RFC3339), gotAt)
	}
	if _, err := time.Parse(time.RFC3339, gotAt); err != nil {
		t.Errorf("at must PARSE as RFC3339 (%v): %q", err, gotAt)
	}
}

// TestHealthz_CarriesAtToo: /healthz gained `at` for the same reason (§1.9's liveness shape). The
// senders already had it; this surface did not, so a dashboard could not tell a live 200 from a
// stale one on one of the three modules.
func TestHealthz_CarriesAtToo(t *testing.T) {
	srv, at := readyzServer(t, true, &fakePinger{})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz is liveness — it must be 200 whatever the deps say, got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/healthz body is not JSON (%v): %s", err, rec.Body.String())
	}
	if body["status"] != "ok" {
		t.Errorf("liveness status must stay %q (additive-only), got %v", "ok", body["status"])
	}
	if got, _ := body["at"].(string); got != at.Format(time.RFC3339) {
		t.Errorf("at: want %q, got %q", at.Format(time.RFC3339), got)
	}

	// Liveness must NOT depend on readiness: a dead DB still means the process is alive, and
	// conflating the two makes an orchestrator RESTART a process whose only problem is downstream.
	dead, _ := readyzServer(t, false, &fakePinger{err: errors.New("down")})
	rec2 := httptest.NewRecorder()
	dead.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec2.Code != http.StatusOK {
		t.Errorf("/healthz must stay 200 with every dependency down, got %d", rec2.Code)
	}
}

// TestReadyz_PreExistingFieldsAreStillThere is the additive-only assertion (PROJECT_RULES: "a rename
// or removal is a break, never allowed"). Item 10 adds an envelope AROUND an operator surface that
// is already in use: `config` is grepped by a documented runbook step (operator-runbook.md's
// `grep -o 'frontdoor=[^ "]*'`) during an incident, and `ready`/`pepper`/`supabaseKey` are what the
// existing tests and phase-0 status doc describe. Adding fields is free; moving these is not.
func TestReadyz_PreExistingFieldsAreStillThere(t *testing.T) {
	srv, _ := readyzServer(t, true, &fakePinger{})
	_, body := getReadyz(t, srv)

	if _, ok := body["ready"].(bool); !ok {
		t.Errorf("`ready` must still be a top-level BOOL, got %T (%v)", body["ready"], body["ready"])
	}
	cfgSummary, ok := body["config"].(string)
	if !ok || cfgSummary == "" {
		t.Fatalf("`config` must still be a top-level non-empty string — operator-runbook.md greps it: %T %v", body["config"], body["config"])
	}
	// Not just present: it must still be the SUMMARY, i.e. still contain the rung an operator looks
	// for. A `config` reduced to "ok" would satisfy a presence check and break the runbook.
	if !strings.Contains(cfgSummary, "frontdoor=") {
		t.Errorf("`config` must still be cfg.Summary() (it must contain frontdoor=…, which operator-runbook.md greps): %q", cfgSummary)
	}
	// Presence-only secret reporting, unchanged: the WORDS "set"/"unset", never a value.
	if body["pepper"] != "set" {
		t.Errorf("`pepper` must still report presence as \"set\"/\"unset\", got %v", body["pepper"])
	}
	if body["supabaseKey"] != "unset" {
		t.Errorf("`supabaseKey` must still report presence as \"set\"/\"unset\", got %v", body["supabaseKey"])
	}
}

// TestReadyz_LeaksNoSecretValue: the surface reports PRESENCE. Item 10 added fields to a body that
// is served unauthenticated, so the leak check is re-run over the new shape rather than assumed to
// still hold from before.
func TestReadyz_LeaksNoSecretValue(t *testing.T) {
	srv, _ := readyzServer(t, true, &fakePinger{})
	rec, _ := getReadyz(t, srv)
	if strings.Contains(rec.Body.String(), readyzTestPepper) {
		t.Errorf("/readyz leaked the pepper VALUE: %s", rec.Body.String())
	}
	// Control: the fixture really did load that pepper, so the assertion above had something to
	// find. Without this, a broken fixture would make the leak check pass by loading no secret.
	if !srv.secrets.HasPepper() {
		t.Fatal("fixture broken: no pepper was loaded, so the leak assertion above proved nothing")
	}
}

// TestReadyz_RealPGPingIsTheRealDependency closes the one gap the fake cannot: that *store.PG
// actually satisfies Pinger against a real server, and that a genuinely dead pool reads as
// not-ready. A compile-time `var _ Pinger = (*store.PG)(nil)` would prove the signature and nothing
// about behaviour — pgxpool's Ping acquires a connection, so "the method exists" and "the method
// detects a dead pool" are different claims.
//
// SKIP-honest when TM_TEST_DATABASE_URL is unset (test-strategy §4); CI sets it, so in CI it RUNS.
func TestReadyz_RealPGPingIsTheRealDependency(t *testing.T) {
	dsn := os.Getenv("TM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PENDING: set TM_TEST_DATABASE_URL to a reachable Postgres to exercise readiness against the real store (the fake proves the handler's logic, not that *store.PG's Ping detects a dead pool)")
	}
	pg, err := store.OpenPG(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}

	srv, _ := readyzServer(t, true, pg)
	rec, body := getReadyz(t, srv)
	if rec.Code != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("a REACHABLE real Postgres must read as ready/200, got %v/%d (body %s)", body["status"], rec.Code, rec.Body.String())
	}
	if ch := checksOf(t, body); !ch["db"] {
		t.Fatalf("checks.db must be true against a live Postgres, got %v", ch)
	}

	// Now kill it for real — no fake, no injected error. Closing the pool is what a lost DB looks
	// like to this process.
	pg.Close()
	rec2, body2 := getReadyz(t, srv)
	if rec2.Code != http.StatusServiceUnavailable || body2["status"] != "not_ready" {
		t.Errorf("after Close() the real pool must read as not_ready/503, got %v/%d (body %s)", body2["status"], rec2.Code, rec2.Body.String())
	}
	if ch := checksOf(t, body2); ch["db"] {
		t.Errorf("checks.db must be false against a closed pool, got %v", ch)
	}
}
