package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file: the ZeroBounce HTTP adapter behind the Vendor interface (§8). It is the first real vendor
// client and it SHIPS DISABLED: buildExternal wires it only when every independent gate is affirmative
// (kill-switch rung, vendor key present, DPA confirmed), and even then Decide refuses every call until
// a daily spend cap above zero is set (TM_EXTERNAL_DAILY_SPEND_CAP defaults to 0 = no calls). Nothing
// here decides whether a call may be made — that is Decide (budget, breaker) and the stage's gates;
// this file decides only what ONE call means.
//
// THE CONTRACT WITH THE STAGE (external_stage.go):
//
//   - An ANSWER is a VendorResult whose Result is valid|invalid|risky (the normalization table in
//     zerobounce_map.go) with CostUnits=1. The stage then runs Resolve (anti-laundering, OQ-P6-3) and
//     the call is recorded outcome=ok with its cost.
//   - EVERYTHING ELSE is a NON-ANSWER and is returned as a *ZBError. The stage records it as
//     outcome=error, cost 0 — the breaker's error window sees it, NO vendor-charged marker is written,
//     and M1's own residue verdict stands unchanged. A non-answer is: any non-200, a 429, a 200 whose
//     body carries an error field, an unparseable or truncated body, a body with no status, a status
//     of `unknown`, and any (status, sub_status) the table does not list. We NEVER guess a verdict out
//     of a response we did not understand.
//
// UNVERIFIED against the live documentation (Docs/vendor-zerobounce.md, "UNVERIFIED"): whether
// ZeroBounce charges for an `unknown` or for a call that timed out, the 429 / Retry-After contract, and
// the exact error-body text. The code is conservative about each (a non-answer; a bounded pause).
//
// THE KEY. It rides in the query string (ZeroBounce's only documented way) so it WOULD appear in any
// error that quotes the request URL — Go's own *url.Error does. So no error from this file is ever
// built from a transport error's text, a response body, or the URL: every ZBError message is a static
// string plus an HTTP status integer. The client, its config and every error redact themselves under
// %v, %+v and %#v (TestZB_KeyNeverLeaks), redirects are never followed (the key travels with the
// request), and the stored `raw` projection is built from a whitelist of fields, so a response that
// echoes the address, the key or other personal data cannot reach the sidecar.
//
// SPEND-ONCE. ZeroBounce offers no idempotency key, so two concurrent calls for the same address (the
// same email_hash ‖ vendor key) would be billed twice. Concurrent identical calls are therefore
// collapsed in-process onto ONE request; followers share the leader's answer with CostUnits=0 (charged
// once). A P5 lease-loss RETRY after the first call returned is not collapsed (the answer is not
// retained), which is the bounded one-extra-lookup worst case the design already accepts (§5.4).
const (
	// ZeroBounceVendorName is Vendor.Name(): the budget/breaker/spend key for this adapter. The shipped
	// policy's external.vendor field still names a different vendor; it is informational — the stage
	// uses the compiled vendor (Docs/vendor-zerobounce.md).
	ZeroBounceVendorName = "zerobounce"

	// ZeroBounceDefaultBaseURL is the global API host. ZeroBounce also publishes api-us and api-eu hosts
	// (Postman collection); pick one with TM_EXTERNAL_ZEROBOUNCE_BASE_URL.
	ZeroBounceDefaultBaseURL = "https://api.zerobounce.net"

	// ZeroBounceDefaultTimeout / Min / Max are the per-call timeout bounds. 3s..60s mirrors the range the
	// Postman collection documents for the `timeout` query parameter.
	ZeroBounceDefaultTimeout = 8 * time.Second
	ZeroBounceMinTimeout     = 3 * time.Second
	ZeroBounceMaxTimeout     = 60 * time.Second

	zbValidatePath = "/v2/validate"
	zbMaxBody      = 64 << 10 // a validate answer is ~1 KiB; anything near this is not one
	zbUserAgent    = "coldmail-verify-control-plane/1"

	// 429 handling. UNVERIFIED: no SDK states a Retry-After contract, so a bounded pause is taken
	// whether or not the header is present, and during it NO request is made.
	zbDefaultRatePause = 10 * time.Second
	zbMinRatePause     = 1 * time.Second
	zbMaxRatePause     = 5 * time.Minute
)

