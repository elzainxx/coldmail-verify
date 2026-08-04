package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// =============================================================================
// U2 / D12 — a DSN credential must never reach a log line (fix-plan 15b, escalated to Tier 0)
// =============================================================================
//
// THE DEFECT, verified first-hand against the PINNED pgx v5.10.0 rather than assumed. pgx embeds the
// entire connection string in ParseConfigError.Error(), and its own redaction is incomplete:
//
//	pgconn/errors.go redactPW  → takes the URL branch for any postgres:// DSN...
//	pgconn/errors.go redactURL → ...then returns u.String(), which re-emits RawQuery VERBATIM.
//	                             So ?password=  /  ?sslpassword= (both honored) are logged in clear.
//	pgconn/errors.go redactPW  → the keyword/value branch's `password=[^ ]*` cannot match the
//	                             honored spaced spelling `password = SECRET`.
//
// Measured, at the pin (TestDSNRedaction_PgxStillLeaksAtThisPin below re-measures it on every run):
//
//	postgres://u@h/db?password=SECRET      → LEAKS
//	postgres://u@h/db?sslpassword=SECRET   → LEAKS
//	host=h password = SECRET dbname=db     → LEAKS
//	host=h password=SECRET dbname=db       → redacted by pgx
//	postgres://u:SECRET@h/db               → redacted by pgx
//
// And M1's own code hands that error straight to log.Printf at boot
// (cmd/control-plane/main.go buildAPI), so the leak lands in the process log.
//
// WHY THE FIX IS IN OpenPG AND NOT AT THE LOG CALL. A fix at the call site protects exactly one
// caller; the next `log.Printf("%v", err)` anywhere reintroduces it. Scrubbing where the error is
// produced means no caller can hold an unsafe value in the first place.
//
// WHY THE CHECK IS DERIVED, NOT A PATTERN LIST. scrubDSNError asks "does this message contain a
// credential candidate from the DSN I was handed" — not "does it match a known-bad spelling". A
// blocklist of `password=` spellings is precisely how pgx's own redaction came to miss two of them,
// and a blocklist cannot cover a pgx release that leaks a field nobody has thought of yet.
// TestDSNRedaction_UnknownKeywordShapeStillCaught is that property, stated as a test.
//
// TWO INDEPENDENT CHECKS, AND WHY BOTH ARE HERE. scrubDSNError fires if the message contains the
// whole DSN *or* any credential candidate from it. For the errors pgx actually produces today the
// first check alone suffices — pgx echoes the DSN entire. The candidate check is what covers the case
// the whole-DSN check cannot: a driver that leaks a credential WITHOUT quoting the whole string (a
// future pgx, a wrapped error, a pool option error that names one field). That case is not reachable
// through OpenPG at this pin, so it is pinned directly against scrubDSNError in
// TestDSNRedaction_CandidateCheckStandsAlone — measured, not assumed: mutation testing showed that
// deleting either check alone left every OpenPG-level test green, which means an OpenPG-level test can
// never be the proof for either branch.
//
// FAIL-VERIFY (measured 2026-08-04, each mutation reverted with no residue):
//   - Drop the scrubDSNError call from either OpenPG return → 4 tests fail. CAUGHT.
//   - Make it scrub UNCONDITIONALLY → CleanErrorsKeepTheirDetail + ShortValuesDoNotSuppressEverything
//     fail (an unreachable-host error, the likelier failure, would lose the diagnosis). CAUGHT.
//   - Delete the bare-token branch of dsnCredentialCandidates → UnwrapSurvivesScrubbing and
//     CandidateCheckStandsAlone/kv-spaced fail. CAUGHT.
//   - Delete the URL-query branch → CandidateCheckStandsAlone/url-query fails. CAUGHT (and NOT caught
//     by any OpenPG-level test, which is why that test exists).
//   - Delete the whole-DSN containment check → WholeDSNCheckStandsAlone fails. CAUGHT (likewise
//     invisible at the OpenPG level).

const dsnCanary = "CANARYSECRET1234"

