package external

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// The pure half of the ZeroBounce adapter: how ONE 200 response body becomes an answer or a non-answer,
// what the raw sidecar keeps, that the key cannot be printed, the base-URL rule, the 429 pause, and the
// hard daily spend cap in Decide. No stand-in needed; the stand-in tests (zerobounce_standin_test.go) prove
// the same behaviours over a real HTTP connection.

const zbTestKey = "ZBKEY-EXTERNAL-0000-0000" // a throwaway string; there is no real key in this repository

func kindOf(t *testing.T, err error) ZBErrKind {
	t.Helper()
	k := ZBKind(err)
	if k == "" {
		t.Fatalf("expected a *ZBError non-answer, got %v", err)
	}
	return k
}

// Every body shape that is NOT an answer, each next to a nearly-identical body that IS one: the error field,
// the bare Message, the wrong type, the missing address and the unmapped pair are what make the difference.
func TestZB_Interpret_NonAnswers_ResidueStands(t *testing.T) {
	good := `{"address":"a@b.example","status":"valid","sub_status":""}`
	cases := []struct {
		name string
		body string
		want ZBErrKind // "" => an ANSWER (the paired control)
	}{
		{"control: a plain valid answer", good, ""},
		{"control: null sub_status is the empty sub_status", `{"address":"a@b.example","status":"valid","sub_status":null}`, ""},
		{"control: absent sub_status is the empty sub_status", `{"address":"a@b.example","status":"valid"}`, ""},
		{"control: success:true is not a failure", `{"success":true,"address":"a@b.example","status":"valid"}`, ""},

		{"error body (invalid key / no credits)", `{"error":"Invalid API key"}`, ZBErrErrorBody},
		{"error body, capitalised", `{"Error":"Invalid API key"}`, ZBErrErrorBody},
		{"error_message body", `{"error_message":"x"}`, ZBErrErrorBody},
		{"an error field beats an otherwise valid-looking answer", `{"address":"a@b.example","status":"valid","error":"quota"}`, ZBErrErrorBody},
		{"success:false", `{"success":false,"message":"x"}`, ZBErrErrorBody},
		{"success:\"False\"", `{"success":"False"}`, ZBErrErrorBody},

		{"the bare greylist Message carries no status", `{"Message":"Greylist validation still in progress"}`, ZBErrMalformed},
		{"empty object", `{}`, ZBErrMalformed},
		{"a JSON array", `[]`, ZBErrMalformed},
		{"JSON null", `null`, ZBErrMalformed},
		{"not JSON", `this is not json`, ZBErrMalformed},
		{"empty body", ``, ZBErrMalformed},
		{"an HTML page", `<html><body>maintenance</body></html>`, ZBErrMalformed},
		{"truncated JSON", `{"address":"a@b.example","status":"va`, ZBErrMalformed},
		{"two JSON values", good + good, ZBErrMalformed},
		{"status is not a string", `{"address":"a@b.example","status":7}`, ZBErrMalformed},
		{"status is null", `{"address":"a@b.example","status":null}`, ZBErrMalformed},
		{"no address", `{"status":"valid","sub_status":""}`, ZBErrMalformed},
		{"sub_status is not a string", `{"address":"a@b.example","status":"valid","sub_status":5}`, ZBErrMalformed},

		{"unknown is the vendor's own non-answer", `{"address":"a@b.example","status":"unknown","sub_status":"greylisted"}`, ZBErrNoAnswer},
		{"unknown with no sub_status", `{"address":"a@b.example","status":"unknown"}`, ZBErrNoAnswer},
		{"a status this build has never seen", `{"address":"a@b.example","status":"totally_new_status"}`, ZBErrUnrecognised},
		{"a differently-cased status", `{"address":"a@b.example","status":"Valid"}`, ZBErrUnrecognised},
		{"a known status with an unseen sub_status", `{"address":"a@b.example","status":"valid","sub_status":"some_future_sub_status"}`, ZBErrUnrecognised},
		{"accept_all is a sub_status of valid only: under invalid it has no documented pairing", `{"address":"a@b.example","status":"invalid","sub_status":"accept_all"}`, ZBErrUnrecognised},
		{"mx_forward is a sub_status of do_not_mail only: under valid it has no documented pairing", `{"address":"a@b.example","status":"valid","sub_status":"mx_forward"}`, ZBErrUnrecognised},
		{"blocked is on neither the live page nor in any pair", `{"address":"a@b.example","status":"valid","sub_status":"blocked"}`, ZBErrUnrecognised},
		{"allowed is on neither the live page nor in any pair", `{"address":"a@b.example","status":"do_not_mail","sub_status":"allowed"}`, ZBErrUnrecognised},
		{"invalid with no sub_status (never shown by any SDK)", `{"address":"a@b.example","status":"invalid"}`, ZBErrUnrecognised},
	}
	answers, nonAnswers := 0, 0
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := interpretZeroBounce([]byte(c.body), "k", 7)
			if c.want == "" {
				answers++
				if err != nil {
					t.Fatalf("the control body must be an answer, got %v", err)
				}
				if r.Result != vocab.StatusValid || r.CostUnits != 1 || r.StatusRaw != "valid" {
					t.Errorf("answer = %+v, want valid / 1 credit / status_raw valid", r)
				}
				return
			}
			nonAnswers++
			if err == nil {
				t.Fatalf("body %q was read as an answer %+v: a response we did not understand must never become a verdict", c.body, r)
			}
			if got := kindOf(t, err); got != c.want {
				t.Errorf("kind = %s, want %s", got, c.want)
			}
			if r.Result != "" || r.CostUnits != 0 || r.StatusRaw != "" || len(r.RawJSON) != 0 {
				t.Errorf("a non-answer carried a result: %+v — no vendor-charged marker may appear", r)
			}
		})
	}
	if answers != 4 || nonAnswers < 20 {
		t.Fatalf("answers=%d non-answers=%d: the table did not exercise both sides", answers, nonAnswers)
	}
}

