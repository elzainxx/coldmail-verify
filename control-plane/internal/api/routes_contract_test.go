package api

// routes_contract_test.go — fix-plan item 14, M1's half: the ROUTE-DRIFT gate.
//
// WHAT WAS MISSING. Modules 2 and 3 have had a `contract_drift` CI gate since their Phase 0: it
// probes the running Control and fails if the served route set differs from Docs/api-contract.md in
// either direction. M1 had the OTHER half of the mechanism — an in-code `Valid()` assertion on the
// response body before every sync write — and no route gate at all. Neither module had both. This is
// M1's half; the senders' half of the trade (the response-version assertion) goes the other way.
//
// WHY IN-PROCESS AND NOT A PROBE, i.e. why this is not a copy. The senders' gate curls a live
// compose stack. M1 HAS NO COMPOSE STACK — no Dockerfile for the control plane at all (fix-plan item
// 15, open). Waiting for packaging would mean no gate today, and the property does not need a network
// to be asserted: the served set is `(*Server).Routes()`, which a test can read directly. Two
// consequences worth stating rather than discovering later:
//
//   - STRONGER in one way: it runs on every `go test`, on a laptop, with no Docker and no database.
//     The senders' gate only runs where the stack is up.
//   - WEAKER in one way, and this is the honest limit: it proves the mux is CONFIGURED with these
//     routes, not that a deployed binary answers them on a socket. A reverse proxy or an ingress that
//     rewrites paths is outside what this can see. When item 15 lands, the compose harness should
//     probe the same set — and it can, because Routes() is exported for exactly that.
//
// WHY THE ROUTE TABLE IS DECLARED AND NOT SCANNED. Go's ServeMux cannot enumerate its own patterns,
// so the alternatives were (a) grep `mux.HandleFunc(` out of api.go, or (b) make the route set a
// value. (a) is the shape this project keeps getting burned by — a source-scanning check narrower
// than the property it guards, satisfied by text rather than by behaviour (rule 1 of the enumeration
// gate matched a comment about itself; two sender config gates were satisfied by a TODO naming the
// key). (b) makes the served set the compiler's view: Handler() is a loop over Routes(), so they
// cannot disagree. Same reasoning as the test-execution gate reading `go test -list` instead of a
// hardcoded count.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// contractPath is the documented set. Relative to this package.
const contractPath = "../../../Docs/api-contract.md"

// docRouteRow matches one row of the route table in Docs/api-contract.md:
//
//	| `POST` | `/v1/verify` | … | … |
//
// Anchored to the row START so a method/path named in prose elsewhere in the document cannot satisfy
// this — the defect that let a commented-out CI step satisfy the enumeration gate's rule 5, and a
// TODO satisfy the senders' config gates. The table row is a structural form; a sentence is not.
var docRouteRow = regexp.MustCompile("(?m)^\\|\\s*`([A-Z]+)`\\s*\\|\\s*`([^`]+)`\\s*\\|")

func documentedRoutes(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(contractPath)
	if err != nil {
		// NOT t.Skip. A missing contract document must fail: skipping would make this gate silently
		// inert exactly when the thing it reads is gone, which is this project's signature defect —
		// two append-only tests that always skipped while CI claimed they proved append-only (D13).
		t.Fatalf("cannot read the documented route set at %s: %v\n"+
			"  This test IS the contract_drift gate; without that file it would assert nothing. If the\n"+
			"  document moved, update contractPath. If it was deleted, the served surface now has no\n"+
			"  documented counterpart at all, which is the state fix-plan item 14 exists to end.", contractPath, err)
	}
	ms := docRouteRow.FindAllStringSubmatch(string(b), -1)
	if len(ms) == 0 {
		t.Fatalf("%s contains no route-table rows matching %s — the document exists but documents no\n"+
			"  routes, so a served-set comparison against it would pass by being empty on both sides.",
			contractPath, docRouteRow)
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m[1]+" "+m[2])
	}
	sort.Strings(out)
	return out
}