// ZBErrKind classifies a non-answer. It is the whole of what an error says about WHY — never a
// response body, never a URL.
type ZBErrKind string

const (
	ZBErrTransport    ZBErrKind = "transport"    // connect/TLS/timeout/truncated body: no usable response
	ZBErrRateLimited  ZBErrKind = "rate_limited" // HTTP 429, or a call refused locally inside a 429 pause
	ZBErrHTTPStatus   ZBErrKind = "http_status"  // any other non-200 (403 from the edge, 5xx, a refused redirect)
	ZBErrErrorBody    ZBErrKind = "error_body"   // HTTP 200 whose body reports a failure (invalid key, no credits)
	ZBErrMalformed    ZBErrKind = "malformed"    // not JSON, wrong shape, no status/address, oversized
	ZBErrUnrecognised ZBErrKind = "unrecognised" // a (status, sub_status) outside the table
	ZBErrNoAnswer     ZBErrKind = "no_answer"    // the vendor said `unknown`
	ZBErrRefused      ZBErrKind = "refused"      // the call was not made (empty address)
)

// ZBError is a non-answer. Its message is static: kind plus an HTTP status integer, nothing the
// network or the vendor supplied.
type ZBError struct {
	Kind       ZBErrKind
	HTTPStatus int // 0 unless the kind carries one
	cause      error
}

func (e *ZBError) Error() string {
	if e.HTTPStatus != 0 {
		return "external: zerobounce non-answer (" + string(e.Kind) + ", http " + strconv.Itoa(e.HTTPStatus) + ")"
	}
	return "external: zerobounce non-answer (" + string(e.Kind) + ")"
}

// Unwrap exposes only a context error (deadline / cancellation); it is never a transport error.
func (e *ZBError) Unwrap() error { return e.cause }

// ZBKind returns the kind of a non-answer, or "" when err is not a *ZBError.
func ZBKind(err error) ZBErrKind {
	var ze *ZBError
	if errors.As(err, &ze) {
		return ze.Kind
	}
	return ""
}

// ZeroBounceConfig is the client's construction input. It holds the API key, so it redacts itself.
type ZeroBounceConfig struct {
	APIKey  string        // the key VALUE, read by the caller from the env var the dial names
	BaseURL string        // scheme://host[:port]; "" => ZeroBounceDefaultBaseURL
	Timeout time.Duration // per call; 0 => ZeroBounceDefaultTimeout; bounded 3s..60s

	// AllowLoopbackHTTP permits an http:// base URL for a loopback or test-fixture host. The caller
	// derives it from TM_ENV (development/test only); it is false in production.
	AllowLoopbackHTTP bool

	Now        func() time.Time // injected clock for the 429 pause; nil => time.Now
	HTTPClient *http.Client     // optional; nil => a dedicated client that never follows redirects
}

func (c ZeroBounceConfig) String() string {
	return "ZeroBounceConfig{key:" + redactedKey(c.APIKey) + " base:" + safeBase(c.BaseURL) + " timeout:" + c.Timeout.String() + "}"
}

// safeBase shows only scheme://host of a base URL, so a value that (wrongly) carried credentials cannot be
// printed through the config.
func safeBase(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "<invalid>"
	}
	return u.Scheme + "://" + u.Host
}

// GoString covers %#v, which would otherwise dump every field including the key.
func (c ZeroBounceConfig) GoString() string { return c.String() }

func redactedKey(k string) string {
	if k == "" {
		return "<unset>"
	}
	return "<set>"
}