// EVERY answered pair through the interpreter on a body shaped like the vendor's, from the INDEPENDENT list
// (zbWantRows, typed from the doc): the normalized result, the verbatim "status/sub_status" grade, one credit.
// The four rows the live page added (valid/alias_address, valid/alternate, valid/accept_all, do_not_mail/
// mx_forward) are among them; each one's paired control is the same sub_status under a status that does not
// own it, which is a non-answer of kind unrecognised, and the four SDK-only pairs still answer.
func TestZB_Interpret_EveryAnsweredPair_AndTheLivePageAdditions(t *testing.T) {
	body := func(status, sub string) []byte {
		return []byte(fmt.Sprintf(`{"address":"a@b.example","status":%q,"sub_status":%q}`, status, sub))
	}
	for _, w := range zbWantRows {
		r, err := interpretZeroBounce(body(w.status, w.sub), "k", 1)
		if err != nil {
			t.Errorf("%s/%q is an answered pair but read as a non-answer: %v", w.status, w.sub, err)
			continue
		}
		wantRaw := w.status
		if w.sub != "" {
			wantRaw += "/" + w.sub
		}
		if r.Result != w.result || r.StatusRaw != wantRaw || r.CostUnits != 1 {
			t.Errorf("%s/%q = %+v, want result %s, status_raw %q, 1 credit", w.status, w.sub, r, w.result, wantRaw)
		}
	}
	for _, w := range zbPageAdded {
		for _, st := range zbDocStatuses {
			if st == w.status {
				continue
			}
			if r, err := interpretZeroBounce(body(st, w.sub), "k", 1); err == nil {
				t.Errorf("control: %s/%s was read as an answer %+v; %q is an answer only under %q", st, w.sub, r, w.sub, w.status)
			} else if k := ZBKind(err); k != ZBErrUnrecognised && !(st == "unknown" && k == ZBErrNoAnswer) {
				t.Errorf("control: %s/%s kind = %q, want unrecognised (no_answer for unknown)", st, w.sub, k)
			}
		}
	}
	for _, p := range [][2]string{{"valid", "gold"}, {"valid", "role_based_accept_all"}, {"valid", "ai_agent_mailbox"}, {"catch-all", "ai_agent_mailbox"}} {
		if _, err := interpretZeroBounce(body(p[0], p[1]), "k", 1); err != nil {
			t.Errorf("the SDK-only pair %s/%s must stay mapped, got %v", p[0], p[1], err)
		}
	}
	if len(zbWantRows) != 26 {
		t.Fatalf("zbWantRows has %d rows, want 26 (22 on the live page + 4 SDK-only)", len(zbWantRows))
	}
}