// routeServer is the Server these tests probe. unwiredServer (errors_test.go) — no verifier, no
// store, no pepper — is exactly right here: every route either answers from config alone (/healthz,
// /readyz) or errors, and NEITHER outcome is a 404, which is the only distinction the mux assertions
// below need. A literal `&Server{}` would be wrong, not merely lazy: `now` would be nil and /healthz
// would panic inside the test rather than report anything.
func routeServer(t *testing.T) *Server {
	t.Helper()
	return unwiredServer(t)
}

func servedRoutes(t *testing.T) []string {
	t.Helper()
	out := []string{}
	for _, rt := range routeServer(t).Routes() {
		out = append(out, rt.Pattern)
	}
	sort.Strings(out)
	return out
}

// TestRoutes_ServedSetMatchesTheDocumentedSet is the gate. BOTH directions: an undocumented route is
// a surface nobody reviewed, and a documented route that is not served is a promise to a caller that
// 404s.
func TestRoutes_ServedSetMatchesTheDocumentedSet(t *testing.T) {
	served, documented := servedRoutes(t), documentedRoutes(t)

	inDoc := map[string]bool{}
	for _, r := range documented {
		inDoc[r] = true
	}
	inCode := map[string]bool{}
	for _, r := range served {
		inCode[r] = true
	}

	for _, r := range served {
		if !inDoc[r] {
			t.Errorf("SERVED BUT UNDOCUMENTED: %q is in (*Server).Routes() and absent from %s.\n"+
				"  An undocumented route is a surface no reviewer saw and no caller can rely on. Add a row\n"+
				"  to the route table saying what it does and what authenticates it.", r, contractPath)
		}
	}
	for _, r := range documented {
		if !inCode[r] {
			t.Errorf("DOCUMENTED BUT NOT SERVED: %s names %q and this module does not answer it.\n"+
				"  A documented route that 404s is worse than an undocumented one: a caller writes against\n"+
				"  it. Either serve it or remove the row.", contractPath, r)
		}
	}
	if !t.Failed() {
		t.Logf("served set == documented set (%d routes): %s", len(served), strings.Join(served, ", "))
	}
}

// TestRoutes_HandlerServesExactlyTheRouteTable is the anti-vacuity control for the test above.
//
// WHY IT IS NEEDED. That test compares a doc to Routes(). If Handler() ever stopped being a loop over
// Routes() — someone adds one more mux.HandleFunc for a "quick debug endpoint" — the doc/Routes()
// comparison would still pass while the module served a route neither knew about. The gate would go
// quiet at precisely the moment the property failed, which is the shape that has bitten this project
// six times.
//
// WHAT THIS TEST DOES AND DOES NOT PROVE, stated precisely because the imprecise version of this
// sentence is itself the failure family. It proves (a) every route in the table dispatches, and
// (b) five specific unclaimed paths get the envelope's 404. It does NOT prove "every path not in the
// table 404s" — five samples cannot, and no finite set can. The extra-registration case above is
// therefore held by TestRoutes_HandlerIsALoopOverRoutes, which reads the SOURCE, because a route
// registered outside Routes() is invisible from the outside unless you already guess its path. That
// split is deliberate: this test asserts behaviour it can actually observe, and the source scan
// asserts the structural property that makes the behaviour total.
func TestRoutes_HandlerServesExactlyTheRouteTable(t *testing.T) {
	srv := routeServer(t)
	h := srv.Handler()

	// Each declared route must reach ITS OWN handler. Asserting "not 404" is the strongest thing
	// available without wiring a database: an unwired Server answers 503/401/400 from the handler,
	// and what matters here is that the mux dispatched rather than fell through.
	for _, rt := range srv.Routes() {
		path := strings.ReplaceAll(rt.Path(), "{id}", "00000000-0000-7000-8000-000000000000")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(rt.Method(), path, nil))
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s: the mux did not dispatch to a handler (404). Handler() and Routes() have\n"+
				"  diverged — the doc-comparison gate above cannot see that.", rt.Pattern)
		}
	}

	// A path no route claims must be the envelope's 404, not ServeMux's plain text. This is the half
	// of item 18 that never reached a handler, so item 18's own unification could not fix it.
	// requireEnvelope (errors_test.go) is the package's ONE envelope standard: `error` as an OBJECT
	// and not the pre-item-18 string, the registered code, the code's curated message, and a
	// bare-UUID request_id. Reusing it rather than writing a second checker here is deliberate — two
	// similar assertion helpers in one package means these paths could be held to a weaker standard
	// than the handler paths without anyone noticing, and item 18's point is that they are not. It
	// Fatalf's, so each probe is a subtest: one bad path reports its own name instead of ending the
	// loop and hiding the rest.
	for _, p := range []string{"/", "/v1", "/v1/nope", "/v1/verify/bulk/extra", "/metrics"} {
		t.Run("unclaimed "+p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
			requireEnvelope(t, rec, codeNotFound, http.StatusNotFound)
		})
	}
}

