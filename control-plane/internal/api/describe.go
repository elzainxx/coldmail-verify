package api

// describe.go — GET /v1/describe (0019, partial Tier 4, owner ruling OD-6/OD-8): the module's self-description, in the SHAPE FAMILY of modules 2 and 3's
// describe (module, api_version, capabilities, routes with auth/scope/schema refs, schemas, vocabularies{source,values,fields}, error_codes[{code,
// http_status}]), so a platform client that reads those two reads this one with the same code.
//
// WHY IT EXISTS. Module 1 was the only module whose vocabularies the platform had to hold as constants copied from this code, because it could not
// ask. A vocabulary that changes in production between two CI runs would then surface as a contract error on a live row. Now the module says what
// it may say.
//
// EVERYTHING HERE IS DERIVED, never retyped:
//   - the vocabularies come from the constants and sets the code itself uses (internal/vocab: Statuses, Reasons, Recommendations, FailureOrigins,
//     SkipTokens, JobStatuses, SubmitAccepted; this package: the issue slugs);
//   - error_codes[] is built by walking codeStatus, the SAME map writeErr resolves a status from, so it can neither omit an emittable code nor invent one;
//   - routes[] is built by walking Routes(), the SAME table Handler() serves, so a served route cannot be undescribed and a described route cannot be
//     unserved; routeMetas holds only what a table of patterns cannot (an id, the auth kind, a sentence, the schema refs), and a test fails if the
//     two ever differ in either direction;
//   - api_version is the version segment of the served /v1 paths.
//
// NOT PUBLISHED, on purpose: the node lifecycle (provisioning/active/quarantined/recovering) and anything about nodes, vendors, cost or policy
// internals. They never appear in a response, and PROJECT_RULES §3 (two vocabularies) forbids leaking mechanics; describe lists what a caller can
// SEE. There is no `pricing` either: what the platform charges depends on the answer, so module 1 has nothing to quote.
//
// AUTH. Like every other /v1 route that is not the synchronous verify: a PER-CALLER key (the legacy static token is refused, 401). Describe holds no
// secret and reads no database, so it works whenever the process can authenticate a caller.

