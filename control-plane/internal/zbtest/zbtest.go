// Package zbtest is the test-side client of the ZeroBounce stand-in (control-plane/test/zbstub). It is
// imported only by _test.go files: it lets the adapter's tests, the orchestrator's stage tests and the
// control-plane's wiring tests share ONE way of finding the stand-in, addressing it and reading its
// request counters.
//
// A test that needs the stand-in calls URL(t). If TM_TEST_ZEROBOUNCE_URL is unset it FAILS in CI and SKIPS
// elsewhere — the same rule the test-execution gate enforces for the database-backed tests, and for the
// same reason: a skip reads as green, so in CI the absence of the fixture must be the failure, not a reason
// to check less. CI keys on the standard markers (GITHUB_ACTIONS / CI), the way scripts/check-test-execution.sh does.
//
// Every test takes its OWN tag (Tag) and puts it in the address, and counts only that tag. Packages run in
// parallel against one stand-in, so a global counter would be read by whichever test ran last; a per-tag
// counter is also what makes "this call was never made" provable (the count of THIS test's tag is 0).
package zbtest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// EnvURL is the variable CI sets to the stand-in's base URL (scheme://host:port).
const EnvURL = "TM_TEST_ZEROBOUNCE_URL"

// URL returns the stand-in's base URL, failing (in CI) or skipping (elsewhere) when it is not configured.
func URL(t testing.TB) string {
	t.Helper()
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv(EnvURL)), "/"); v != "" {
		return v
	}
	if os.Getenv("GITHUB_ACTIONS") != "" || os.Getenv("CI") != "" {
		t.Fatalf("%s is not set in CI: the ZeroBounce stand-in (control-plane/test/zbstub) must be running and its address handed to the tests. "+
			"A test that cannot reach its fixture FAILS in CI; it never skips (a skip reads as green)", EnvURL)
	}
	t.Skipf("%s is not set: start the stand-in (go run ./test/zbstub, or the zbstub image) and point it here. Skipped locally only; CI fails instead", EnvURL)
	return ""
}

// Tag returns a fresh, collision-free tag for one test (lower-case letters and digits only, so it is legal
// in a local part and survives the address normalization).
func Tag(t testing.TB) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("zbtest: tag: %v", err)
	}
	return "t" + hex.EncodeToString(b)
}

// Addr builds the address that makes the stand-in behave as `behaviour` for requests tagged `tag`, e.g.
// Addr("do_not_mail--toxic", tag, "gmail.com") = do_not_mail--toxic+<tag>@gmail.com. Extra "+"-segments in
// behaviour (e.g. "valid+dly400") are passed through.
func Addr(behaviour, tag, domain string) string {
	return behaviour + "+" + tag + "@" + domain
}

var httpc = &http.Client{Timeout: 15 * time.Second}

func get(t testing.TB, rawURL string) []byte {
	t.Helper()
	resp, err := httpc.Get(rawURL)
	if err != nil {
		t.Fatalf("zbtest: stand-in unreachable at %s: %v", EnvURL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("zbtest: reading the stand-in's answer: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("zbtest: stand-in answered HTTP %d to a counter read", resp.StatusCode)
	}
	return b
}

// Calls is the number of /v2/validate requests the stand-in has received for `tag`, counted on arrival
// whatever they were answered with.
func Calls(t testing.TB, tag string) int {
	t.Helper()
	var out struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(get(t, URL(t)+"/__calls?tag="+url.QueryEscape(tag)), &out); err != nil {
		t.Fatalf("zbtest: decoding /__calls: %v", err)
	}
	return out.Count
}

// Request is one request the stand-in recorded. It carries the parameter NAMES and the key's length, never
// the key itself.
type Request struct {
	Tag        string   `json:"tag"`
	Email      string   `json:"email"`
	ParamNames []string `json:"param_names"`
	IPAddress  string   `json:"ip_address"`
	Timeout    string   `json:"timeout"`
	KeyLen     int      `json:"key_len"`
	Method     string   `json:"method"`
}

// Requests returns every request recorded for `tag`, in arrival order.
func Requests(t testing.TB, tag string) []Request {
	t.Helper()
	var out []Request
	if err := json.Unmarshal(get(t, URL(t)+"/__requests?tag="+url.QueryEscape(tag)), &out); err != nil {
		t.Fatalf("zbtest: decoding /__requests: %v", err)
	}
	return out
}