// TestRoutes_WrongVerbIsTheEnvelopeNotServeMuxsPlainText pins the audit-D17 regression.
//
// MEASURED ON THIS TREE BEFORE THE FIX (fix-plan item 14, found while adopting the senders' gate):
//
//	GET     /v1/verify -> 405 text/plain  "Method Not Allowed\n"   Allow: POST
//	DELETE  /healthz   -> 405 text/plain  "Method Not Allowed\n"   Allow: GET, HEAD
//
// net/http.ServeMux's own reply: no `code`, no `request_id`, not this package's envelope. A client
// doing body.error.code on any non-2xx nil-deref'd on every wrong verb, and there was no id to grep —
// the exact gap item 18 was built to close, surviving in the one place item 18 could not see. It is
// also module 3's audit D17, in a second module, and it was found the same way both times: by
// adopting a gate, not by reading the code.
//
// NOTE ON WHAT THE STATUS AND Allow ASSERTIONS DO NOT CATCH — the same caveat the senders' gate
// records, and it holds here for the same reason: ServeMux's plain-text 405 ALSO returns 405 and ALSO
// sets Allow. Those two assertions therefore do not fail on an unfixed binary. They are kept because
// they pin the OTHER regressions (a wrong verb collapsing to 404 or 200, or an Allow that drifts from
// the route table). The envelope assertions are the ones that catch D17.
func TestRoutes_WrongVerbIsTheEnvelopeNotServeMuxsPlainText(t *testing.T) {
	srv := routeServer(t)
	h := srv.Handler()

	// The verbs each path serves, derived from the same table the server uses — never hand-listed, so
	// a new route is covered here the day it is added.
	serves := map[string]map[string]bool{}
	for _, rt := range srv.Routes() {
		if serves[rt.Path()] == nil {
			serves[rt.Path()] = map[string]bool{}
		}
		serves[rt.Path()][rt.Method()] = true
		if rt.Method() == http.MethodGet {
			serves[rt.Path()][http.MethodHead] = true // ServeMux implies HEAD for GET
		}
	}

	probed := 0
	for _, rt := range srv.Routes() {
		path := strings.ReplaceAll(rt.Path(), "{id}", "00000000-0000-7000-8000-000000000000")
		for _, verb := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
			if serves[rt.Path()][verb] {
				continue // a verb this path really serves — not a wrong-verb probe
			}
			probed++
			t.Run(verb+" "+rt.Path(), func(t *testing.T) {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(verb, path, nil))

				if rec.Code != http.StatusMethodNotAllowed {
					t.Fatalf("returned %d, want 405 — a wrong verb on a REAL path must be\n"+
						"  method_not_allowed, never 404 (which says the path does not exist) and never 200",
						rec.Code)
				}
				if allow := rec.Header().Get("Allow"); allow == "" {
					t.Errorf("405 with no Allow header — the caller cannot learn the right verb")
				} else {
					for m := range serves[rt.Path()] {
						if !strings.Contains(allow, m) {
							t.Errorf("Allow=%q omits %s, which this path does serve — the header has\n"+
								"  drifted from the route table", allow, m)
						}
					}
				}
				// THE D17 ASSERTIONS. These are what fail on an unfixed binary. The explicit plain-text
				// check comes first only so the failure NAMES the defect; requireEnvelope would reject
				// that body too, with a less specific message.
				if body := rec.Body.String(); strings.TrimSpace(body) == "Method Not Allowed" {
					t.Fatalf("body is ServeMux's plain text %q, not the item-18 envelope — a client\n"+
						"  reading error.code nil-derefs here, and there is no request_id to correlate", body)
				}
				requireEnvelope(t, rec, codeMethodNotAllowed, http.StatusMethodNotAllowed)
			})
		}
	}
	if probed == 0 {
		t.Fatal("probed 0 wrong-verb combinations — this test asserted nothing. If every route now " +
			"serves every verb the loop above skips them all, and the check has gone vacuous.")
	}
	t.Logf("wrong-verb envelope proved on %d method/path combinations", probed)
}