// leakyDSNs are the shapes proven to leak at this pin. A parse-stage failure is forced with a
// malformed pool_max_conns: that is a ParseConfig error, the only stage whose message carries the
// DSN, and forcing it deterministically is what makes these tests fast and network-free.
func leakyDSNs() map[string]string {
	return map[string]string{
		"url query password":    "postgres://u@nosuchhost.invalid:5432/db?password=" + dsnCanary + "&pool_max_conns=notanumber",
		"url query sslpassword": "postgres://u@nosuchhost.invalid:5432/db?sslpassword=" + dsnCanary + "&pool_max_conns=notanumber",
		"kv spaced password":    "host=nosuchhost.invalid password = " + dsnCanary + " dbname=db pool_max_conns=notanumber",
		// These two ARE redacted by pgx at this pin. They are included so the fix is proven to hold
		// for them too — if a future pgx regresses one, this test catches it without an edit.
		"kv unspaced password": "host=nosuchhost.invalid password=" + dsnCanary + " dbname=db pool_max_conns=notanumber",
		"url userinfo":         "postgres://u:" + dsnCanary + "@nosuchhost.invalid:5432/db?pool_max_conns=notanumber",
	}
}

// TestDSNRedaction_NoCredentialSurvivesOpenPG is the property itself: for every DSN shape that can
// carry a password, the error OpenPG returns must not contain it.
func TestDSNRedaction_NoCredentialSurvivesOpenPG(t *testing.T) {
	for name, dsn := range leakyDSNs() {
		t.Run(name, func(t *testing.T) {
			_, err := OpenPG(context.Background(), dsn)
			if err == nil {
				t.Fatal("expected a parse error from the malformed pool_max_conns — without one this case proves nothing")
			}
			if strings.Contains(err.Error(), dsnCanary) {
				t.Fatalf("U2: the DSN credential LEAKED into the error OpenPG returns (which main.go logs at boot):\n%s", err.Error())
			}
			// The whole DSN must not survive either — user/host/port/database are not credentials
			// but they are infrastructure detail with no business in a log line.
			if strings.Contains(err.Error(), dsn) {
				t.Fatalf("the whole DSN survived in the error text:\n%s", err.Error())
			}
		})
	}
}

// TestDSNRedaction_PgxStillLeaksAtThisPin is the ANTI-VACUITY control, and it is the important test
// in this file. It asserts the raw driver error still leaks — i.e. that the test above is measuring
// a defence and not merely a pgx version that never leaked. If a future pgx bump fixes redaction
// upstream, THIS test fails, and that failure is the signal to re-read the upstream code and decide
// whether the local scrub is still load-bearing (delete this file's premise, not just its assert).
func TestDSNRedaction_PgxStillLeaksAtThisPin(t *testing.T) {
	// Only the three shapes measured as leaking. The other two are pgx-redacted at this pin, so
	// asserting a leak for them would fail for the right reason and confuse the signal.
	for _, name := range []string{"url query password", "url query sslpassword", "kv spaced password"} {
		dsn := leakyDSNs()[name]
		raw := rawPoolError(t, dsn)
		if !strings.Contains(raw, dsnCanary) {
			t.Fatalf("PREMISE CHANGED for %q: the pinned pgx no longer leaks the credential in its own error.\n"+
				"raw error: %s\n"+
				"This is good news, not a bug — but re-read pgconn/errors.go redactPW/redactURL and decide whether\n"+
				"scrubDSNError is still needed before deleting anything. Do NOT just delete this assertion.", name, raw)
		}
	}
}

// rawPoolError returns the driver's UNSCRUBBED error text for a DSN, by calling the driver directly
// instead of through OpenPG.
func rawPoolError(t *testing.T, dsn string) string {
	t.Helper()
	// pgxpool.New is what OpenPG calls; calling it here bypasses the scrub.
	_, err := newPoolForTest(context.Background(), dsn)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	return err.Error()
}

// TestDSNRedaction_CleanErrorsKeepTheirDetail is the other half of the anti-vacuity pairing: a
// scrub that fired on everything would "pass" the leak tests while destroying every diagnosis. The
// likelier real-world failure at this call site is an unreachable database, whose message carries
// user/host/port but structurally CANNOT carry a password — and an operator needs to read it.
func TestDSNRedaction_CleanErrorsKeepTheirDetail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Port 1 on loopback: refused immediately, no DNS, no hang.
	_, err := OpenPG(ctx, "postgres://someuser:"+dsnCanary+"@127.0.0.1:1/somedb")
	if err == nil {
		t.Fatal("expected a connect failure against 127.0.0.1:1")
	}
	msg := err.Error()
	if strings.Contains(msg, dsnCanary) {
		t.Fatalf("the connect-stage error leaked the password: %s", msg)
	}
	if strings.Contains(msg, "message suppressed") {
		t.Fatalf("a CLEAN error was scrubbed — an unconditional scrub destroys the diagnosis for the "+
			"likeliest failure mode (DB unreachable). Got: %s", msg)
	}
	// And it must still say something useful.
	if !strings.Contains(msg, "127.0.0.1") {
		t.Fatalf("the connect error lost the host, so an operator cannot tell WHAT was unreachable: %s", msg)
	}
}

