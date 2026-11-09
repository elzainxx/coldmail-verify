package api

// FIX-PLAN ITEM 15b — THE AUTH SCHEME IS MANDATORY, AND THE CONTROL FOR IT.
//
// THE RULING, which predates this fix twice over and is why no ADR is owed here:
//   - history/phase-1-foundations-spec.md:774 (OQ-13, RESOLVED): "Bearer for /v1/verify".
//   - canonical-module-contract.md §1 clause 1: "`Authorization: Bearer <token>`. The scheme is
//     MANDATORY — a bare token MUST be rejected. (M1 accepts a bare token today; that is a laxness
//     to close, not a variation to accommodate.)"
//
// WHY IT SURVIVED A FULL AUDIT — the old helper carried a TRUE justification for a FALSE conclusion.
// trimBearer's comment was "the front door is bearer; the verifier-node token is raw — kept distinct
// per P1 §3.5.4". Both clauses are true and they describe OPPOSITE DIRECTIONS: the raw node token is
// sent OUTBOUND (internal/node/http_client.go:67, audit_client.go:39, config_client.go:36,
// health_client.go:36) and validated by the RUBY node (verifier-node/.../wrapper/app.rb:176). It never
// arrives here. trimBearer had exactly ONE caller — the inbound front door — so the clause that
// mentioned raw tokens was doing no work at the only site that ran, while `return h` accepted a
// scheme-less credential on every authenticated route. A comment that explains why a laxness is fine
// is the most durable kind of laxness; that is the shape worth naming, not the six lines of code.
//
// WHAT EACH TEST CONTROLS. The properties are independent, and the split below is MEASURED — four
// breaks were performed against this file and the exact red/green split recorded. Two of the splits
// are NOT what they read like they should be, which is the point of measuring instead of reasoning:
//
//   - SchemelessTokenIsRejected controls the FIX (the `return h` fallback). Break B110 restored
//     trimBearer verbatim: barePerCallerKey and bareStaticToken went RED — and wrongScheme and
//     schemeNameOnlyNoSpace stayed GREEN. That is worth knowing: with the fallback restored, "Basic
//     per-caller-key" and "Bearerper-caller-key" are still rejected, but for the WRONG REASON (the
//     whole header, scheme included, is compared as an opaque token and matches no secret). So only
//     two of these four cases are controls for the fix; the other two guard the parser's grammar and
//     would go red only under a parser that strips a scheme it should not accept. Both are worth
//     keeping, but only the bare ones can detect THIS regression.
//   - SchemeMatchIsCaseInsensitive controls the OTHER HALF, which the fix could plausibly have got
//     wrong in passing. Break B111 replaced strings.EqualFold with a case-SENSITIVE CutPrefix (i.e.
//     the naive fix): this test went RED and all four cases of SchemelessTokenIsRejected stayed
//     GREEN, as did the positive control. So a fix that closes the laxness by hardening the scheme
//     match into a fresh cross-module divergence does not pass this file — and nothing ELSE in the
//     suite would have objected, since every other test builds its header as "Bearer "+tok.
//   - The POSITIVE control (SchemeCorrectTokenStillAuthenticates) stops the refusal tests being
//     satisfied by "reject everything". Break B112 made bearerToken always return "": both refusal
//     tests stayed GREEN, and the positive control went red — but so did SchemeMatchIsCaseInsensitive,
//     which also authenticates. So the positive control is not the UNIQUE detector of vacuity here;
//     it is the one that pins WHICH principal comes back (caller id + owner) and that the static
//     branch still works, neither of which the case test looks at. Break B113 confirmed that
//     asymmetry from the other side: neutering the static-token compare at api.go:709 reddens the
//     positive control while leaving the case test green.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/store"
)

// schemeFixture seeds the two credential KINDS that reach authenticate() — a per-caller key (PN-13)
// and the legacy static token — because they are compared on two different branches (api.go:695 and
// api.go:707) and a fix applied to only one would leave the other lax. The same fixture serves all
// three tests so no case can pass by construction of its own server.
func schemeFixture(t *testing.T) (*Server, time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	mem := store.NewMem(func() time.Time { return now })
	mem.SeedCallerWithUser(sha("per-caller-key"), "caller_scheme", userA, now.Add(-time.Hour), time.Time{})
	return serverAtTime(t, mem, "legacy-static", now), now
}

