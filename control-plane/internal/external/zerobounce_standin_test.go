package external

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/vocab"
	"github.com/coldmail-verify/control-plane/internal/zbtest"
)

// These tests drive the real adapter over a real HTTP connection against the ZeroBounce STAND-IN
// (control-plane/test/zbstub). There is no real vendor call and no real key. They need TM_TEST_ZEROBOUNCE_URL:
// unset, they FAIL in CI and SKIP locally (zbtest.URL), and ci.yml carries a paired control that proves the
// failing half. Each test counts only requests carrying its OWN tag, so packages running in parallel against
// one stand-in cannot disturb each other, and "no call was made" is provable as a count of zero.

func zbClientFor(t *testing.T, mutate func(*ZeroBounceConfig)) *ZeroBounce {
	t.Helper()
	cfg := ZeroBounceConfig{APIKey: zbTestKey, BaseURL: zbtest.URL(t), Timeout: 5 * time.Second, AllowLoopbackHTTP: true}
	if mutate != nil {
		mutate(&cfg)
	}
	z, err := NewZeroBounce(cfg)
	if err != nil {
		t.Fatalf("NewZeroBounce: %v", err)
	}
	return z
}

// EVERY answered row, over HTTP: the normalized result, the verbatim status string, one credit, the audit
// id, exactly one request, and a raw sidecar that kept no personal data. The stand-in's answer carries the
// address, a name, a location and a suggested address, so a projection that kept them would fail here.
func TestZB_Client_EveryAnsweredRow(t *testing.T) {
	zbtest.URL(t) // first: with no stand-in this must fail (CI) naming TM_TEST_ZEROBOUNCE_URL, before anything else runs
	z := zbClientFor(t, nil)
	for _, row := range zbWantRows {
		t.Run(row.behaviour(), func(t *testing.T) {
			tag := zbtest.Tag(t)
			addr := zbtest.Addr(row.behaviour(), tag, "gmail.com")
			r, err := z.Verify(context.Background(), addr, "idem-"+tag)
			if err != nil {
				t.Fatalf("an answered row returned %v", err)
			}
			if r.Result != row.result {
				t.Errorf("Result = %s, want %s", r.Result, row.result)
			}
			wantRaw := row.status
			if row.sub != "" {
				wantRaw += "/" + row.sub
			}
			if r.StatusRaw != wantRaw {
				t.Errorf("StatusRaw = %q, want %q (the vendor's own grade, verbatim)", r.StatusRaw, wantRaw)
			}
			if r.CostUnits != 1 || r.RequestID != "zb-idem-"+tag {
				t.Errorf("cost/request id = %d / %q", r.CostUnits, r.RequestID)
			}
			if got := zbtest.Calls(t, tag); got != 1 {
				t.Errorf("the stand-in saw %d requests, want exactly 1", got)
			}
			raw := string(r.RawJSON)
			for _, pii := range []string{addr, "firstname", "lastname", "gender", "didyoumean", "West Palm Beach", "33401", "Florida", "United States"} {
				if strings.Contains(raw, pii) {
					t.Errorf("the raw sidecar kept %q: %s", pii, raw)
				}
			}
			if !strings.Contains(raw, `"status":"`+row.status+`"`) || !strings.Contains(raw, `"mx_record":"mx.example.com"`) {
				t.Errorf("the raw sidecar lost its status or its kept fields: %s", raw)
			}
		})
	}
}