// TestDSNRedaction_UnknownKeywordShapeStillCaught pins the derived-not-enumerated property. A DSN
// keyword this code has never heard of still surrenders its value, because the check is "does the
// message contain one of MY secrets", not "does it match one of THEIR spellings".
func TestDSNRedaction_UnknownKeywordShapeStillCaught(t *testing.T) {
	// A plausible future/vendor keyword. It does not matter whether pgx honors it; what matters is
	// that IF a driver echoes the DSN, the value does not survive our scrub.
	dsn := "host=nosuchhost.invalid some_future_secret=" + dsnCanary + " dbname=db pool_max_conns=notanumber"
	_, err := OpenPG(context.Background(), dsn)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if strings.Contains(err.Error(), dsnCanary) {
		t.Fatalf("an unenumerated credential spelling leaked — the check has become a pattern list:\n%s", err.Error())
	}
	// Control: the raw driver error DOES contain it, so the assertion above is about our scrub.
	if raw := rawPoolError(t, dsn); !strings.Contains(raw, dsnCanary) {
		t.Skipf("premise: the driver did not echo this DSN shape, so nothing was there to scrub (raw: %s)", raw)
	}
}

// TestDSNRedaction_UnwrapSurvivesScrubbing: the scrub replaces the STRING, not the error. A caller
// testing for a driver sentinel with errors.Is/As must still succeed, or scrubbing a message would
// silently break error handling elsewhere.
func TestDSNRedaction_UnwrapSurvivesScrubbing(t *testing.T) {
	sentinel := errors.New("the original driver error mentioning " + dsnCanary)
	scrubbed := scrubDSNError(sentinel, "host=h password = "+dsnCanary+" dbname=db")
	if strings.Contains(scrubbed.Error(), dsnCanary) {
		t.Fatalf("scrubDSNError did not scrub: %s", scrubbed.Error())
	}
	if !errors.Is(scrubbed, sentinel) {
		t.Fatal("errors.Is must still reach the original error — the scrub replaces the message, not the error chain")
	}
	// And a nil error stays nil (OpenPG's success path must not manufacture one).
	if scrubDSNError(nil, "anything") != nil {
		t.Fatal("scrubDSNError(nil) must be nil")
	}
}

// TestDSNRedaction_ShortValuesDoNotSuppressEverything guards the over-collection tradeoff: candidate
// extraction takes every `key=value` value, so a DSN full of short values ("db", "5432", "disable")
// must not make every error look like a leak. A scrub that always fires is the same failure as a
// scrub that never fires — it just hides in the other direction.
func TestDSNRedaction_ShortValuesDoNotSuppressEverything(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// No credential anywhere; every value is short and generic.
	_, err := OpenPG(ctx, "postgres://ci@127.0.0.1:1/db?sslmode=disable")
	if err == nil {
		t.Fatal("expected a connect failure")
	}
	if strings.Contains(err.Error(), "message suppressed") {
		t.Fatalf("a passwordless DSN's error was suppressed — short generic values are matching as "+
			"credentials, which would blind every operator to every DB error: %s", err.Error())
	}
}

