// Package smtp is the brain-side SMTP surface for Phase 2: the wrapper-response types, the
// smtp_debug transcript parser (§6.2, closes GAP-4), the 5xx address-fault-vs-your-fault
// classifier (§6.1), and the SMTP-signal → verdict scoring map (§7.2), plus DNS reconciliation
// (§6.6). It runs entirely control-plane-side (Door 5: the box returns raw engine FACTS; ALL
// interpretation is here). Nothing in this package opens a socket — the verifier-node does the
// SMTP; this parses what the wrapper serialized.
//
// The parse target is the REAL shape captured in Docs/history/truemail-ground-truth.md §6.2 and served
// by the fork wrapper's /v1/verify (verifier-node .../wrapper/serializer.rb): smtp_debug is
// null on a clean success, else an array of per-host objects; a stage success is {status,string},
// a failure is `false`/nil with the detail in errors{stage} as a RAW STRING (there is no numeric
// code from the engine — GAP-4). The brain extracts the code from that string.
package smtp

import (
	"encoding/json"
	"regexp"
	"strings"
)

// WrapperResponse is the additive superset of the stock rack JSON that POST /v1/verify returns
// (P2 §4.1). Only the fields the brain interprets are typed; unknown keys are tolerated (the raw
// blob is stored whole for the sidecar). The box renders NO verdict — these are engine facts.
type WrapperResponse struct {
	WrapperAPIVersion string           `json:"wrapper_api_version"`
	Email             string           `json:"email"`
	ValidationType    string           `json:"validation_type"`
	Success           bool             `json:"success"`
	Errors            map[string]any   `json:"errors"`
	SMTPDebug         []HostTranscript `json:"smtp_debug"`
	Catchall          *CatchallBlock   `json:"catchall"`
	MX                *MXBlock         `json:"mx"`
	ProbeRefused      *ProbeRefusal    `json:"probe_refused"` // ADR-0021: present only when the node's destination guard refused
	Date              string           `json:"date"`
}

// HostTranscript is one element of smtp_debug[] — a per-MX-host request/response (ground-truth
// §6.2). MailFrom/RCPTTo are `json.RawMessage`-free: a success is an object {status,string}, a
// failure is bare `false`. We decode them permissively via StageResult.
type HostTranscript struct {
	MailHost   string            `json:"mail_host"`
	PortOpened bool              `json:"port_opened"`
	Connection bool              `json:"connection"`
	Helo       any               `json:"helo"`     // bool or null
	MailFrom   StageResult       `json:"mailfrom"` // {status,string} | false | null
	RCPTTo     StageResult       `json:"rcptto"`   // {status,string} | false | null
	Errors     map[string]string `json:"errors"`   // {stage => raw SMTP string} (GAP-4)
}

// StageResult decodes a stage value that is EITHER a {status,string} success object OR a bare
// false/null failure marker. OK is true only when the object form was present.
type StageResult struct {
	OK     bool
	Status string
	String string
}

