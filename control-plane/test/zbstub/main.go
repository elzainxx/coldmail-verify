// Command zbstub is a STAND-IN for ZeroBounce's validate API, for tests and CI ONLY. There is no real
// vendor call in CI and no real key anywhere in this repository: the control plane's ZeroBounce adapter
// (internal/external/zerobounce.go) is pointed at this server through TM_TEST_ZEROBOUNCE_URL.
//
// It speaks the one endpoint the adapter uses, GET /v2/validate?api_key=&email=&ip_address=[&timeout=],
// and answers from the LOCAL PART of the requested address, so a test drives every row of the mapping
// table (Docs/vendor-zerobounce.md) and every failure mode with an address alone:
//
//	<status>[--<sub_status>][+dly<ms>][+<tag>]@<any domain>
//
//	valid@x                      200 {"status":"valid","sub_status":""}
//	do_not_mail--toxic@x         200 {"status":"do_not_mail","sub_status":"toxic"}
//	catch-all@x                  200 {"status":"catch-all","sub_status":""}
//	fail--<mode>@x               a failure mode (below)
//	valid+dly400+<tag>@x         the answer is held back 400 ms (so a test can overlap two calls)
//	valid+<tag>@x                <tag> groups the request for the counters below (any other + segment is noise)
//
// Any <status>--<sub_status> is served as written; the stand-in does not validate pairs. That is the point:
// it serves every pair the live status-codes page lists (valid--alias_address, valid--alternate,
// valid--accept_all, do_not_mail--mx_forward and the rest of the mapping table), the four SDK-only pairs, all
// eight sub-statuses of `unknown`, the pairs the adapter must NOT read as answers (invalid--accept_all,
// valid--mx_forward, valid--blocked, ...), and a status this build has never seen. What counts as an answer is
// decided by the adapter's table (internal/external/zerobounce_map.go), never here: this server is a test
// double, not a model of the vendor.
//
// Failure modes (fail--<mode>): error_body_key, error_body_credits (HTTP 200 {"error":...}),
// error_body_echo_key (an error body that quotes the api_key it was sent — the hazard the adapter must
// not repeat), error_body_capital ({"Error":...}), success_false, message_only (the bare greylist
// {"Message":...}), http_429, http_429_retry_after (Retry-After: 120), http_500, http_503,
// http_403_html (an edge "Access denied" page, what the real docs host returned to our tooling),
// http_302 (a redirect to a second request the adapter must NOT follow), slow (answers after 10 s unless
// the client gives up first), malformed, truncated (Content-Length larger than the body), html_200,
// empty_200, empty_object, array_body, status_not_string, no_address, unknown_status.
//
// Counters, so a test can assert "disabled sends NOTHING" and "the N+1th call was refused":
//
//	GET /__calls                total + per-tag counts
//	GET /__calls?tag=T          {"tag":"T","count":n}
//	GET /__calls?email=E        {"email":"E","count":n}
//	GET /__requests?tag=T       the recorded request shapes for T (parameter NAMES, never the key)
//	GET /healthz                readiness
//
// Every request to /v2/validate is counted on ARRIVAL, whatever it then gets back. The server never logs a
// request line (the key rides in the query string) and never stores the key, only its length.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxRecords = 20000

type reqRecord struct {
	Tag        string   `json:"tag"`
	Email      string   `json:"email"`
	ParamNames []string `json:"param_names"`
	IPAddress  string   `json:"ip_address"`
	Timeout    string   `json:"timeout"`
	KeyLen     int      `json:"key_len"`
	Method     string   `json:"method"`
}

type server struct {
	mu      sync.Mutex
	total   int
	byTag   map[string]int
	byEmail map[string]int
	records []reqRecord
}

func newServer() *server {
	return &server{byTag: map[string]int{}, byEmail: map[string]int{}}
}

func main() {
	addr := os.Getenv("ZBSTUB_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: newServer(), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("zbstub: stand-in ZeroBounce validate API listening on %s (tests only; no real vendor)", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("zbstub: %v", err)
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	case "/v2/validate":
		s.validate(w, r)
	case "/__calls":
		s.calls(w, r)
	case "/__requests":
		s.requests(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// parsed is the behaviour a local part encodes.
type parsed struct {
	behaviour string
	tag       string
	delay     time.Duration
}

func parseLocal(local string) parsed {
	parts := strings.Split(strings.ToLower(local), "+")
	p := parsed{behaviour: parts[0]}
	for _, seg := range parts[1:] {
		if strings.HasPrefix(seg, "dly") {
			if ms, err := strconv.Atoi(strings.TrimPrefix(seg, "dly")); err == nil && ms >= 0 && ms <= 30000 {
				p.delay = time.Duration(ms) * time.Millisecond
				continue
			}
		}
		if p.tag == "" {
			p.tag = seg
		}
	}
	return p
}

func splitEmail(email string) (local, domain string) {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return email, ""
	}
	return email[:at], email[at+1:]
}

func (s *server) validate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	email := q.Get("email")
	local, domain := splitEmail(email)
	p := parseLocal(local)

	names := make([]string, 0, len(q))
	for k := range q {
		names = append(names, k)
	}
	sort.Strings(names)
	s.record(reqRecord{
		Tag: p.tag, Email: email, ParamNames: names, IPAddress: q.Get("ip_address"),
		Timeout: q.Get("timeout"), KeyLen: len(q.Get("api_key")), Method: r.Method,
	})

	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-r.Context().Done():
			return
		}
	}

	key := q.Get("api_key")
	if key == "" {
		// The real API answers an invalid or missing key with HTTP 200 and an error body (Go SDK, http_error.go).
		writeJSON(w, http.StatusOK, map[string]any{"error": "Invalid API key"})
		return
	}

	if strings.HasPrefix(p.behaviour, "fail--") {
		s.fail(w, r, strings.TrimPrefix(p.behaviour, "fail--"), email, key, p)
		return
	}

	status, sub := p.behaviour, ""
	if i := strings.Index(p.behaviour, "--"); i >= 0 {
		status, sub = p.behaviour[:i], p.behaviour[i+2:]
	}
	writeJSON(w, http.StatusOK, answerBody(email, local, domain, status, sub, p.tag))
}