// rawAuthReq sets Authorization to the EXACT string given, bypassing authReq's "Bearer "+tok
// construction. That bypass is the whole point: authReq can only build conforming headers, so it is
// structurally incapable of expressing the input this fix is about.
func rawAuthReq(header string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"a@b.co"}`))
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

// TestBearerScheme_SchemelessTokenIsRejected is the control for the fix.
//
// It asserts BOTH credential kinds and BOTH surfaces. Asserting only the sync surface would be
// satisfiable by a bulk-only fix, and asserting only the per-caller key would leave the static-token
// branch — the one that does a constant-time compare against a real secret — still accepting a bare
// credential.
func TestBearerScheme_SchemelessTokenIsRejected(t *testing.T) {
	s, _ := schemeFixture(t)

	// Every shape a scheme-less or wrong-scheme credential can take. The VALUES are real and correct:
	// each of these would authenticate if presented with the scheme, which is what makes the rejection
	// attributable to the scheme alone and not to a bad secret.
	cases := []struct {
		name   string
		header string
		why    string
	}{
		{"barePerCallerKey", "per-caller-key",
			"a valid per-caller key with no scheme must not authenticate (contract §1 clause 1)"},
		{"bareStaticToken", "legacy-static",
			"the legacy static token with no scheme must not authenticate either — it is compared on a " +
				"different branch (api.go:707) than the per-caller key, so one fix must cover both"},
		// The two below are GRAMMAR cases, not controls for this fix — measured under B110, both stay
		// green with the old fallback restored (the whole header is then compared as one opaque token
		// and matches nothing). They are kept because they pin the parser against a future rewrite that
		// strips a scheme it should not accept, but the file header records which cases detect what.
		{"wrongScheme", "Basic per-caller-key",
			"a DIFFERENT scheme is not the bearer scheme; accepting it would make the scheme check cosmetic"},
		{"schemeNameOnlyNoSpace", "Bearerper-caller-key",
			"the SP after the scheme name is part of the grammar (RFC 9110 §11.1); without it this is one " +
				"opaque token, not a bearer credential"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// (a) at the authenticate() seam, on BOTH surfaces.
			if _, _, ok := s.authenticate(rawAuthReq(tc.header), false); ok {
				t.Fatalf("%s: %s — authenticated on the SYNC surface", tc.name, tc.why)
			}
			if _, _, ok := s.authenticate(rawAuthReq(tc.header), true); ok {
				t.Fatalf("%s: %s — authenticated on the BULK surface", tc.name, tc.why)
			}

			// (b) through the real HTTP round trip, because a caller experiences the status code, not the
			// seam. This also pins that the refusal is the item-18 envelope: a scheme-less token must be
			// indistinguishable from a wrong token, so it cannot become an oracle for "this credential
			// exists but you spelled the header wrong".
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, rawAuthReq(tc.header))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: want 401, got %d %s", tc.name, rec.Code, rec.Body.String())
			}
			var env struct {
				Error struct {
					Code      string `json:"code"`
					Message   string `json:"message"`
					RequestID string `json:"request_id"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("%s: refusal must be the coded envelope: %v (body %s)", tc.name, err, rec.Body.String())
			}
			if env.Error.Code != codeUnauthenticated {
				t.Fatalf("%s: want code %q, got %q", tc.name, codeUnauthenticated, env.Error.Code)
			}
			if env.Error.RequestID == "" {
				t.Fatalf("%s: every error carries a request_id (§1.3)", tc.name)
			}
			// Two-vocabulary rule: the body must not echo the credential the caller presented, or a log
			// aggregator scraping 401 bodies collects live secrets.
			if strings.Contains(rec.Body.String(), "per-caller-key") || strings.Contains(rec.Body.String(), "legacy-static") {
				t.Fatalf("%s: the 401 envelope echoed the presented credential: %s", tc.name, rec.Body.String())
			}
		})
	}
}