// Every pair that is NOT an answer — every `unknown`, every sub_status with no documented pairing, and a
// status or sub_status nobody has seen — is a non-answer: the call WAS made (and so is counted) but it yields
// no result, no cost marker and no raw blob. The control is the answered row beside it.
func TestZB_Client_NonAnswerRows_YieldNoVerdict(t *testing.T) {
	zbtest.URL(t)
	z := zbClientFor(t, nil)
	type row struct {
		behaviour string
		kind      ZBErrKind
	}
	var rows []row
	for _, sub := range zbDocSubs {
		b := "unknown--" + sub
		if sub == "" {
			b = "unknown"
		}
		rows = append(rows, row{b, ZBErrNoAnswer})
	}
	for _, b := range []string{
		"valid--accept_all", "valid--alternate", "valid--mx_forward", "valid--blocked", "valid--allowed", "valid--alias_address",
		"invalid--accept_all", "do_not_mail--allowed", "catch-all--gold", "invalid", "invalid--toxic", "spamtrap--toxic",
		"abuse--disposable", "valid--some_future_sub_status", "totally_new_status", "catch-all--role_based",
	} {
		rows = append(rows, row{b, ZBErrUnrecognised})
	}
	for _, rw := range rows {
		t.Run(rw.behaviour, func(t *testing.T) {
			tag := zbtest.Tag(t)
			r, err := z.Verify(context.Background(), zbtest.Addr(rw.behaviour, tag, "gmail.com"), "idem-"+tag)
			if err == nil {
				t.Fatalf("%s was read as an answer %+v", rw.behaviour, r)
			}
			if got := ZBKind(err); got != rw.kind {
				t.Errorf("kind = %q, want %q (err=%v)", got, rw.kind, err)
			}
			if r.Result != "" || r.CostUnits != 0 || r.StatusRaw != "" || len(r.RawJSON) != 0 || r.RequestID != "" {
				t.Errorf("a non-answer carried a result: %+v", r)
			}
			if got := zbtest.Calls(t, tag); got != 1 {
				t.Errorf("the stand-in saw %d requests, want 1 (the paid call was made; it just was not an answer)", got)
			}
		})
	}
	tag := zbtest.Tag(t)
	if r, err := z.Verify(context.Background(), zbtest.Addr("valid", tag, "gmail.com"), "idem-"+tag); err != nil || r.Result != vocab.StatusValid {
		t.Errorf("control: the same client must answer a mapped row, got %+v / %v", r, err)
	}
}

// Every failure mode the stand-in can play, against a FRESH client each (so a 429 pause cannot bleed into the
// next row). Each is a non-answer of the right kind, each cost exactly one request, none carries a result, and
// no error prints the key. The 302 row proves a redirect is not followed (a second request would be counted).
func TestZB_Client_FailureModes(t *testing.T) {
	zbtest.URL(t)
	cases := []struct {
		mode   string
		kind   ZBErrKind
		status int
	}{
		{"error_body_key", ZBErrErrorBody, 200},
		{"error_body_credits", ZBErrErrorBody, 200},
		{"error_body_echo_key", ZBErrErrorBody, 200},
		{"error_body_capital", ZBErrErrorBody, 200},
		{"success_false", ZBErrErrorBody, 200},
		{"message_only", ZBErrMalformed, 0},
		{"http_429", ZBErrRateLimited, 429},
		{"http_429_retry_after", ZBErrRateLimited, 429},
		{"http_500", ZBErrHTTPStatus, 500},
		{"http_503", ZBErrHTTPStatus, 503},
		{"http_403_html", ZBErrHTTPStatus, 403},
		{"http_302", ZBErrHTTPStatus, 302},
		{"malformed", ZBErrMalformed, 0},
		{"truncated", ZBErrTransport, 0},
		{"html_200", ZBErrMalformed, 0},
		{"empty_200", ZBErrMalformed, 0},
		{"empty_object", ZBErrMalformed, 0},
		{"array_body", ZBErrMalformed, 0},
		{"status_not_string", ZBErrMalformed, 0},
		{"no_address", ZBErrMalformed, 0},
		{"unknown_status", ZBErrUnrecognised, 0},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			z := zbClientFor(t, nil)
			tag := zbtest.Tag(t)
			r, err := z.Verify(context.Background(), zbtest.Addr("fail--"+c.mode, tag, "gmail.com"), "idem-"+tag)
			if err == nil {
				t.Fatalf("%s was read as an answer %+v", c.mode, r)
			}
			var ze *ZBError
			if !errors.As(err, &ze) || ze.Kind != c.kind || ze.HTTPStatus != c.status {
				t.Errorf("error = %v, want kind %q with HTTP status %d", err, c.kind, c.status)
			}
			if r.Result != "" || r.CostUnits != 0 || r.StatusRaw != "" || len(r.RawJSON) != 0 {
				t.Errorf("a non-answer carried a result: %+v", r)
			}
			if got := zbtest.Calls(t, tag); got != 1 {
				t.Errorf("the stand-in saw %d requests, want 1 (a redirect must not be followed, a failure must not be retried)", got)
			}
			if strings.Contains(err.Error(), zbTestKey) {
				t.Errorf("the error printed the key: %v", err)
			}
		})
	}
	// Paired control: the same fresh-client path answers a normal address, so each failure above is the
	// stand-in's mode and not a broken harness.
	z := zbClientFor(t, nil)
	tag := zbtest.Tag(t)
	if r, err := z.Verify(context.Background(), zbtest.Addr("valid", tag, "gmail.com"), "idem-"+tag); err != nil || r.Result != vocab.StatusValid {
		t.Errorf("control: a healthy call must answer, got %+v / %v", r, err)
	}
}