// answerBody is a full ZeroBounce-shaped answer, enrichment fields and all, so a test can prove the adapter
// keeps none of the personal data (address, name, location, did_you_mean) in the raw sidecar.
func answerBody(email, local, domain, status, sub, tag string) map[string]any {
	return map[string]any{
		"address":         email,
		"status":          status,
		"sub_status":      sub,
		"free_email":      false,
		"did_you_mean":    "didyoumean+" + tag + "@example.com",
		"account":         local,
		"domain":          domain,
		"domain_age_days": "9692",
		"smtp_provider":   "example",
		"mx_found":        "true",
		"mx_record":       "mx.example.com",
		"firstname":       "zero",
		"lastname":        "bounce",
		"gender":          "male",
		"country":         "United States",
		"region":          "Florida",
		"city":            "West Palm Beach",
		"zipcode":         "33401",
		"processed_at":    "2023-04-28 15:18:45.631",
	}
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, mode, email, key string, p parsed) {
	local, domain := splitEmail(email)
	switch mode {
	case "error_body_key":
		writeJSON(w, http.StatusOK, map[string]any{"error": "Invalid API key"})
	case "error_body_credits":
		writeJSON(w, http.StatusOK, map[string]any{"error": "Insufficient credits"})
	case "error_body_echo_key":
		writeJSON(w, http.StatusOK, map[string]any{"error": "Invalid API key " + key + " for " + email})
	case "error_body_capital":
		writeJSON(w, http.StatusOK, map[string]any{"Error": "Invalid API key"})
	case "success_false":
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": "Request failed"})
	case "message_only":
		writeJSON(w, http.StatusOK, map[string]any{"Message": "Greylist validation still in progress, please try again later"})
	case "http_429":
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many requests"})
	case "http_429_retry_after":
		w.Header().Set("Retry-After", "120")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many requests"})
	case "http_500":
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
	case "http_503":
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "unavailable"})
	case "http_403_html":
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<!DOCTYPE html><html><head><title>Access denied</title></head><body>Access denied</body></html>"))
	case "http_302":
		// A second, distinct request under the same tag: if the adapter follows the redirect it is counted.
		next := "/v2/validate?api_key=" + key + "&email=valid%2B" + p.tag + "%2Bfollowed%40" + domain + "&ip_address="
		w.Header().Set("Location", next)
		w.WriteHeader(http.StatusFound)
	case "slow":
		select {
		case <-time.After(10 * time.Second):
			writeJSON(w, http.StatusOK, answerBody(email, local, domain, "valid", "", p.tag))
		case <-r.Context().Done():
		}
	case "malformed":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"address": "x", "status": "valid", "sub_status": `))
		_, _ = w.Write([]byte(`this is not json <html>`))
	case "truncated":
		full := `{"address":"` + email + `","status":"valid","sub_status":"","free_email":false,"did_you_mean":""}`
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(full)+200)) // promise more than we send
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(full[:len(full)/2]))
	case "html_200":
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>maintenance</body></html>"))
	case "empty_200":
		w.WriteHeader(http.StatusOK)
	case "empty_object":
		writeJSON(w, http.StatusOK, map[string]any{})
	case "array_body":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	case "status_not_string":
		writeJSON(w, http.StatusOK, map[string]any{"address": email, "status": 7, "sub_status": ""})
	case "no_address":
		writeJSON(w, http.StatusOK, map[string]any{"status": "valid", "sub_status": ""})
	case "unknown_status":
		writeJSON(w, http.StatusOK, answerBody(email, local, domain, "totally_new_status", "", p.tag))
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unknown failure mode %q", mode)})
	}
}

func (s *server) record(rec reqRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	s.byTag[rec.Tag]++
	s.byEmail[rec.Email]++
	if len(s.records) < maxRecords {
		s.records = append(s.records, rec)
	}
}

func (s *server) calls(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case q.Has("tag"):
		writeJSON(w, http.StatusOK, map[string]any{"tag": q.Get("tag"), "count": s.byTag[strings.ToLower(q.Get("tag"))]})
	case q.Has("email"):
		writeJSON(w, http.StatusOK, map[string]any{"email": q.Get("email"), "count": s.byEmail[q.Get("email")]})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"total": s.total, "by_tag": s.byTag})
	}
}

func (s *server) requests(w http.ResponseWriter, r *http.Request) {
	tag := strings.ToLower(r.URL.Query().Get("tag"))
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]reqRecord, 0)
	for _, rec := range s.records {
		if rec.Tag == tag {
			out = append(out, rec)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "stub marshal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}