// TestRoutes_HandlerIsALoopOverRoutes closes the one hole the two behavioural tests above cannot see:
// a route registered on the mux that is NOT in Routes(). From outside the process that route is
// invisible unless you already know its path, so no amount of probing finds it — the doc comparison
// stays green, the wrong-verb sweep never visits it, and the module serves a surface nobody reviewed.
//
// THIS IS A SOURCE SCAN, AND SOURCE SCANS ARE THE THING THIS PROJECT DISTRUSTS. The distinction from
// the scans that failed here: those were narrower than the property they guarded (rule 1 of the
// enumeration gate was satisfied by a comment about itself; two sender config gates by a TODO naming
// the key). This one is not a proxy for the property — for THIS property, "how many places register
// patterns" is a fact about the source and nowhere else. The behavioural half is already covered by
// the two tests above; this is the half that has no behavioural form. Same reasoning, same package:
// TestErrorEnvelope_NoHandRolledErrorBodiesRemain scans for the same kind of reason.
//
// IT GLOBS THE PACKAGE rather than reading api.go, because that exact narrowing was found by breaking
// the error-body scan: moving a handler to a new file left it blind while everything stayed green.
func TestRoutes_HandlerIsALoopOverRoutes(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// The three registrations Handler() is allowed to make, each on its own line, each derived from
	// the route table: the routes themselves, the per-path 405 fallback, and the "/" 404 catch-all.
	const wantRegistrations = 3
	found, scanned := 0, 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			code, _, _ := strings.Cut(line, "//") // a comment mentioning HandleFunc is not a registration
			if !strings.Contains(code, "mux.HandleFunc(") {
				continue
			}
			found++
			t.Logf("registration %d: %s:%d %s", found, f, i+1, strings.TrimSpace(line))
		}
	}
	if scanned == 0 {
		t.Fatal("scanned 0 non-test files — this scan asserted nothing (the glob or the package layout changed)")
	}
	if found != wantRegistrations {
		t.Errorf("found %d mux.HandleFunc registration(s) across %d non-test file(s) in this package, want exactly %d.\n"+
			"  MORE means a route was registered outside (*Server).Routes(): it is served, it is not in the\n"+
			"  route table, and therefore neither TestRoutes_ServedSetMatchesTheDocumentedSet nor the\n"+
			"  wrong-verb sweep can see it. Put it in Routes() and document it in %s.\n"+
			"  FEWER means Handler() stopped registering something it used to — most likely the 405 or 404\n"+
			"  fallback, which sends those replies back to net/http.ServeMux's plain text (audit D17).\n"+
			"  If you are legitimately changing the registration structure, update wantRegistrations AND\n"+
			"  say here what the new lines are; do not just bump the number.",
			found, scanned, wantRegistrations, contractPath)
	}
}