// The raw sidecar is a WHITELIST projection. The vendor's answer carries the address, the local part, a
// suggested address and data about the address's owner; none of it may be persisted (Door 3 forbids the raw
// address in storage). The control proves the PII really was in the input and that the projection is not
// simply empty.
func TestZB_Interpret_RawProjection_KeepsNoPersonalData(t *testing.T) {
	body := `{"address":"jane.doe+x@example.com","status":"do_not_mail","sub_status":"toxic","free_email":false,` +
		`"did_you_mean":"jane.doe@example.org","account":"jane.doe","domain":"example.com","domain_age_days":"9692",` +
		`"smtp_provider":"example","mx_found":"true","mx_record":"mx.example.com","firstname":"Jane","lastname":"Doe",` +
		`"gender":"female","country":"United States","region":"Florida","city":"West Palm Beach","zipcode":"33401",` +
		`"processed_at":"2023-04-28 15:18:45.631","nested":{"address":"deep@example.com"}}`
	for _, pii := range []string{"jane.doe", "Jane", "Doe", "female", "United States", "Florida", "West Palm Beach", "33401", "example.org", "deep@"} {
		if !strings.Contains(body, pii) {
			t.Fatalf("control: the input body should contain %q", pii)
		}
	}
	r, err := interpretZeroBounce([]byte(body), "idem1", 3)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(r.RawJSON)
	for _, pii := range []string{"jane.doe", "Jane", "Doe", "female", "United States", "Florida", "West Palm Beach", "33401", "example.org", "deep@", "@"} {
		if strings.Contains(raw, pii) {
			t.Errorf("the raw sidecar kept %q: %s", pii, raw)
		}
	}
	for _, keep := range []string{`"status":"do_not_mail"`, `"sub_status":"toxic"`, `"vendor":"zerobounce"`, `"mx_record":"mx.example.com"`, `"smtp_provider":"example"`} {
		if !strings.Contains(raw, keep) {
			t.Errorf("the projection lost %s: %s", keep, raw)
		}
	}
	if r.StatusRaw != "do_not_mail/toxic" || r.RequestID != "zb-idem1" || r.LatencyMS != 3 {
		t.Errorf("provenance = %+v", r)
	}
}