// UnmarshalJSON accepts {"status":"250","string":"250 OK\n"} | false | true | null.
func (s *StageResult) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "false" || trimmed == "true" || trimmed == "null" || trimmed == "" {
		s.OK = false
		return nil
	}
	// object form
	var obj struct {
		Status string `json:"status"`
		String string `json:"string"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		// Unknown shape → treat as not-OK rather than failing the whole parse (harden-toward-unknown).
		s.OK = false
		return nil
	}
	s.OK = obj.Status != "" || obj.String != ""
	s.Status = obj.Status
	s.String = obj.String
	return nil
}

// CatchallBlock is the wrapper-added catch-all probe outcome (§4.1/§5).
type CatchallBlock struct {
	Probed         bool             `json:"probed"`
	ProbeLocalPart string           `json:"probe_local_part"`
	Accepted       bool             `json:"accepted"`
	Conclusion     string           `json:"conclusion"` // catchall|not_catchall|inconclusive
	SMTPDebug      []HostTranscript `json:"smtp_debug"`
}

// MXBlock is the wrapper-added engine MX view (§4.1, closes GAP-2) — engine-authoritative for
// the SMTP verdict (§6.6).
type MXBlock struct {
	Domain      string   `json:"domain"`
	MailServers []string `json:"mail_servers"`
}

// codeRe extracts the leading 3-digit SMTP code from a raw response string ("550 User..." → "550").
var codeRe = regexp.MustCompile(`^\s*(\d{3})`)

// enhancedRe extracts an RFC 3463 enhanced status code (e.g. "5.1.1") if present in the body.
var enhancedRe = regexp.MustCompile(`\b([245]\.\d{1,3}\.\d{1,3})\b`)

// Parsed is the brain's structured read of one target's transcript (§6.2). It is the input to
// the §6.1 classifier. Everything is derived from the raw transcript — never invented.
type Parsed struct {
	Reached      bool   // did the session reach the RCPT stage on any host?
	PortOpened   bool   // any host opened the port? (false → connection_failed-shaped)
	Code         string // leading 3-digit code from the decisive stage ("" if none)
	EnhancedCode string // "5.1.1" if present
	Body         string // the raw response string the code came from (for §6.1 body match)
	Stage        string // decisive stage: "rcptto" | "mailfrom" | "connection" | ""
	Timeout      bool   // decisive error is a timeout (→ smtp_timeout)
	CleanSuccess bool   // rcptto succeeded with a 2xx and no error (a clean 250)
}

// ParseTranscript walks a target's smtp_debug and extracts the decisive signal. It prefers the
// most-specific stage: a definitive rcptto outcome (success or error) dominates; then mailfrom;
// then connection. A clean rcptto success with no errors is the CleanSuccess case (§6.8). The
// walk is defensive against nil/empty (which means SMTP was never reached — the caller decides
// whether that is connection_failed or a pre-SMTP cheap gate).
func ParseTranscript(hosts []HostTranscript, engineSuccess bool) Parsed {
	var p Parsed
	if len(hosts) == 0 {
		// smtp_debug is null on a CLEAN success (§6.8 discriminator); the caller passes
		// engineSuccess so we can distinguish "clean 250, no transcript" from "never ran".
		p.CleanSuccess = engineSuccess
		p.Reached = engineSuccess
		return p
	}

	for _, h := range hosts {
		if h.PortOpened {
			p.PortOpened = true
		}
		// A definitive RCPT outcome is the strongest signal — take it and stop.
		if h.RCPTTo.OK {
			p.Reached = true
			p.Stage = "rcptto"
			p.Code = leadingCode(h.RCPTTo.Status, h.RCPTTo.String)
			p.Body = firstNonEmpty(h.RCPTTo.String, h.RCPTTo.Status)
			p.EnhancedCode = enhanced(p.Body)
			// Clean success = a 2xx rcptto with no rcptto error recorded.
			if strings.HasPrefix(p.Code, "2") && h.Errors["rcptto"] == "" {
				p.CleanSuccess = true
			}
			return p
		}
		if raw, ok := h.Errors["rcptto"]; ok && raw != "" {
			p.Reached = true
			p.Stage = "rcptto"
			p.Code = codeOf(raw)
			p.Body = raw
			p.EnhancedCode = enhanced(raw)
			p.Timeout = isTimeout(raw)
			return p
		}
	}

	// No RCPT-stage outcome on any host → look for a mailfrom or connection-stage error to
	// localize the failure (§6.2 step 5). Take the first host that has one.
	for _, h := range hosts {
		if raw, ok := h.Errors["mailfrom"]; ok && raw != "" {
			p.Stage = "mailfrom"
			p.Code = codeOf(raw)
			p.Body = raw
			p.Timeout = isTimeout(raw)
			return p
		}
		if raw, ok := h.Errors["connection"]; ok && raw != "" {
			p.Stage = "connection"
			p.Code = codeOf(raw) // usually empty (timeouts have no code)
			p.Body = raw
			p.Timeout = isTimeout(raw)
			return p
		}
	}

	// Reached no decisive stage. If no port ever opened, that's a connection failure.
	return p
}

func codeOf(raw string) string {
	if m := codeRe.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	return ""
}

func leadingCode(status, str string) string {
	if c := codeOf(status); c != "" {
		return c
	}
	return codeOf(str)
}

func enhanced(body string) string {
	if m := enhancedRe.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

func isTimeout(raw string) bool {
	l := strings.ToLower(raw)
	return strings.Contains(l, "timeout") || strings.Contains(l, "timed out")
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ProbeRefusal is the node's destination-guard FACT (ADR-0021): the wrapper refused to open an SMTP session because the domain's
// mail host resolves to a non-public address. There is no transcript; the brain turns this into risky/non_public_mx.
type ProbeRefusal struct {
	Reason  string          `json:"reason"` // "non_public_target"
	Count   int             `json:"count"`
	Targets []RefusedTarget `json:"targets"`
}

// RefusedTarget is one refused address and the special-purpose range it fell in.
type RefusedTarget struct {
	Address string `json:"address"`
	Range   string `json:"range"`
}