// TestBearerScheme_SchemeMatchIsCaseInsensitive pins the half of the fix that is NOT implied by the
// laxness being closed.
//
// RFC 9110 §11.1 makes the auth scheme case-insensitive, so `bearer <t>` is a CONFORMING request.
// Both senders already accept it (coldmail-send-byo/.../platformapi/auth.go:56 and .../adminapi/auth.go
// use strings.EqualFold); M1 alone rejected it. Closing the bare-token laxness with a case-SENSITIVE
// CutPrefix would have traded one cross-module divergence for another, and nothing else in the suite
// would have noticed — which is exactly why this test exists as its own case rather than as an extra
// assertion on the test above.
func TestBearerScheme_SchemeMatchIsCaseInsensitive(t *testing.T) {
	s, _ := schemeFixture(t)

	for _, header := range []string{
		"Bearer per-caller-key", // the canonical spelling
		"bearer per-caller-key", // all-lower, what curl users and several clients send
		"BEARER per-caller-key", // all-upper
		"BeArEr per-caller-key", // mixed, to prove the comparison is EqualFold and not two literals
	} {
		id, owner, ok := s.authenticate(rawAuthReq(header), false)
		if !ok || id != "caller_scheme" || owner != userA {
			t.Fatalf("%q: a conforming request in any case must authenticate (RFC 9110 §11.1); ok=%v id=%q owner=%q",
				header, ok, id, owner)
		}
	}
}

// TestBearerScheme_SchemeCorrectTokenStillAuthenticates is the POSITIVE control. It is not redundant
// with the case test above, and the reason is MEASURED (break B113): neutering the static-token
// constant-time compare at api.go:709 reddens THIS test and leaves the case test green, because the
// case test only ever presents the per-caller key. Note the case test is also an authenticating test,
// so it too catches a blanket "reject everything" (B112) — what only this test catches is a regression
// in WHICH principal comes back and in the static branch.
//
// It also pins the owner, because the scheme fix touches the one line every credential passes through
// and a regression there could plausibly return ok with an empty principal — which on the bulk surface
// would mean a job stamped with a NULL owner, readable by nobody (0013's fail-safe direction, but
// still silent data loss for the submitter).
func TestBearerScheme_SchemeCorrectTokenStillAuthenticates(t *testing.T) {
	s, _ := schemeFixture(t)

	// (1) per-caller key, sync AND bulk, with the owner asserted.
	if id, owner, ok := s.authenticate(rawAuthReq("Bearer per-caller-key"), false); !ok || id != "caller_scheme" || owner != userA {
		t.Fatalf("per-caller key on sync: ok=%v id=%q owner=%q, want true/caller_scheme/%s", ok, id, owner, userA)
	}
	if id, owner, ok := s.authenticate(rawAuthReq("Bearer per-caller-key"), true); !ok || id != "caller_scheme" || owner != userA {
		t.Fatalf("per-caller key on bulk: ok=%v id=%q owner=%q", ok, id, owner)
	}

	// (2) the legacy static token still works on sync — the fix must close the SCHEME laxness without
	// retiring the transition credential (that is a separate, ruling-blocked 15b row).
	if _, owner, ok := s.authenticate(rawAuthReq("Bearer legacy-static"), false); !ok {
		t.Fatal("static token with a correct scheme was rejected — the fix broke the transition path")
	} else if owner != "" {
		t.Fatalf("the static token has no api_callers row and must resolve NO owner; got %q", owner)
	}

	// (3) surrounding whitespace is tolerated on the token, which the old TrimSpace also did. Asserted
	// so a future tightening of the parser has to face this case deliberately rather than break a
	// client that pads the header.
	if id, _, ok := s.authenticate(rawAuthReq("Bearer   per-caller-key  "), false); !ok || id != "caller_scheme" {
		t.Fatalf("padded token must still authenticate; ok=%v id=%q", ok, id)
	}

	// (4) ANTI-VACUITY on this whole file: a WRONG token with a CORRECT scheme must still be rejected.
	// Without this, every assertion above is satisfied by an authenticate() that accepts any header
	// beginning with "Bearer ", which would be a strictly worse bug than the one being fixed.
	if _, _, ok := s.authenticate(rawAuthReq("Bearer not-a-real-key"), false); ok {
		t.Fatal("a wrong token with a correct scheme authenticated — the scheme check replaced the secret check")
	}
	if _, _, ok := s.authenticate(rawAuthReq(""), false); ok {
		t.Fatal("a request with NO Authorization header authenticated")
	}
}