// A slow vendor is a non-answer at OUR deadline, not at theirs. The control is the same client answering a
// prompt address quickly.
func TestZB_Client_Timeout_IsANonAnswer(t *testing.T) {
	zbtest.URL(t)
	z := zbClientFor(t, func(c *ZeroBounceConfig) { c.Timeout = 300 * time.Millisecond })
	tag := zbtest.Tag(t)
	start := time.Now()
	_, err := z.Verify(context.Background(), zbtest.Addr("fail--slow", tag, "gmail.com"), "idem-"+tag)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a response slower than the timeout was read as an answer")
	}
	if ZBKind(err) != ZBErrTransport || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a timeout must be a transport non-answer carrying the deadline, got %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("the call took %s: the 300ms timeout did not bound it", elapsed)
	}
	if got := zbtest.Calls(t, tag); got != 1 {
		t.Errorf("the stand-in saw %d requests, want 1", got)
	}
	ctag := zbtest.Tag(t)
	start = time.Now()
	if r, err := z.Verify(context.Background(), zbtest.Addr("valid", ctag, "gmail.com"), "idem-"+ctag); err != nil || r.Result != vocab.StatusValid {
		t.Errorf("control: a prompt address must answer within the same timeout, got %+v / %v", r, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("control: the prompt call was slow")
	}
}

// After a 429 NO request is made until the pause ends (the counter proves it), and a Retry-After longer than
// the default is honoured. Paired controls: without the header the pause is the shorter default; and once the
// pause ends the very same call goes through.
func TestZB_Client_RateLimit_NoRequestDuringPause(t *testing.T) {
	zbtest.URL(t)
	run := func(t *testing.T, mode string, heldFor, freeAfter time.Duration) {
		t.Helper()
		now := time.Unix(1_700_000_000, 0)
		z := zbClientFor(t, func(c *ZeroBounceConfig) { c.Now = func() time.Time { return now } })
		tag := zbtest.Tag(t)
		ctx := context.Background()

		_, err := z.Verify(ctx, zbtest.Addr("fail--"+mode, tag, "gmail.com"), "idem-a-"+tag)
		var ze *ZBError
		if !errors.As(err, &ze) || ze.Kind != ZBErrRateLimited || ze.HTTPStatus != 429 {
			t.Fatalf("first call: want a 429 non-answer, got %v", err)
		}
		if got := zbtest.Calls(t, tag); got != 1 {
			t.Fatalf("after the 429 the stand-in saw %d requests, want 1", got)
		}

		now = now.Add(heldFor)
		_, err = z.Verify(ctx, zbtest.Addr("valid", tag, "gmail.com"), "idem-b-"+tag)
		if !errors.As(err, &ze) || ze.Kind != ZBErrRateLimited || ze.HTTPStatus != 0 {
			t.Fatalf("inside the pause: want a LOCAL rate_limited refusal, got %v", err)
		}
		if got := zbtest.Calls(t, tag); got != 1 {
			t.Fatalf("a request reached the stand-in during the pause: %d, want 1", got)
		}

		now = now.Add(freeAfter - heldFor)
		r, err := z.Verify(ctx, zbtest.Addr("valid", tag, "gmail.com"), "idem-c-"+tag)
		if err != nil || r.Result != vocab.StatusValid {
			t.Fatalf("after the pause the call must go through, got %+v / %v", r, err)
		}
		if got := zbtest.Calls(t, tag); got != 2 {
			t.Fatalf("after the pause the stand-in saw %d requests, want 2", got)
		}
	}
	t.Run("Retry-After: 120 holds for two minutes", func(t *testing.T) {
		run(t, "http_429_retry_after", 119*time.Second, 121*time.Second)
	})
	t.Run("no Retry-After holds for the bounded default only", func(t *testing.T) {
		run(t, "http_429", zbDefaultRatePause-time.Second, zbDefaultRatePause+time.Second)
	})
}