// ParseZeroBounceBaseURL validates the base URL dial and returns it reduced to scheme://host[:port].
// https is required. http is accepted ONLY when allowLoopbackHTTP is true AND the host is a loopback
// address or the test-fixture host name, i.e. only under TM_ENV=development/test. The error text never
// echoes the value (it may carry credentials).
func ParseZeroBounceBaseURL(raw string, allowLoopbackHTTP bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = ZeroBounceDefaultBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("external: zerobounce base URL is not a valid URL")
	}
	if u.Hostname() == "" {
		return nil, errors.New("external: zerobounce base URL has no host")
	}
	if u.User != nil {
		return nil, errors.New("external: zerobounce base URL must not carry credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("external: zerobounce base URL must be scheme://host[:port] only (no path, query or fragment)")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowLoopbackHTTP || !isFixtureHost(u.Hostname()) {
			return nil, errors.New("external: zerobounce base URL must be https (http is accepted only for a loopback or fixture host under TM_ENV=development or test)")
		}
	default:
		return nil, errors.New("external: zerobounce base URL must be https")
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// isFixtureHost reports a host an http:// base URL may point at: loopback, or the stand-in's container
// name. A real vendor is never on this list, so the key cannot be sent in clear to one.
func isFixtureHost(h string) bool {
	h = strings.ToLower(h)
	if h == "localhost" || h == "zbstub" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ZeroBounce is the Vendor adapter for ZeroBounce's GET /v2/validate. Safe for concurrent use.
type ZeroBounce struct {
	key      string // never logged, never in an error; see the file comment
	endpoint string // scheme://host[:port]/v2/validate (no query)
	timeout  time.Duration
	httpc    *http.Client
	now      func() time.Time

	mu         sync.Mutex
	pauseUntil time.Time
	inflight   map[string]*zbFlight
	noDedupe   bool // test-only paired control: switches the in-flight collapse off
}

type zbFlight struct {
	done chan struct{}
	res  VendorResult
	err  error
}

// NewZeroBounce builds the adapter. Errors are static (they never contain the key or the URL).
func NewZeroBounce(cfg ZeroBounceConfig) (*ZeroBounce, error) {
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, errors.New("external: zerobounce API key is empty")
	}
	base, err := ParseZeroBounceBaseURL(cfg.BaseURL, cfg.AllowLoopbackHTTP)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = ZeroBounceDefaultTimeout
	}
	if timeout < 0 || timeout > ZeroBounceMaxTimeout {
		return nil, errors.New("external: zerobounce timeout must be at most 60s")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	httpc := cfg.HTTPClient
	if httpc == nil {
		tr, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			tr = &http.Transport{Proxy: http.ProxyFromEnvironment}
		} else {
			tr = tr.Clone()
		}
		httpc = &http.Client{
			Transport: tr,
			// The key travels with the request, so a redirect could carry it to another host. Never follow.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &ZeroBounce{
		key:      key,
		endpoint: base.String() + zbValidatePath,
		timeout:  timeout,
		httpc:    httpc,
		now:      now,
		inflight: map[string]*zbFlight{},
	}, nil
}

// Name implements Vendor.
func (z *ZeroBounce) Name() string { return ZeroBounceVendorName }

// Timeout is the per-call timeout in force (a dial; exposed so wiring tests can see it consumed).
func (z *ZeroBounce) Timeout() time.Duration { return z.timeout }

// Endpoint is the validate URL without its query. It carries no key.
func (z *ZeroBounce) Endpoint() string { return z.endpoint }

func (z *ZeroBounce) String() string {
	return "ZeroBounce{endpoint:" + z.endpoint + " key:" + redactedKey(z.key) + " timeout:" + z.timeout.String() + "}"
}

// GoString covers %#v.
func (z *ZeroBounce) GoString() string { return z.String() }

// Verify implements Vendor: ONE paid lookup for ONE plaintext address (address-only egress: the only
// parameters are api_key, email, an empty ip_address and the vendor-side timeout). idempotencyKey is
// ZeroBounce-agnostic (the vendor has none); it keys the in-process collapse of concurrent identical
// calls and the RequestID audit trace.
func (z *ZeroBounce) Verify(ctx context.Context, plaintextAddr, idempotencyKey string) (VendorResult, error) {
	if strings.TrimSpace(plaintextAddr) == "" {
		return VendorResult{}, &ZBError{Kind: ZBErrRefused}
	}
	if z.paused() {
		return VendorResult{}, &ZBError{Kind: ZBErrRateLimited}
	}
	if idempotencyKey == "" || z.noDedupe {
		return z.call(ctx, plaintextAddr, idempotencyKey)
	}

	z.mu.Lock()
	if f, ok := z.inflight[idempotencyKey]; ok {
		z.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return VendorResult{}, &ZBError{Kind: ZBErrTransport, cause: ctx.Err()}
		}
		if f.err != nil {
			return VendorResult{}, f.err
		}
		shared := f.res
		shared.CostUnits = 0 // the leader's call is the one that was charged
		return shared, nil
	}
	f := &zbFlight{done: make(chan struct{})}
	z.inflight[idempotencyKey] = f
	z.mu.Unlock()

	f.res, f.err = z.call(ctx, plaintextAddr, idempotencyKey)

	z.mu.Lock()
	delete(z.inflight, idempotencyKey)
	z.mu.Unlock()
	close(f.done)
	return f.res, f.err
}

