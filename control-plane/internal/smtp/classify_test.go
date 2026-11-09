package smtp

import (
	"encoding/json"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// These tests anchor the §6.1 classifier and §6.2 parser on the REAL transcript shapes captured
// in Docs/history/truemail-ground-truth.md (§3.4 live smtpmock v2.5.4, §6.2 live gmail timeout). The
// fixtures are verbatim-shaped so a spec-vs-reality drift fails here, not in production.

// parseWrapper decodes a raw /v1/verify JSON body the way the brain will, then parses the target.
func parseWrapper(t *testing.T, body string) (WrapperResponse, Parsed) {
	t.Helper()
	var wr WrapperResponse
	if err := json.Unmarshal([]byte(body), &wr); err != nil {
		t.Fatalf("decode wrapper body: %v", err)
	}
	return wr, ParseTranscript(wr.SMTPDebug, wr.Success)
}

// ── §6.2 parse target: the REAL failure shape (rcptto:false, code only in errors{}) — GAP-4 ──
func TestParse_RealRcpttoErrorString_ExtractsCode(t *testing.T) {
	// Shape from ground-truth §6.2 / smtp_mock §3.4 seeded reject: rcptto false, code in errors.
	body := `{
	  "success": false,
	  "smtp_debug": [
	    {"mail_host":"1.2.3.4","port_opened":true,"connection":true,"helo":true,
	     "mailfrom":{"status":"250","string":"250 OK\n"},
	     "rcptto":false,
	     "errors":{"rcptto":"550 User not found\n"}}
	  ]
	}`
	_, p := parseWrapper(t, body)
	if p.Code != "550" {
		t.Fatalf("want code 550 parsed from raw string (GAP-4), got %q", p.Code)
	}
	if p.Stage != "rcptto" {
		t.Fatalf("want stage rcptto, got %q", p.Stage)
	}
	if p.CleanSuccess {
		t.Fatal("a 550 failure must not be CleanSuccess")
	}
}

func TestClassify_550NoUser_IsInvalidAddressFault(t *testing.T) {
	_, p := parseWrapper(t, `{"success":false,"smtp_debug":[
	  {"mail_host":"h","port_opened":true,"connection":true,"helo":true,"rcptto":false,
	   "errors":{"rcptto":"550 5.1.1 User unknown\n"}}]}`)
	got := Classify(p)
	if got.Status != vocab.StatusInvalid || got.Reason != vocab.ReasonMailboxNotFound {
		t.Fatalf("550 no-user must be invalid/mailbox_not_found, got %s/%s", got.Status, got.Reason)
	}
}

// The pivotal split: a 550 WITHOUT a user/mailbox body is NOT invalid — it is unknown.
func TestClassify_550Ambiguous_IsUnknownNeverInvalid(t *testing.T) {
	_, p := parseWrapper(t, `{"success":false,"smtp_debug":[
	  {"mail_host":"h","port_opened":true,"connection":true,"helo":true,"rcptto":false,
	   "errors":{"rcptto":"550 rejected\n"}}]}`)
	got := Classify(p)
	if got.Status == vocab.StatusInvalid {
		t.Fatalf("ambiguous 550 must NEVER be invalid (harden-toward-unknown), got %s/%s", got.Status, got.Reason)
	}
	if got.Status != vocab.StatusUnknown || got.Reason != vocab.ReasonSMTPError {
		t.Fatalf("ambiguous 550 → unknown/smtp_error, got %s/%s", got.Status, got.Reason)
	}
}

func TestClassify_554PolicyBlocked_IsUnknownSMTPBlocked(t *testing.T) {
	_, p := parseWrapper(t, `{"success":false,"smtp_debug":[
	  {"mail_host":"h","port_opened":true,"connection":true,"helo":true,"rcptto":false,
	   "errors":{"rcptto":"554 5.7.1 blocked by policy\n"}}]}`)
	got := Classify(p)
	if got.Status != vocab.StatusUnknown || got.Reason != vocab.ReasonSMTPBlocked {
		t.Fatalf("554 policy block → unknown/smtp_blocked, got %s/%s", got.Status, got.Reason)
	}
}

// The REAL smtpmock blacklist delta (ground-truth §3.4): a blocked RCPT returns 421, not 554.
// Both map to unknown (harden-toward-unknown holds); the reason is transient, not smtp_blocked.
func TestClassify_421RealBlockDelta_IsUnknownNeverInvalid(t *testing.T) {
	_, p := parseWrapper(t, `{"success":false,"smtp_debug":[
	  {"mail_host":"h","port_opened":true,"connection":true,"helo":true,"rcptto":false,
	   "errors":{"rcptto":"421 Service not available\n"}}]}`)
	got := Classify(p)
	if got.Status != vocab.StatusUnknown {
		t.Fatalf("real 421 block delta must be unknown, got %s/%s", got.Status, got.Reason)
	}
	if got.Reason == vocab.ReasonSMTPBlocked {
		t.Fatalf("421 is transient (4xx), must NOT classify as smtp_blocked; got %s", got.Reason)
	}
}

func TestClassify_450Greylist_IsUnknownGreylisted(t *testing.T) {
	_, p := parseWrapper(t, `{"success":false,"smtp_debug":[
	  {"mail_host":"h","port_opened":true,"connection":true,"helo":true,"rcptto":false,
	   "errors":{"rcptto":"450 4.2.0 greylisted, try again later\n"}}]}`)
	got := Classify(p)
	if got.Status != vocab.StatusUnknown || got.Reason != vocab.ReasonGreylisted {
		t.Fatalf("450 → unknown/greylisted, got %s/%s", got.Status, got.Reason)
	}
}

func TestClassify_552MailboxFull_IsValidNeverInvalid(t *testing.T) {
	_, p := parseWrapper(t, `{"success":false,"smtp_debug":[
	  {"mail_host":"h","port_opened":true,"connection":true,"helo":true,"rcptto":false,
	   "errors":{"rcptto":"552 mailbox full\n"}}]}`)
	got := Classify(p)
	if got.Status != vocab.StatusValid || got.Reason != vocab.ReasonMailboxFull {
		t.Fatalf("552 = positive existence proof → valid/mailbox_full (owner override), got %s/%s", got.Status, got.Reason)
	}
}

// The REAL gmail timeout shape from ground-truth §6.2: connection error "server response timeout".
func TestClassify_RealTimeout_IsUnknownTimeout(t *testing.T) {
	_, p := parseWrapper(t, `{"success":false,"smtp_debug":[
	  {"mail_host":"gmail-smtp-in.l.google.com","port_opened":true,"connection":false,
	   "helo":null,"mailfrom":null,"rcptto":null,
	   "errors":{"connection":"server response timeout"}}]}`)
	got := Classify(p)
	if got.Status != vocab.StatusUnknown || got.Reason != vocab.ReasonSMTPTimeout {
		t.Fatalf("real timeout → unknown/smtp_timeout, got %s/%s", got.Status, got.Reason)
	}
}

func TestClassify_PortNeverOpened_IsConnectionFailed(t *testing.T) {
	_, p := parseWrapper(t, `{"success":false,"smtp_debug":[
	  {"mail_host":"h","port_opened":false,"connection":false,"helo":null,"errors":{}}]}`)
	got := Classify(p)
	if got.Status != vocab.StatusUnknown || got.Reason != vocab.ReasonConnectionFailed {
		t.Fatalf("port never opened → unknown/connection_failed, got %s/%s", got.Status, got.Reason)
	}
}