// Spend-once at the adapter: two CONCURRENT identical lookups cost ONE request and ONE credit; the follower
// shares the leader's answer with cost 0. The controls change one thing each: distinct addresses are NOT
// suppressed (two requests, two credits), and with the collapse switched off the identical pair is billed twice.
func TestZB_Client_ConcurrentIdentical_OneCharge(t *testing.T) {
	zbtest.URL(t)
	run := func(t *testing.T, noDedupe, same bool) (calls, cost int, res []VendorResult) {
		t.Helper()
		z := zbClientFor(t, nil)
		z.noDedupe = noDedupe
		tag := zbtest.Tag(t)
		a1 := "valid+dly400+" + tag + "@gmail.com"
		a2, k2 := a1, "idem-"+tag
		if !same {
			a2, k2 = "valid+dly400+"+tag+"+n2@gmail.com", "idem2-"+tag
		}
		type call struct{ addr, key string }
		start := make(chan struct{})
		var wg sync.WaitGroup
		res = make([]VendorResult, 2)
		errs := make([]error, 2)
		for i, c := range []call{{a1, "idem-" + tag}, {a2, k2}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				res[i], errs[i] = z.Verify(context.Background(), c.addr, c.key)
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
			cost += res[i].CostUnits
			if res[i].Result != vocab.StatusValid {
				t.Errorf("call %d: Result = %s", i, res[i].Result)
			}
		}
		return zbtest.Calls(t, tag), cost, res
	}
	t.Run("identical, collapse on: one request, one credit", func(t *testing.T) {
		calls, cost, res := run(t, false, true)
		if calls != 1 || cost != 1 {
			t.Fatalf("requests=%d credits=%d, want 1 and 1 (spend-once)", calls, cost)
		}
		if res[0].RequestID != res[1].RequestID || res[0].StatusRaw != res[1].StatusRaw {
			t.Errorf("the follower must share the leader's answer: %+v vs %+v", res[0], res[1])
		}
	})
	t.Run("control: distinct addresses are not suppressed", func(t *testing.T) {
		if calls, cost, _ := run(t, false, false); calls != 2 || cost != 2 {
			t.Fatalf("requests=%d credits=%d, want 2 and 2", calls, cost)
		}
	})
	t.Run("control: identical with the collapse OFF is billed twice", func(t *testing.T) {
		if calls, cost, _ := run(t, true, true); calls != 2 || cost != 2 {
			t.Fatalf("requests=%d credits=%d, want 2 and 2 (the collapse is the only thing that makes it one)", calls, cost)
		}
	})
}