import (
	"net/http"
	"sort"
	"strings"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// moduleName is what describe reports as `module` (the senders report send_managed / send_byo).
const moduleName = "verify"

// The auth kinds a route declares.
const (
	authPerCallerKey         = "per_caller_key"
	authPerCallerKeyOrLegacy = "per_caller_key_or_legacy_token"
	authNone                 = "none"
)

// routeMeta is what describe says about one route beyond its pattern.
type routeMeta struct {
	ID                string
	Auth              string
	Description       string
	RequestSchemaRef  string // "" when the route takes no body
	ResponseSchemaRef string
}

// routeMetas is keyed by the exact ServeMux pattern of Routes(). TestDescribe_EveryServedRouteIsDescribedAndNothingElse keeps it equal to the
// served set in both directions.
var routeMetas = map[string]routeMeta{
	"POST /v1/verify": {ID: "verify", Auth: authPerCallerKeyOrLegacy,
		Description:      "Verify one address synchronously; the answer is the verdict object.",
		RequestSchemaRef: "#/schemas/verify_request", ResponseSchemaRef: "#/schemas/verdict"},
	"POST /v1/verify/bulk": {ID: "verify.bulk.create", Auth: authPerCallerKey,
		Description:      "Submit a job of addresses (addresses[] or items[] with item_id). Optional Idempotency-Key header: a repeat with the same request returns the original job (header Idempotent-Replayed: true); the same key with another request is a 409 idempotency_key_reused.",
		RequestSchemaRef: "#/schemas/bulk_request", ResponseSchemaRef: "#/schemas/bulk_accepted_response"},
	"GET /v1/jobs/{id}": {ID: "verify.bulk.status", Auth: authPerCallerKey,
		Description:       "A job's status: complete, counts and totals (submitted, settled, pending, duplicates, malformed, by_status), all from one consistent read.",
		ResponseSchemaRef: "#/schemas/job_status_response"},
	"GET /v1/jobs/{id}/results": {ID: "verify.bulk.results", Auth: authPerCallerKey,
		Description:       "One result per input row that has an outcome, in ordinal order, cursor-paged. Stable once the job is complete.",
		ResponseSchemaRef: "#/schemas/job_results_response"},
	"GET /v1/describe": {ID: "describe", Auth: authPerCallerKey,
		Description: "This document."},
	"GET /healthz": {ID: "healthz", Auth: authNone,
		Description:       "Liveness: the process is up.",
		ResponseSchemaRef: "#/schemas/healthz_response"},
	"GET /readyz": {ID: "readyz", Auth: authNone,
		Description:       "Readiness: pepper, database and (when declared) queue. 200 ready, 503 not_ready.",
		ResponseSchemaRef: "#/schemas/readyz_response"},
}

// vocabEntry is one published value set: where it comes from, its values, and the response paths that carry it.
type vocabEntry struct {
	Name   string
	Values []string
	Fields []string
}

// describedVocabularies is the SOURCE of the vocabularies describe publishes. It is a variable so that a test can swap in a source with a value
// removed and prove the describe-vs-code comparison turns red (the paired control); production never reassigns it. Every Values slice is built
// from the code's own constants, never typed here.
var describedVocabularies = func() []vocabEntry {
	strs := func(n int, at func(i int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = at(i)
		}
		return out
	}
	statuses, recs, origins, jobs, reasons := vocab.Statuses(), vocab.Recommendations(), vocab.FailureOrigins(), vocab.JobStatuses(), vocab.Reasons()
	return []vocabEntry{
		{"status", strs(len(statuses), func(i int) string { return string(statuses[i]) }),
			[]string{"verify.status", "job.results[].status", "job.totals.by_status (the keys)"}},
		{"reason", strs(len(reasons), func(i int) string { return string(reasons[i]) }),
			[]string{"verify.reason", "job.results[].reason"}},
		{"recommendation", strs(len(recs), func(i int) string { return string(recs[i]) }),
			[]string{"verify.recommendation", "job.results[].recommendation"}},
		{"failure_origin", strs(len(origins), func(i int) string { return string(origins[i]) }),
			[]string{"verify.signals.failure_origin", "job.results[].signals.failure_origin"}},
		{"smtp_skipped_reason", vocab.SkipTokens(),
			[]string{"verify.signals.smtp_skipped_reason", "job.results[].signals.smtp_skipped_reason"}},
		{"job_status", strs(len(jobs), func(i int) string { return string(jobs[i]) }),
			[]string{"job.status"}},
		{"submit_status", []string{vocab.SubmitAccepted},
			[]string{"bulk.accepted.status"}},
		{"detail_issue", []string{issueRequired, issueMalformed, issueOutOfRange, issueNotFound, issueConflict, issueDuplicate},
			[]string{"error.details[].issue"}},
	}
}

// apiVersionOf is the version segment shared by every versioned (/vN/...) route of the table, or "" when the table has none or its versioned
// routes disagree. The unversioned operational routes (/healthz, /readyz) say nothing about the version.
func apiVersionOf(rs []Route) string {
	version := ""
	for _, rt := range rs {
		seg := strings.SplitN(strings.TrimPrefix(rt.Path(), "/"), "/", 2)[0]
		if !isVersionSegment(seg) {
			continue
		}
		if version == "" {
			version = seg
			continue
		}
		if seg != version {
			return ""
		}
	}
	return version
}

func isVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// describedErrorCodes renders every error code this module can emit with the HTTP status it maps to, sorted by status then code, DERIVED from
// codeStatus (the map writeErr resolves a status from). The fixed message is not published: it is operator-facing English, not a value to branch on.
func describedErrorCodes() []map[string]any {
	out := make([]map[string]any, 0, len(codeStatus))
	for code, status := range codeStatus {
		out = append(out, map[string]any{"code": code, "http_status": status})
	}
	sort.Slice(out, func(i, j int) bool {
		si, sj := out[i]["http_status"].(int), out[j]["http_status"].(int)
		if si != sj {
			return si < sj
		}
		return out[i]["code"].(string) < out[j]["code"].(string)
	})
	return out
}

// describedRoutes renders routes[] from the served table, so it cannot drift from it.
func describedRoutes(rs []Route) []map[string]any {
	out := make([]map[string]any, 0, len(rs))
	for _, rt := range rs {
		m := map[string]any{"method": rt.Method(), "path": rt.Path()}
		if meta, ok := routeMetas[rt.Pattern]; ok {
			m["id"], m["auth"], m["description"] = meta.ID, meta.Auth, meta.Description
			if meta.RequestSchemaRef != "" {
				m["request_schema_ref"] = meta.RequestSchemaRef
			}
			if meta.ResponseSchemaRef != "" {
				m["response_schema_ref"] = meta.ResponseSchemaRef
			}
		}
		// A served route with no routeMetas entry is still listed (method and path are facts); the test that compares the two tables is what fails.
		out = append(out, m)
	}
	return out
}

// describeBody builds the whole document from the served route table.
func describeBody(rs []Route, version string) map[string]any {
	vocabs := map[string]any{}
	for _, v := range describedVocabularies() {
		vocabs[v.Name] = map[string]any{"source": "module_constant", "values": v.Values, "fields": v.Fields}
	}
	return map[string]any{
		"module":      moduleName,
		"api_version": version,
		// Honest capabilities: what this module does and, because modules 2 and 3 answer quote/run/report, what it deliberately does not.
		"capabilities": map[string]any{
			"sync_verify":            true,
			"bulk_verify":            true,
			"idempotent_bulk_create": true,
			"job_totals":             true,
			"quote":                  false,
			"run":                    false,
			"report":                 false,
		},
		"routes":       describedRoutes(rs),
		"schemas":      describedSchemas(),
		"vocabularies": vocabs,
		"error_codes":  describedErrorCodes(),
		// The idempotency contract of POST /v1/verify/bulk, so a client does not read it out of prose.
		"idempotency": map[string]any{
			"route":                     "POST /v1/verify/bulk",
			"header":                    idempotencyKeyHeader,
			"key_pattern":               "^[A-Za-z0-9._:-]{1,128}$",
			"scope":                     "authenticated_caller",
			"replay_response_header":    idempotentReplayedHeader,
			"reused_with_other_request": codeIdempotencyKeyReused,
		},
	}
}

// handleDescribe is GET /v1/describe.
func (s *Server) handleDescribe(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.authenticate(r, true); !ok {
		writeErr(w, codeUnauthenticated, "auth rejected: describe requires a per-caller key")
		return
	}
	rs := s.Routes()
	version := apiVersionOf(rs)
	if version == "" {
		// Unreachable with the shipped table (TestDescribe_APIVersionIsDerivedFromTheServedRoutes pins both the derivation and this refusal on a table
		// that cannot name one version); answered as what it would be, an internal error, never a guessed version.
		writeErr(w, codeInternalError, "describe: the served /vN routes do not name one version")
		return
	}
	writeJSON(w, http.StatusOK, describeBody(rs, version))
}

// ── schemas ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────

func schObj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func schArr(items any) map[string]any { return map[string]any{"type": "array", "items": items} }

// schVocab is a string whose legal values are the vocabulary of that name in describe's `vocabularies` (never copied into the schema).
func schVocab(name string) map[string]any {
	return map[string]any{"type": "string", "vocabulary": name}
}

// describedSchemas is the shape of every request and response body, as JSON-schema-like fragments. Where a value set exists the schema POINTS at the
// vocabulary (`vocabulary: <name>`) instead of repeating it. TestDescribe_SchemasMatchRealResponses compares each schema with a real response of the
// real handler: a key the response has and the schema lacks, or a required key the response lacks, is red.
func describedSchemas() map[string]any {
	str := map[string]any{"type": "string"}
	boolT := map[string]any{"type": "boolean"}
	intT := map[string]any{"type": "integer"}
	anyObj := map[string]any{"type": "object"}
	detail := schObj(map[string]any{"field": str, "issue": schVocab("detail_issue")}, "field", "issue")

	verdict := schObj(map[string]any{
		"api_version": str, "status": schVocab("status"), "reason": schVocab("reason"),
		"confidence": intT, "recommendation": schVocab("recommendation"),
		"email": str, "domain": str, "flags": anyObj, "signals": anyObj, "meta": anyObj, "raw": map[string]any{"type": "null"},
	}, "api_version", "status", "reason", "confidence", "recommendation", "email", "domain", "flags", "signals", "meta", "raw")

	element := schObj(map[string]any{
		"api_version": str, "status": schVocab("status"), "reason": schVocab("reason"),
		"confidence": intT, "recommendation": schVocab("recommendation"),
		"email": str, "domain": str, "flags": anyObj, "signals": anyObj, "meta": anyObj, "raw": map[string]any{"type": "null"},
		"ordinal": intT, "item_id": str, "duplicate_of": intT,
	}, "api_version", "status", "reason", "confidence", "recommendation", "email", "domain", "flags", "signals", "meta", "raw", "ordinal")

	return map[string]any{
		"verify_request": schObj(map[string]any{"email": str, "policy": str}, "email"),
		"verdict":        verdict,
		"bulk_request": schObj(map[string]any{
			"addresses": schArr(str),
			"items":     schArr(schObj(map[string]any{"item_id": str, "address": str}, "item_id", "address")),
			"policy":    str, "caller_ref": str,
		}),
		"bulk_accepted_response": schObj(map[string]any{
			"job_id": str, "status": schVocab("submit_status"),
			"counts":                   schObj(map[string]any{"total": intT, "enqueued": intT, "duplicate": intT, "malformed": intT}, "total", "enqueued", "duplicate", "malformed"),
			"malformed_rows":           schArr(schObj(map[string]any{"ordinal": intT, "item_id": str}, "ordinal")),
			"malformed_rows_truncated": boolT, "processing_paused": boolT,
		}, "job_id", "status", "counts", "malformed_rows", "malformed_rows_truncated", "processing_paused"),
		"job_status_response": schObj(map[string]any{
			"job_id": str, "status": schVocab("job_status"), "complete": boolT, "caller_ref": str,
			"counts": schObj(map[string]any{"total": intT, "settled": intT, "pending": intT}, "total", "settled", "pending"),
			"totals": schObj(map[string]any{
				"submitted": intT, "settled": intT, "pending": intT, "duplicates": intT, "malformed": intT,
				"by_status": schObj(map[string]any{"valid": intT, "invalid": intT, "risky": intT, "unknown": intT}, "valid", "invalid", "risky", "unknown"),
			}, "submitted", "settled", "pending", "duplicates", "malformed", "by_status"),
			"processing_paused": boolT,
		}, "job_id", "status", "complete", "caller_ref", "counts", "totals", "processing_paused"),
		"job_results_response": schObj(map[string]any{
			"job_id": str, "results": schArr(element), "next_cursor": str,
		}, "job_id", "results", "next_cursor"),
		"error_response": schObj(map[string]any{
			errorEnvelopeKey: schObj(map[string]any{"code": str, "message": str, "details": schArr(detail), "request_id": str}, "code", "message", "request_id"),
		}, errorEnvelopeKey),
		"healthz_response": schObj(map[string]any{"status": str, "at": str}, "status", "at"),
		"readyz_response": schObj(map[string]any{
			"status": str, "checks": anyObj, "at": str, "ready": boolT, "config": str, "pepper": str, "supabaseKey": str, "reasons": schArr(str),
		}, "status", "checks", "at", "ready", "config", "pepper", "supabaseKey"),
	}
}