// TestDSNRedaction_CandidateCheckStandsAlone tests scrubDSNError DIRECTLY, with synthetic messages
// that contain ONLY a credential and never the whole DSN.
//
// WHY IT CANNOT BE AN OpenPG TEST. pgx at this pin echoes the DSN *entire*, so every OpenPG-level
// leak is already caught by the whole-DSN containment check alone — which means no OpenPG-level test
// can ever fail when a candidate branch is deleted. Mutation testing proved that: deleting the
// url-query branch left the entire suite green. The candidate machinery exists for the case the
// whole-DSN check structurally cannot cover — a driver that names one credential without quoting the
// connection string (a future pgx, a wrapped/reformatted error, a pool-option error about one field).
// That case is not reachable through pgx v5.10.0, so it is stated here against the unit instead of
// being left unproven. Each subtest is the sole proof for one extraction branch.
func TestDSNRedaction_CandidateCheckStandsAlone(t *testing.T) {
	cases := []struct {
		name   string
		dsn    string
		msg    string // a driver message carrying the credential but NOT the whole DSN
		branch string // the extraction branch this case is the sole proof for
	}{
		{
			name:   "userinfo password alone",
			dsn:    "postgres://u:" + dsnCanary + "@h.example:5432/db",
			msg:    "authentication failed for password " + dsnCanary,
			branch: "url.User.Password() — the keyword/value split cannot reach inside a URL authority (no space, '&' or '?' to split on), so without this branch only the WHOLE url is a candidate",
		},
		{
			name:   "url query value, percent-decoded",
			dsn:    "postgres://u@h.example:5432/db?password=CAN%20ARY" + dsnCanary,
			msg:    "connect: bad password \"CAN ARY" + dsnCanary + "\"",
			branch: "url.Query() — it decodes; the raw split yields CAN%20ARY..., which does not match a driver that reports the DECODED value it actually used",
		},
		{
			name:   "keyword/value, spaces around the '='",
			dsn:    "host=h.example password = " + dsnCanary + " dbname=db",
			msg:    "password authentication failed: " + dsnCanary,
			branch: "the bare-token branch — `password`, `=` and the value split into three fields, so the value only survives as a bare token",
		},
		{
			name:   "keyword/value, no spaces",
			dsn:    "host=h.example password=" + dsnCanary + " dbname=db",
			msg:    "password authentication failed: " + dsnCanary,
			branch: "the `key=value` branch — without it the whole `password=...` pair is the candidate and never matches a message quoting the value alone",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Premise of the test itself: the message must not contain the whole DSN, or the other
			// check would satisfy it and this subtest would prove nothing.
			if strings.Contains(tc.msg, tc.dsn) {
				t.Fatalf("bad fixture: the synthetic message contains the whole DSN, so this case cannot isolate the candidate check")
			}
			got := scrubDSNError(errors.New(tc.msg), tc.dsn)
			if strings.Contains(got.Error(), dsnCanary) {
				t.Fatalf("the credential survived a message that did NOT quote the whole DSN.\n"+
					"This is the branch that was supposed to catch it: %s\n"+
					"dsn: %s\nmsg: %s", tc.branch, tc.dsn, got.Error())
			}
		})
	}
}

// TestDSNRedaction_WholeDSNCheckStandsAlone is the mirror: the sole proof for the whole-DSN
// containment check. Every candidate in this DSN is under the 4-char floor, so candidate extraction
// returns nothing and only `strings.Contains(msg, dsn)` can fire.
//
// This is NOT in tension with TestDSNRedaction_ShortValuesDoNotSuppressEverything. That test says
// short values must not match INCIDENTALLY inside an unrelated message. This one says a message that
// quotes the connection string VERBATIM is not incidental — host, port, user and database are not
// credentials, but they are infrastructure detail with no business in a log line, and an error that
// reproduces the whole string is exactly the shape that also carries passwords.
func TestDSNRedaction_WholeDSNCheckStandsAlone(t *testing.T) {
	dsn := "host=h port=1 user=u dbname=d"
	if c := dsnCredentialCandidates(dsn); len(c) != 0 {
		t.Fatalf("bad fixture: this DSN was chosen because every value is under the length floor, "+
			"so that ONLY the whole-DSN check can fire — but candidates came back non-empty: %q", c)
	}
	got := scrubDSNError(errors.New(`cannot parse "`+dsn+`": invalid dbname`), dsn)
	if strings.Contains(got.Error(), dsn) {
		t.Fatalf("the whole connection string survived: %s", got.Error())
	}
}

// newPoolForTest calls the driver EXACTLY as OpenPG does, minus the scrub, so a control test can
// observe the raw leak. Kept in the test file (never in production code) so there is no unscrubbed
// path a caller could reach by accident.
func newPoolForTest(ctx context.Context, dsn string) (any, error) {
	return pgxpool.New(ctx, dsn)
}
