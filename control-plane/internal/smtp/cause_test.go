package smtp

import (
	"testing"

	"github.com/coldmail-verify/control-plane/internal/failureorigin"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// failure_origin for the SMTP unknowns, at the lowest real level: the REAL transcript parser, the REAL classifier, the REAL scorer.
// Each row is a transcript shape the node can report (the shapes are the ones in classify_test.go and ground-truth §6.2); the row asserts
// the verdict is UNCHANGED (status/reason/confidence — the origin never moves them) and that the origin is the one the evidence proves.
//
// The direction of error is the point: `remote` only where the recipient's server demonstrably spoke (a numeric SMTP reply) or demonstrably
// stayed silent after the TCP session was proven open; `undetermined` for every transcript that cannot tell our egress from their refusal.

func scoreBody(t *testing.T, body string) Result {
	t.Helper()
	wr, p := parseWrapper(t, body)
	return Score(Input{Target: p, EngineSuccess: wr.Success, Catchall: wr.Catchall})
}

func TestScore_FailureOrigin_PerTranscriptShape(t *testing.T) {
	hostWith := func(portOpened bool, errs string) string {
		po := "false"
		if portOpened {
			po = "true"
		}
		return `{"success":false,"smtp_debug":[{"mail_host":"h","port_opened":` + po + `,"connection":` + po + `,"helo":null,"rcptto":false,"errors":` + errs + `}]}`
	}
	for _, tc := range []struct {
		name       string
		body       string
		wantReason vocab.Reason
		wantCause  failureorigin.Cause
		wantOrigin vocab.FailureOrigin
	}{
		// ── undetermined: nothing proves whose side the silence was on ──
		{"the port never opened: our egress (port 25 blocked) and their refusal look identical",
			hostWith(false, `{}`), vocab.ReasonConnectionFailed, failureorigin.CauseSMTPNoSession, vocab.OriginUndetermined},
		{"no transcript at all (the node's own MX lookup found nothing): no evidence the session ever started",
			`{"success":false,"smtp_debug":null,"errors":{"mx":"target host(s) not found"}}`, vocab.ReasonConnectionFailed, failureorigin.CauseSMTPNoSession, vocab.OriginUndetermined},
		{"the session was open and the connection was dropped with no reply and no timeout: the raw string cannot be attributed",
			hostWith(true, `{"rcptto":"end of file reached"}`), vocab.ReasonSMTPError, failureorigin.CauseSMTPSessionLost, vocab.OriginUndetermined},
		{"a reset at the connection stage after the port opened, no reply code: not attributable",
			hostWith(true, `{"connection":"Connection reset by peer"}`), vocab.ReasonConnectionFailed, failureorigin.CauseSMTPSessionLost, vocab.OriginUndetermined},

		// ── remote: the recipient's side provably spoke, or provably stayed silent in an open session ──
		{"a timeout at the connection stage AFTER the TCP port opened (ground-truth §6.2): the server accepted and did not answer",
			hostWith(true, `{"connection":"server response timeout"}`), vocab.ReasonSMTPTimeout, failureorigin.CauseSMTPSilentAfterConnect, vocab.OriginRemote},
		{"a read timeout at RCPT in an open session",
			hostWith(true, `{"rcptto":"Net::ReadTimeout"}`), vocab.ReasonSMTPTimeout, failureorigin.CauseSMTPSilentAfterConnect, vocab.OriginRemote},
		{"450 greylisting reply",
			hostWith(true, `{"rcptto":"450 4.2.0 Greylisted, please try again later\n"}`), vocab.ReasonGreylisted, failureorigin.CauseSMTPReply, vocab.OriginRemote},
		{"421 service not available (the real smtpmock delta, ground-truth §3.4)",
			hostWith(true, `{"rcptto":"421 Service not available\n"}`), vocab.ReasonGreylisted, failureorigin.CauseSMTPReply, vocab.OriginRemote},
		{"an ambiguous 550 with a transcript (owner: remote only if a transcript exists)",
			hostWith(true, `{"rcptto":"550 rejected\n"}`), vocab.ReasonSMTPError, failureorigin.CauseSMTPReply, vocab.OriginRemote},
		{"an unrecognised 5xx reply",
			hostWith(true, `{"rcptto":"503 5.5.1 Bad sequence of commands\n"}`), vocab.ReasonSMTPError, failureorigin.CauseSMTPReply, vocab.OriginRemote},

		// ── a reply BEFORE the recipient was named answers the connecting client (our IP, our sender), not the mailbox: undetermined ──
		{"a refusal at the greeting with a reply code, at the connection stage",
			hostWith(true, `{"connection":"554 5.3.2 no SMTP service here\n"}`), vocab.ReasonConnectionFailed, failureorigin.CauseSMTPPreRecipientReply, vocab.OriginUndetermined},
		{"a 4xx at MAIL FROM (a deferral aimed at the client)",
			hostWith(true, `{"mailfrom":"451 4.7.1 Please try again later\n"}`), vocab.ReasonGreylisted, failureorigin.CauseSMTPPreRecipientReply, vocab.OriginUndetermined},
		{"an ambiguous 550 at MAIL FROM (about our sender)",
			hostWith(true, `{"mailfrom":"550 sender not accepted\n"}`), vocab.ReasonSMTPError, failureorigin.CauseSMTPPreRecipientReply, vocab.OriginUndetermined},

		// ── own: a policy or reputation block is a statement about OUR IP (the reason-level rule), whatever the code ──
		{"550 with policy language",
			hostWith(true, `{"rcptto":"550 5.7.1 blocked by policy\n"}`), vocab.ReasonSMTPBlocked, failureorigin.CauseSMTPReply, vocab.OriginOwn},
		{"554 with no no-such-user body",
			hostWith(true, `{"rcptto":"554 5.7.1 Message refused\n"}`), vocab.ReasonSMTPBlocked, failureorigin.CauseSMTPReply, vocab.OriginOwn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := scoreBody(t, tc.body)
			if res.Status != vocab.StatusUnknown || res.Reason != tc.wantReason {
				t.Fatalf("verdict = %s/%s, want unknown/%s (the cause must not move the verdict)", res.Status, res.Reason, tc.wantReason)
			}
			if res.Confidence != confUnknown || res.Cacheable {
				t.Errorf("an unknown keeps confidence %d and stays non-cacheable, got %d / cacheable=%v", confUnknown, res.Confidence, res.Cacheable)
			}
			if res.Cause != tc.wantCause {
				t.Errorf("cause = %q, want %q", res.Cause, tc.wantCause)
			}
			if got := failureorigin.OriginFor(res.Reason, res.Cause); got != tc.wantOrigin {
				t.Errorf("origin = %s, want %s", got, tc.wantOrigin)
			}
		})
	}
}