// call performs the HTTP request and interprets the response. It runs on a context detached from the
// caller's cancellation so a cancelled leader cannot fail the followers that share its request; the
// per-call timeout still bounds it.
func (z *ZeroBounce) call(ctx context.Context, addr, idem string) (VendorResult, error) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), z.timeout)
	defer cancel()

	q := url.Values{}
	q.Set("api_key", z.key)
	q.Set("email", addr)
	q.Set("ip_address", "") // the parameter is required but may be blank; we never send a caller IP
	q.Set("timeout", strconv.Itoa(z.vendorTimeoutSeconds()))
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, z.endpoint+"?"+q.Encode(), nil)
	if err != nil {
		// err quotes the URL, key included. Dropped.
		return VendorResult{}, &ZBError{Kind: ZBErrMalformed}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", zbUserAgent)

	start := time.Now()
	resp, err := z.httpc.Do(req)
	if err != nil {
		// err is a *url.Error quoting the URL, key included. Only the fact of a deadline survives.
		return VendorResult{}, transportError(cctx)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		z.pause(resp.Header.Get("Retry-After"))
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return VendorResult{}, &ZBError{Kind: ZBErrRateLimited, HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return VendorResult{}, &ZBError{Kind: ZBErrHTTPStatus, HTTPStatus: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, zbMaxBody+1))
	if err != nil {
		return VendorResult{}, transportError(cctx)
	}
	if len(body) > zbMaxBody {
		return VendorResult{}, &ZBError{Kind: ZBErrMalformed}
	}
	return interpretZeroBounce(body, idem, int(time.Since(start)/time.Millisecond))
}

func transportError(ctx context.Context) *ZBError {
	e := &ZBError{Kind: ZBErrTransport}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		e.cause = context.DeadlineExceeded
	}
	return e
}

// vendorTimeoutSeconds is the `timeout` query parameter: ZeroBounce's own validation deadline, kept just
// under ours so the vendor answers (unknown/timeout_exceeded) instead of working past our deadline. The
// documented range is 3..60 seconds.
func (z *ZeroBounce) vendorTimeoutSeconds() int {
	s := int(z.timeout/time.Second) - 1
	if s < 3 {
		s = 3
	}
	if s > 60 {
		s = 60
	}
	return s
}

// paused reports whether a 429 pause is in force. During it no request is made.
func (z *ZeroBounce) paused() bool {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.now().Before(z.pauseUntil)
}

// pause starts (or extends) the 429 pause from a Retry-After header (seconds or an HTTP date). Absent or
// unreadable => a default; always clamped.
func (z *ZeroBounce) pause(retryAfter string) {
	d := zbDefaultRatePause
	if v := strings.TrimSpace(retryAfter); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			d = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(v); err == nil {
			d = t.Sub(z.now())
		}
	}
	if d < zbMinRatePause {
		d = zbMinRatePause
	}
	if d > zbMaxRatePause {
		d = zbMaxRatePause
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	if until := z.now().Add(d); until.After(z.pauseUntil) {
		z.pauseUntil = until
	}
}