// A body that sneaks a nested object or array into a whitelisted key is dropped, not stored.
func TestZB_Interpret_RawProjection_DropsNonScalarsAndHugeValues(t *testing.T) {
	huge := strings.Repeat("x", 300)
	body := fmt.Sprintf(`{"address":"a@b.example","status":"valid","mx_record":{"a":"b"},"smtp_provider":["q"],"processed_at":"%s","free_email":true}`, huge)
	r, err := interpretZeroBounce([]byte(body), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(r.RawJSON)
	if strings.Contains(raw, "mx_record") || strings.Contains(raw, "smtp_provider") || strings.Contains(raw, huge) {
		t.Errorf("a non-scalar or oversized value reached the sidecar: %s", raw)
	}
	if !strings.Contains(raw, `"free_email":true`) {
		t.Errorf("control: a short scalar must be kept: %s", raw)
	}
	if r.RequestID != "zb" {
		t.Errorf("RequestID without an idempotency key = %q, want zb", r.RequestID)
	}
}

// fmt of the client, its config and every kind of error must not print the key — under %v, %+v, %#v, %s and
// %q. The control proves the grep can see a key: the SAME formatting of a plain struct that holds it DOES leak.
func TestZB_KeyNeverLeaks_Formatting(t *testing.T) {
	type naive struct{ APIKey string }
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if got := fmt.Sprintf(verb, naive{APIKey: zbTestKey}); !strings.Contains(got, zbTestKey) {
			t.Fatalf("control: %s of a plain struct should show the key (else this test could not detect a leak), got %q", verb, got)
		}
	}

	z, err := NewZeroBounce(ZeroBounceConfig{APIKey: zbTestKey, BaseURL: "https://api.zerobounce.net", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	cfg := ZeroBounceConfig{APIKey: zbTestKey, BaseURL: "https://api.zerobounce.net", Timeout: 5 * time.Second}
	values := map[string]any{"client": z, "config": cfg, "config pointer": &cfg}
	for _, k := range []ZBErrKind{ZBErrTransport, ZBErrRateLimited, ZBErrHTTPStatus, ZBErrErrorBody, ZBErrMalformed, ZBErrUnrecognised, ZBErrNoAnswer, ZBErrRefused} {
		values["error "+string(k)] = &ZBError{Kind: k, HTTPStatus: 429, cause: context.DeadlineExceeded}
		values["wrapped "+string(k)] = fmt.Errorf("stage: %w", &ZBError{Kind: k})
	}
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			if got := fmt.Sprintf(verb, v); strings.Contains(got, zbTestKey) {
				t.Errorf("%s under %s printed the API key: %s", name, verb, got)
			}
		}
	}
	// The redaction still says something useful (and says it is set), so an operator can tell it loaded.
	if s := z.String(); !strings.Contains(s, "<set>") || !strings.Contains(s, "api.zerobounce.net") {
		t.Errorf("String() = %q, want the endpoint and <set>", s)
	}
	if s := (ZeroBounceConfig{}).String(); !strings.Contains(s, "<unset>") {
		t.Errorf("an empty config must read <unset>, got %q", s)
	}
}

// Every error text is static: kind plus an HTTP status integer. Nothing the network or the vendor supplied.
func TestZB_ErrorText_IsStatic(t *testing.T) {
	if got := (&ZBError{Kind: ZBErrRateLimited, HTTPStatus: 429}).Error(); got != "external: zerobounce non-answer (rate_limited, http 429)" {
		t.Errorf("Error() = %q", got)
	}
	if got := (&ZBError{Kind: ZBErrMalformed}).Error(); got != "external: zerobounce non-answer (malformed)" {
		t.Errorf("Error() = %q", got)
	}
	e := &ZBError{Kind: ZBErrTransport, cause: context.DeadlineExceeded}
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Error("a transport error must expose a deadline, and only that, through Unwrap")
	}
	if ZBKind(errors.New("something else")) != "" {
		t.Error("ZBKind of a foreign error must be empty")
	}
}