// PAIRED CONTROLS: the same node, the same address, the cause removed. A clean 250 on a non-catch-all domain and a 550 no-such-user are
// ANSWERS: no unknown, no cause, and so no failure_origin to assign.
func TestScore_FailureOrigin_ControlsAreNotUnknown(t *testing.T) {
	accepted := scoreBody(t, `{"success":true,"smtp_debug":null,"catchall":{"probed":true,"accepted":false,"conclusion":"not_catchall"}}`)
	if accepted.Status != vocab.StatusValid || accepted.Cause != "" {
		t.Errorf("control (clean 250): got %s/%s cause %q, want valid with no cause", accepted.Status, accepted.Reason, accepted.Cause)
	}
	noUser := scoreBody(t, `{"success":false,"smtp_debug":[{"mail_host":"h","port_opened":true,"connection":true,"helo":true,"rcptto":false,"errors":{"rcptto":"550 5.1.1 User unknown\n"}}]}`)
	if noUser.Status != vocab.StatusInvalid || noUser.Cause != "" {
		t.Errorf("control (550 no-such-user): got %s/%s cause %q, want invalid with no cause", noUser.Status, noUser.Reason, noUser.Cause)
	}
}

// CauseOf agrees with Classify on every parsed shape it can see: whenever Classify says unknown, CauseOf returns a cause the table knows
// for that reason (so no unknown the scorer can produce falls through to an unmapped pair by accident).
func TestCauseOf_EveryClassifiedUnknownHasATableRow(t *testing.T) {
	rows := map[[2]string]bool{}
	for _, r := range failureorigin.Rows() {
		rows[[2]string{string(r.Reason), string(r.Cause)}] = true
	}
	shapes := []Parsed{
		{}, // nothing
		{PortOpened: true},
		{PortOpened: true, Stage: "connection"},
		{PortOpened: true, Stage: "connection", Code: "554"},
		{PortOpened: true, Stage: "connection", Timeout: true},
		{PortOpened: true, Reached: true, Stage: "rcptto", Timeout: true},
		{PortOpened: true, Reached: true, Stage: "rcptto", Code: "450"},
		{PortOpened: true, Reached: true, Stage: "rcptto", Code: "550", Body: "rejected"},
		{PortOpened: true, Reached: true, Stage: "rcptto", Code: "550", Body: "blocked by policy"},
		{PortOpened: true, Reached: true, Stage: "rcptto", Code: "554", Body: "refused"},
		{PortOpened: true, Reached: true, Stage: "rcptto", Code: "503"},
		{PortOpened: true, Reached: true, Stage: "rcptto"},
		{Stage: "mailfrom", Timeout: true},
		{Stage: "mailfrom"},
		{PortOpened: true, Stage: "mailfrom", Code: "421"},
		{PortOpened: true, Stage: "mailfrom", Code: "550", Body: "sender not accepted"},
		{PortOpened: true, Stage: "mailfrom", Code: "550", Body: "sender rejected by local policy"},
	}
	unknowns := 0
	for _, p := range shapes {
		cls := Classify(p)
		if cls.Status != vocab.StatusUnknown {
			continue
		}
		unknowns++
		cause := CauseOf(p)
		if cause == "" {
			t.Errorf("%+v: unknown/%s but CauseOf returned no cause", p, cls.Reason)
		}
		if cls.Reason != vocab.ReasonSMTPBlocked && !rows[[2]string{string(cls.Reason), string(cause)}] {
			t.Errorf("%+v: unknown/%s with cause %q has no row in the mapping table (it would silently default to undetermined)", p, cls.Reason, cause)
		}
	}
	if unknowns < 10 {
		t.Fatalf("only %d of the shapes classified as unknown: the sweep is not exercising the unknown branches", unknowns)
	}
}