// Address-only egress: the request carries api_key, email, an EMPTY ip_address and the vendor-side timeout —
// nothing else — and the timeout parameter follows the dial. The key's length is recorded, never the key.
func TestZB_Client_AddressOnlyEgress(t *testing.T) {
	zbtest.URL(t)
	for _, tc := range []struct {
		timeout time.Duration
		want    string
	}{{5 * time.Second, "4"}, {20 * time.Second, "19"}, {3 * time.Second, "3"}} {
		z := zbClientFor(t, func(c *ZeroBounceConfig) { c.Timeout = tc.timeout })
		tag := zbtest.Tag(t)
		addr := zbtest.Addr("valid", tag, "gmail.com")
		if _, err := z.Verify(context.Background(), addr, "idem-"+tag); err != nil {
			t.Fatal(err)
		}
		reqs := zbtest.Requests(t, tag)
		if len(reqs) != 1 {
			t.Fatalf("recorded %d requests, want 1", len(reqs))
		}
		got := reqs[0]
		if !reflect.DeepEqual(got.ParamNames, []string{"api_key", "email", "ip_address", "timeout"}) {
			t.Errorf("request parameters = %v, want exactly api_key, email, ip_address, timeout (address-only egress)", got.ParamNames)
		}
		if got.IPAddress != "" || got.Email != addr || got.Method != http.MethodGet || got.KeyLen != len(zbTestKey) {
			t.Errorf("request = %+v", got)
		}
		if got.Timeout != tc.want {
			t.Errorf("a %s client sent timeout=%q, want %q (kept just under our own deadline)", tc.timeout, got.Timeout, tc.want)
		}
	}
}

// The key must not appear in anything the adapter hands back, even when the VENDOR echoes it and even when the
// transport error's own text (Go's *url.Error) quotes it. Each hazard is first proven REAL — the stand-in does
// echo the key; Go's error does quote it — so a clean result is the adapter's doing, not the input's absence.
func TestZB_KeyNeverLeaks_AgainstStandIn(t *testing.T) {
	base := zbtest.URL(t)
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q"}

	// Hazard 1: a vendor error body that quotes the key.
	tag := zbtest.Tag(t)
	addr := zbtest.Addr("fail--error_body_echo_key", tag, "gmail.com")
	resp, err := http.Get(base + "/v2/validate?api_key=" + url.QueryEscape(zbTestKey) + "&email=" + url.QueryEscape(addr))
	if err != nil {
		t.Fatalf("raw GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), zbTestKey) {
		t.Fatal("control: the stand-in's echo mode should have put the key in the body (otherwise this test proves nothing)")
	}
	z := zbClientFor(t, nil)
	r, verr := z.Verify(context.Background(), addr, "idem-"+tag)
	if verr == nil {
		t.Fatalf("an error body was read as an answer: %+v", r)
	}
	for _, v := range verbs {
		for name, x := range map[string]any{"error": verr, "result": r, "client": z} {
			if got := fmt.Sprintf(v, x); strings.Contains(got, zbTestKey) {
				t.Errorf("%s under %s printed the key: %s", name, v, got)
			}
		}
	}

	// Hazard 2: a transport error. Go's own error quotes the whole URL, api_key included.
	dead := "http://127.0.0.1:1"
	_, rawErr := http.Get(dead + "/v2/validate?api_key=" + url.QueryEscape(zbTestKey) + "&email=x%40y.example")
	if rawErr == nil || !strings.Contains(rawErr.Error(), zbTestKey) {
		t.Fatalf("control: Go's transport error should quote the key (else this test proves nothing), got %v", rawErr)
	}
	dz, err := NewZeroBounce(ZeroBounceConfig{APIKey: zbTestKey, BaseURL: dead, AllowLoopbackHTTP: true, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, derr := dz.Verify(context.Background(), "x@y.example", "k")
	if ZBKind(derr) != ZBErrTransport {
		t.Fatalf("a dead port must be a transport non-answer, got %v", derr)
	}
	for _, v := range verbs {
		if got := fmt.Sprintf(v, derr); strings.Contains(got, zbTestKey) {
			t.Errorf("the transport error printed the key under %s: %s", v, got)
		}
		if got := fmt.Sprintf(v, errors.Unwrap(derr)); strings.Contains(got, zbTestKey) {
			t.Errorf("the unwrapped transport error printed the key under %s: %s", v, got)
		}
	}

	// And a SUCCESSFUL answer carries no key either.
	okTag := zbtest.Tag(t)
	ok, err := z.Verify(context.Background(), zbtest.Addr("valid", okTag, "gmail.com"), "idem-"+okTag)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range verbs {
		if got := fmt.Sprintf(v, ok); strings.Contains(got, zbTestKey) {
			t.Errorf("a successful result printed the key under %s: %s", v, got)
		}
	}
}