func TestZB_NewZeroBounce_Rejects(t *testing.T) {
	ok := ZeroBounceConfig{APIKey: zbTestKey, BaseURL: "https://api.zerobounce.net"}
	if _, err := NewZeroBounce(ok); err != nil {
		t.Fatalf("control: a valid config must build: %v", err)
	}
	for name, mut := range map[string]func(*ZeroBounceConfig){
		"empty key":        func(c *ZeroBounceConfig) { c.APIKey = "" },
		"blank key":        func(c *ZeroBounceConfig) { c.APIKey = "   " },
		"timeout too long": func(c *ZeroBounceConfig) { c.Timeout = 61 * time.Second },
		"negative timeout": func(c *ZeroBounceConfig) { c.Timeout = -time.Second },
		"http to a vendor": func(c *ZeroBounceConfig) { c.BaseURL = "http://api.zerobounce.net" },
	} {
		c := ok
		mut(&c)
		_, err := NewZeroBounce(c)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), zbTestKey) {
			t.Errorf("%s: the error printed the key: %v", name, err)
		}
	}
	z, err := NewZeroBounce(ZeroBounceConfig{APIKey: zbTestKey})
	if err != nil {
		t.Fatal(err)
	}
	if z.Name() != "zerobounce" || z.Timeout() != ZeroBounceDefaultTimeout || z.Endpoint() != "https://api.zerobounce.net/v2/validate" {
		t.Errorf("defaults: name=%q timeout=%s endpoint=%q", z.Name(), z.Timeout(), z.Endpoint())
	}
}

// The base-URL rule: https always; http only for a loopback / fixture host AND only when the caller says it is
// development/test; no credentials, path, query or fragment. The error never echoes the value.
func TestZB_BaseURL_Validation(t *testing.T) {
	type c struct {
		raw   string
		allow bool
		ok    bool
	}
	for _, tc := range []c{
		{"https://api.zerobounce.net", false, true},
		{"https://api-eu.zerobounce.net", false, true},
		{"https://api.zerobounce.net/", false, true},
		{"", false, true}, // empty => the default
		{"https://api.zerobounce.net:8443", false, true},
		{"http://127.0.0.1:18081", true, true},
		{"http://localhost:8080", true, true},
		{"http://[::1]:8080", true, true},
		{"http://zbstub:8080", true, true},
		// refused
		{"http://127.0.0.1:18081", false, false},               // loopback http in production
		{"http://api.zerobounce.net", true, false},             // the real vendor over http, even in development
		{"http://evil.example.com", true, false},               // a non-fixture host
		{"http://10.0.0.5:8080", true, false},                  // private, but not loopback
		{"ftp://api.zerobounce.net", false, false},             // wrong scheme
		{"https://user:pass@api.zerobounce.net", false, false}, // credentials
		{"https://api.zerobounce.net/v2", false, false},        // a path
		{"https://api.zerobounce.net?x=1", false, false},       // a query
		{"https://api.zerobounce.net#f", false, false},         // a fragment
		{"https://", false, false},                             // no host
		{"://bad", false, false},                               // unparseable
	} {
		u, err := ParseZeroBounceBaseURL(tc.raw, tc.allow)
		if (err == nil) != tc.ok {
			t.Errorf("ParseZeroBounceBaseURL(%q, allowLoopbackHTTP=%v) err=%v, want ok=%v", tc.raw, tc.allow, err, tc.ok)
			continue
		}
		if err != nil {
			if strings.Contains(err.Error(), "pass") || strings.Contains(err.Error(), tc.raw) && tc.raw != "" {
				t.Errorf("the error echoed the value: %v", err)
			}
			continue
		}
		if u.Path != "" || u.RawQuery != "" || u.User != nil {
			t.Errorf("the parsed URL must be scheme://host only, got %v", u)
		}
	}
}