// zbErrorKeys are the keys that only ever carry a failure (the Go SDK's errorOnlyKeys: an invalid or
// drained key answers validate with {"error": "..."} on HTTP 200).
var zbErrorKeys = []string{"error", "error_message", "Error"}

// zbRawKeep is the whitelist of vendor fields kept in the `raw` sidecar. Everything else — address,
// account, did_you_mean (an address), first/last name, gender, country, region, city, zip — is dropped:
// the raw address is never persisted (Door 3) and enrichment data about the address's owner is not ours
// to keep.
var zbRawKeep = []string{"free_email", "mx_found", "mx_record", "smtp_provider", "domain_age_days", "processed_at"}

// interpretZeroBounce turns a 200 response body into an answer or a non-answer. It is pure (no I/O).
func interpretZeroBounce(body []byte, idem string, latencyMS int) (VendorResult, error) {
	if !json.Valid(body) {
		return VendorResult{}, &ZBError{Kind: ZBErrMalformed}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return VendorResult{}, &ZBError{Kind: ZBErrMalformed}
	}
	for _, k := range zbErrorKeys {
		if _, ok := obj[k]; ok {
			return VendorResult{}, &ZBError{Kind: ZBErrErrorBody, HTTPStatus: http.StatusOK}
		}
	}
	if raw, ok := obj["success"]; ok && zbFalsy(raw) {
		return VendorResult{}, &ZBError{Kind: ZBErrErrorBody, HTTPStatus: http.StatusOK}
	}
	status, ok := zbString(obj["status"])
	if !ok { // also the bare {"Message": "..."} answer: it carries no status, so it is not an answer
		return VendorResult{}, &ZBError{Kind: ZBErrMalformed}
	}
	if _, ok := zbString(obj["address"]); !ok {
		return VendorResult{}, &ZBError{Kind: ZBErrMalformed}
	}
	sub := ""
	if rawSub, present := obj["sub_status"]; present && !zbIsNull(rawSub) {
		s, ok := zbString(rawSub)
		if !ok {
			return VendorResult{}, &ZBError{Kind: ZBErrMalformed}
		}
		sub = s
	}

	result, mapped := MapZeroBounce(status, sub)
	if !mapped {
		if status == zbStatusUnknown {
			return VendorResult{}, &ZBError{Kind: ZBErrNoAnswer}
		}
		return VendorResult{}, &ZBError{Kind: ZBErrUnrecognised}
	}

	statusRaw := status
	if sub != "" {
		statusRaw = status + "/" + sub
	}
	requestID := "zb"
	if idem != "" {
		requestID = "zb-" + idem
	}
	return VendorResult{
		Result:    result,
		StatusRaw: statusRaw,
		RequestID: requestID,
		CostUnits: 1, // UNVERIFIED: one credit per answered validation (no SDK states the price)
		LatencyMS: latencyMS,
		RawJSON:   zbProjection(status, sub, obj),
	}, nil
}

// zbProjection builds the raw sidecar blob from the whitelist. A kept value must be a short JSON scalar.
func zbProjection(status, sub string, obj map[string]json.RawMessage) []byte {
	out := map[string]any{"vendor": ZeroBounceVendorName, "status": status, "sub_status": sub}
	for _, k := range zbRawKeep {
		raw, ok := obj[k]
		if !ok || len(raw) > 256 {
			continue
		}
		t := bytes.TrimSpace(raw)
		if len(t) == 0 || t[0] == '{' || t[0] == '[' {
			continue
		}
		out[k] = json.RawMessage(t)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// zbString decodes a JSON string value; ok=false when the value is absent, null or not a string.
func zbString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || zbIsNull(raw) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func zbIsNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// zbFalsy reports a "success" field that means failure: false, 0, "false", "0" (the Go SDK's list).
func zbFalsy(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return !t
	case float64:
		return t == 0
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "false" || s == "0"
	}
	return false
}

// Compile-time proof the adapter satisfies the swappable interface (§8.1).
var _ Vendor = (*ZeroBounce)(nil)