// The 429 pause: a Retry-After is honoured within bounds, its absence gets a bounded default, and during the
// pause no request is made (paused() is what Verify consults before it touches the network).
func TestZB_RateLimitPause(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	mk := func() *ZeroBounce {
		z, err := NewZeroBounce(ZeroBounceConfig{APIKey: zbTestKey, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return z
	}
	z := mk()
	if z.paused() {
		t.Fatal("control: a client that has never seen a 429 is not paused")
	}
	for _, tc := range []struct {
		header  string
		lasts   time.Duration // the pause must hold just before this and release just after
		comment string
	}{
		{"", zbDefaultRatePause, "no header: the bounded default"},
		{"garbage", zbDefaultRatePause, "an unreadable header: the default"},
		{"120", 120 * time.Second, "seconds"},
		{"0", zbMinRatePause, "zero is raised to the minimum"},
		{"99999", zbMaxRatePause, "an absurd value is clamped to the maximum"},
		{now.Add(30 * time.Second).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"), 30 * time.Second, "an HTTP date"},
	} {
		z := mk()
		z.pause(tc.header)
		now = time.Unix(1_700_000_000, 0).Add(tc.lasts - time.Second)
		if !z.paused() {
			t.Errorf("%s: not paused 1s before the end", tc.comment)
		}
		now = time.Unix(1_700_000_000, 0).Add(tc.lasts + time.Second)
		if z.paused() {
			t.Errorf("%s: still paused 1s after the end", tc.comment)
		}
		now = time.Unix(1_700_000_000, 0)
	}
	// A refused-during-pause Verify makes no request and reports rate_limited without an HTTP status.
	z = mk()
	z.pause("120")
	_, err := z.Verify(context.Background(), "x@gmail.com", "k")
	if kindOf(t, err) != ZBErrRateLimited || err.(*ZBError).HTTPStatus != 0 {
		t.Errorf("a call inside the pause must be refused locally as rate_limited, got %v", err)
	}
}

// An empty address is refused before anything is sent.
func TestZB_Verify_RefusesEmptyAddress(t *testing.T) {
	z, err := NewZeroBounce(ZeroBounceConfig{APIKey: zbTestKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := z.Verify(context.Background(), "  ", "k"); kindOf(t, err) != ZBErrRefused {
		t.Errorf("an empty address must be refused, got %v", err)
	}
}

// The hard per-vendor daily cap in Decide. 0 means "no calls" ONLY when the cap is enforced (the vendor's
// wiring); a zero Budget is unchanged, so every existing caller keeps its behavior.
func TestZB_Decide_VendorDailyCap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := BreakerParams{}
	capped := func(n int) Budget { return Budget{VendorDailyCap: n, VendorDailyCapEnforced: true} }

	if d := Decide(capped(0), p, SpendFacts{}, now); d.Allow || d.Reason != TripVendorCap {
		t.Errorf("an enforced cap of 0 must allow NO calls, got %+v", d)
	}
	// The paired control: the SAME facts with no enforced cap are allowed (the old behavior, unchanged).
	if d := Decide(Budget{}, p, SpendFacts{}, now); !d.Allow {
		t.Errorf("a zero Budget must not enforce anything, got %+v", d)
	}
	if d := Decide(Budget{VendorDailyCap: 0}, p, SpendFacts{}, now); !d.Allow {
		t.Errorf("a cap value without the enforced flag must not enforce, got %+v", d)
	}
	// N allowed, the N+1th refused.
	for spent := 0; spent < 5; spent++ {
		if d := Decide(capped(5), p, SpendFacts{SpentDay: spent}, now); !d.Allow {
			t.Errorf("spent %d of 5: must allow, got %+v", spent, d)
		}
	}
	if d := Decide(capped(5), p, SpendFacts{SpentDay: 5}, now); d.Allow || d.Reason != TripVendorCap {
		t.Errorf("spent 5 of 5: the next call must be refused, got %+v", d)
	}
	// Relationship to the owner-OQ DailyCap: whichever is lower wins, each with its own reason.
	b := Budget{DailyCap: 3, VendorDailyCap: 10, VendorDailyCapEnforced: true}
	if d := Decide(b, p, SpendFacts{SpentDay: 3}, now); d.Allow || d.Reason != TripBudgetDay {
		t.Errorf("the lower owner-OQ DailyCap must win at 3 spent, got %+v", d)
	}
	b = Budget{DailyCap: 10, VendorDailyCap: 3, VendorDailyCapEnforced: true}
	if d := Decide(b, p, SpendFacts{SpentDay: 3}, now); d.Allow || d.Reason != TripVendorCap {
		t.Errorf("the lower vendor cap must win at 3 spent, got %+v", d)
	}
}
