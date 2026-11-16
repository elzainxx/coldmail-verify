package orchestrator

// signals.failure_origin (Docs/failure-origin.md), end to end: the WHOLE Verify path — real normalisation, the REAL cheap layers over the
// production resolver on the real DNS protocol (internal/dnsfixture), the real policy, the real SMTP stage with the real transcript
// parser / classifier / scorer over the node's wrapper JSON (the same fakeNode fixture every SMTP-stage test here uses), the real
// assembler, the real LOG. Nothing between the input and the response is mocked; the DNS server and the node's answer are the fixtures.
//
// THE THREE THINGS EVERY ROW ASSERTS, independently:
//  1. the verdict is exactly what it was before the key existed — status, reason, confidence 10, recommendation review (or skip for the DNS
//     gate), NEVER cached, logged once — and the queue class (the retry decision) is the pre-existing one for that reason;
//  2. signals.failure_origin is the origin the owner's rulings and the evidence give;
//  3. a PAIRED CONTROL — the same wiring with the cause removed — is not an unknown and carries no failure_origin.
//
// The coverage check at the end makes the file unable to pass vacuously: every origin and every reason the doc's table lists as an
// unknown must have been observed by a row above. (unknown_error is the drainer's and is covered in internal/queue.)

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/dnsfixture"
	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/layers"
	"github.com/coldmail-verify/control-plane/internal/node"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/router"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// originOrch wires the real orchestrator to a resolver, with the SMTP stage wired to a fakeNode and a fully :smtp-eligible node.
func originOrch(t *testing.T, res layers.Resolver, body []byte, nodeErr error) (*Orchestrator, *store.MemStore, *policy.Definition) {
	t.Helper()
	h, err := hash.NewHasher([]byte("failure-origin-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	mem := store.NewMem(nil)
	mem.SeedNodeHealth(greenNode)
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	nodes := []router.Node{{ID: "n1", BaseURL: "http://mock", TokenRef: "n1", Enabled: true, PublicEgressIP: testEgressIP}}
	o := New(Deps{
		Hasher: h, Store: mem, Resolver: res, CacheTTL: time.Hour, ConfigFingerprint: "cfg_sha256:test",
		SMTP: &SMTPDeps{
			Client: fakeNode{body: body, err: nodeErr}, Registry: router.New(nodes, nil),
			TokenFor: func(string) string { return "tok" }, Health: mem, MaxAuditAge: 0,
		},
	})
	return o, mem, def
}

// originValidBody: the node reports a clean 250 on a non-catch-all domain — the CONTROL for every row.
const originValidBody = `{"wrapper_api_version":"v1","email":"jane@hasmx.test","validation_type":"smtp","success":true,"smtp_debug":null,
  "catchall":{"probed":true,"probe_local_part":"nx-seed","accepted":false,"conclusion":"not_catchall"},
  "mx":{"domain":"hasmx.test","mail_servers":["192.0.2.10"]}}`

// originTranscript is a failed probe: one host, with the given stage flags and per-stage error strings.
func originTranscript(portOpened bool, errs string) []byte {
	po := "false"
	if portOpened {
		po = "true"
	}
	return []byte(`{"wrapper_api_version":"v1","email":"jane@hasmx.test","validation_type":"smtp","success":false,
  "smtp_debug":[{"mail_host":"192.0.2.10","port_opened":` + po + `,"connection":` + po + `,"helo":null,"rcptto":false,"errors":` + errs + `}],
  "mx":{"domain":"hasmx.test","mail_servers":["192.0.2.10"]}}`)
}

// seen records what the table rows observed, for the coverage assertions at the end.
type originSeen struct {
	origins map[vocab.FailureOrigin]bool
	reasons map[vocab.Reason]bool
}

func (s *originSeen) note(reason vocab.Reason, origin vocab.FailureOrigin) {
	s.origins[origin] = true
	s.reasons[reason] = true
}

func TestFailureOrigin_SMTPStage_ThroughTheRealLayers(t *testing.T) {
	srv := dnsfixture.Start(t)
	res := layers.NewNetResolverAt(srv.Addr())
	seen := &originSeen{origins: map[vocab.FailureOrigin]bool{}, reasons: map[vocab.Reason]bool{}}

	type row struct {
		name       string
		body       []byte
		nodeErr    error
		wantReason vocab.Reason
		wantOrigin vocab.FailureOrigin
		wantClass  QueueClass // the PRE-EXISTING retry decision for this reason (classifyRan) — the origin must not move it
	}
	rows := []row{
		// ── OUR node: no transcript exists, the recipient's server was never involved as far as M1 can see ──
		{"node transport error (unreachable / refused / brain-side deadline)", nil, &node.TransportError{Err: context.DeadlineExceeded},
			vocab.ReasonSMTPTimeout, vocab.OriginOwn, QTransientNet},
		{"node transport error: connection refused to the node", nil, &node.TransportError{Err: errors.New("dial tcp 10.0.0.9:3000: connect: connection refused")},
			vocab.ReasonSMTPTimeout, vocab.OriginOwn, QTransientNet},
		{"node HTTP 401 (our credential)", nil, &node.HTTPStatusError{Status: 401}, vocab.ReasonSMTPError, vocab.OriginOwn, QSettled},
		{"node HTTP 400 (our protocol)", nil, &node.HTTPStatusError{Status: 400}, vocab.ReasonSMTPError, vocab.OriginOwn, QSettled},
		{"node HTTP 500", nil, &node.HTTPStatusError{Status: 500}, vocab.ReasonConnectionFailed, vocab.OriginOwn, QTransientNet},
		{"node HTTP 503", nil, &node.HTTPStatusError{Status: 503}, vocab.ReasonConnectionFailed, vocab.OriginOwn, QTransientNet},
		{"our own client failed (not a transport or status error)", nil, errors.New("node: marshal request: boom"), vocab.ReasonSMTPError, vocab.OriginOwn, QSettled},
		{"the node answered 200 with a body that is not the wrapper's JSON", []byte("<html>502 bad gateway</html>"), nil, vocab.ReasonSMTPError, vocab.OriginOwn, QSettled},

		// ── the transcript cannot tell whose side failed: undetermined ──
		{"the TCP port never opened (our egress blocked, or their refusal)", originTranscript(false, `{}`), nil,
			vocab.ReasonConnectionFailed, vocab.OriginUndetermined, QTransientNet},
		{"no transcript at all (the node's own MX lookup found nothing)",
			[]byte(`{"wrapper_api_version":"v1","email":"jane@hasmx.test","validation_type":"smtp","success":false,"smtp_debug":null,"errors":{"mx":"target host(s) not found"},"mx":{"domain":"hasmx.test","mail_servers":[]}}`),
			nil, vocab.ReasonConnectionFailed, vocab.OriginUndetermined, QTransientNet},
		{"the session was open and dropped with no reply and no timeout", originTranscript(true, `{"rcptto":"end of file reached"}`), nil,
			vocab.ReasonSMTPError, vocab.OriginUndetermined, QSettled},

		// ── the recipient's server provably spoke, or stayed silent after the session was proven open: remote ──
		{"silence at the connection stage after the TCP port opened", originTranscript(true, `{"connection":"server response timeout"}`), nil,
			vocab.ReasonSMTPTimeout, vocab.OriginRemote, QTransientNet},
		{"450 greylist reply", originTranscript(true, `{"rcptto":"450 4.2.0 Greylisted, please try again later\n"}`), nil,
			vocab.ReasonGreylisted, vocab.OriginRemote, QGreylisted},
		{"421 service not available", originTranscript(true, `{"rcptto":"421 Service not available\n"}`), nil,
			vocab.ReasonGreylisted, vocab.OriginRemote, QGreylisted},
		{"an ambiguous 550 with a transcript", originTranscript(true, `{"rcptto":"550 rejected\n"}`), nil,
			vocab.ReasonSMTPError, vocab.OriginRemote, QSettled},

		// ── a reply BEFORE the recipient was named answers the connecting client (our IP, our sender), not the mailbox: undetermined ──
		{"a refusal at the greeting that carries a reply code", originTranscript(true, `{"connection":"554 5.3.2 no SMTP service here\n"}`), nil,
			vocab.ReasonConnectionFailed, vocab.OriginUndetermined, QTransientNet},
		{"a 4xx at MAIL FROM", originTranscript(true, `{"mailfrom":"451 4.7.1 Please try again later\n"}`), nil,
			vocab.ReasonGreylisted, vocab.OriginUndetermined, QGreylisted},
		{"an ambiguous 550 at MAIL FROM", originTranscript(true, `{"mailfrom":"550 sender not accepted\n"}`), nil,
			vocab.ReasonSMTPError, vocab.OriginUndetermined, QSettled},

		// ── a policy or reputation block is about OUR IP: always own ──
		{"550 with policy language", originTranscript(true, `{"rcptto":"550 5.7.1 blocked by policy\n"}`), nil,
			vocab.ReasonSMTPBlocked, vocab.OriginOwn, QSettled},
		{"554 with no no-such-user body", originTranscript(true, `{"rcptto":"554 5.7.1 Message refused\n"}`), nil,
			vocab.ReasonSMTPBlocked, vocab.OriginOwn, QSettled},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			o, mem, def := originOrch(t, res, tc.body, tc.nodeErr)
			resp, err := o.Verify(context.Background(), "jane@hasmx.test", def)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			// 1. the verdict is what it was before the key existed.
			if resp.Status != vocab.StatusUnknown || resp.Reason != tc.wantReason {
				t.Fatalf("verdict = %s/%s, want unknown/%s", resp.Status, resp.Reason, tc.wantReason)
			}
			if resp.Confidence != 10 || resp.Recommendation != vocab.RecReview {
				t.Errorf("an SMTP unknown is confidence 10 / review, got %d / %s", resp.Confidence, resp.Recommendation)
			}
			if mem.CacheLen() != 0 {
				t.Errorf("an unknown is never cached, cache len = %d", mem.CacheLen())
			}
			if len(mem.Log) != 1 {
				t.Fatalf("exactly one LOG row, got %d", len(mem.Log))
			}
			// 2. the origin, in the response AND the LOG row.
			got, _ := resp.Signals[vocab.SignalFailureOrigin].(string)
			if vocab.FailureOrigin(got) != tc.wantOrigin {
				t.Errorf("failure_origin = %q, want %q", got, tc.wantOrigin)
			}
			if logged, _ := mem.Log[0].Signals[vocab.SignalFailureOrigin].(string); logged != got {
				t.Errorf("the LOG row says failure_origin=%q, the response says %q — they must not drift", logged, got)
			}
			seen.note(resp.Reason, vocab.FailureOrigin(got))

			// the retry decision (the queue class) is the pre-existing one: the origin is information, never a branch.
			qo, qerr := func() (QueueOutcome, error) {
				oq, _, defq := originOrch(t, res, tc.body, tc.nodeErr)
				return oq.ComputeForQueue(context.Background(), "jane@hasmx.test", defq, "")
			}()
			if qerr != nil {
				t.Fatalf("ComputeForQueue: %v", qerr)
			}
			if qo.Class != tc.wantClass {
				t.Errorf("queue class = %d, want %d: failure_origin must not change retry behaviour", qo.Class, tc.wantClass)
			}
			if qo.Cache != nil {
				t.Error("an unknown must not produce a cache row")
			}

			// 3. PAIRED CONTROL: the same address, the same wiring, the cause removed (a node that answers cleanly).
			oc, memc, defc := originOrch(t, res, []byte(originValidBody), nil)
			ctl, err := oc.Verify(context.Background(), "jane@hasmx.test", defc)
			if err != nil {
				t.Fatalf("control Verify: %v", err)
			}
			if ctl.Status != vocab.StatusValid {
				t.Fatalf("control: want valid/accepted (an answer), got %s/%s", ctl.Status, ctl.Reason)
			}
			if _, has := ctl.Signals[vocab.SignalFailureOrigin]; has {
				t.Errorf("control: an answer must carry no failure_origin, got %v", ctl.Signals[vocab.SignalFailureOrigin])
			}
			if memc.CacheLen() != 1 {
				t.Errorf("control: a settled answer is cached (the unknown above is not), got cache len %d", memc.CacheLen())
			}
		})
	}

	// COVERAGE: this table must have exercised every origin and every SMTP-side unknown reason, or it proves less than it claims.
	for _, o := range []vocab.FailureOrigin{vocab.OriginOwn, vocab.OriginRemote, vocab.OriginUndetermined} {
		if !seen.origins[o] {
			t.Errorf("no SMTP row observed origin %q", o)
		}
	}
	for _, r := range []vocab.Reason{vocab.ReasonSMTPError, vocab.ReasonSMTPTimeout, vocab.ReasonConnectionFailed, vocab.ReasonSMTPBlocked, vocab.ReasonGreylisted} {
		if !seen.reasons[r] {
			t.Errorf("no SMTP row observed reason %q", r)
		}
	}
}

// dns_error through the real layers over the real DNS protocol. Each unknown's cause is the resolver's: ours failing (own) or ours relaying
// the domain's failure (remote). The verdict is the gate's `unknown:dns_error:stop` — confidence 10, recommendation skip (10 < the review
// threshold), never cached, never reaching the SMTP stage — and none of that may move.
func TestFailureOrigin_DNSError_ThroughTheRealLayers(t *testing.T) {
	srv := dnsfixture.Start(t)

	// A resolver address nothing listens on: bound, then closed.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot bind a loopback UDP port: %v", err)
	}
	dead := pc.LocalAddr().String()
	_ = pc.Close()

	for _, tc := range []struct {
		name       string
		resolver   layers.Resolver
		domain     string
		budget     time.Duration
		wantOrigin vocab.FailureOrigin
	}{
		{"the domain's DNS fails behind our healthy resolver (SERVFAIL)", layers.NewNetResolverAt(srv.Addr()), "servfail.test", 4 * time.Second, vocab.OriginRemote},
		{"SERVFAIL on the MX query of a name that has an address", layers.NewNetResolverAt(srv.Addr()), "mxservfail.test", 4 * time.Second, vocab.OriginRemote},
		{"SERVFAIL on the A/AAAA follow-up", layers.NewNetResolverAt(srv.Addr()), "aaaa-servfail.test", 4 * time.Second, vocab.OriginRemote},
		{"our resolver never answers (timeout)", layers.NewNetResolverAt(srv.Addr()), "blackhole.test", 1200 * time.Millisecond, vocab.OriginOwn},
		{"the resolver we configured is unreachable (refused, or the ICMP swallowed and it times out)", layers.NewNetResolverAt(dead), "hasmx.test", 2500 * time.Millisecond, vocab.OriginOwn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, mem, def := originOrch(t, tc.resolver, []byte(originValidBody), nil)
			ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
			defer cancel()
			resp, err := o.Verify(ctx, "jane@"+tc.domain, def)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if resp.Status != vocab.StatusUnknown || resp.Reason != vocab.ReasonDNSError {
				t.Fatalf("verdict = %s/%s, want unknown/dns_error (and never invalid)", resp.Status, resp.Reason)
			}
			if resp.Confidence != 10 || resp.Recommendation != vocab.RecSkip {
				t.Errorf("the dns_error gate is confidence 10 / skip, got %d / %s", resp.Confidence, resp.Recommendation)
			}
			if got, _ := resp.Signals[vocab.SignalFailureOrigin].(string); vocab.FailureOrigin(got) != tc.wantOrigin {
				t.Errorf("failure_origin = %q, want %q", got, tc.wantOrigin)
			}
			if _, has := resp.Signals[vocab.SignalSMTPSkippedReason]; has {
				t.Errorf("a dns_error never reaches the SMTP stage, yet carries smtp_skipped_reason=%v", resp.Signals[vocab.SignalSMTPSkippedReason])
			}
			if mem.CacheLen() != 0 || len(mem.Log) != 1 {
				t.Errorf("an unknown is logged once and never cached: cache=%d log=%d", mem.CacheLen(), len(mem.Log))
			}
			if logged, _ := mem.Log[0].Signals[vocab.SignalFailureOrigin].(string); logged != string(tc.wantOrigin) {
				t.Errorf("the LOG row says failure_origin=%q, want %q", logged, tc.wantOrigin)
			}

			// PAIRED CONTROL: the same input with the cause removed — a healthy resolver and a domain that resolves → an answer.
			oc, _, defc := originOrch(t, layers.NewNetResolverAt(srv.Addr()), []byte(originValidBody), nil)
			ctl, err := oc.Verify(context.Background(), "jane@hasmx.test", defc)
			if err != nil {
				t.Fatalf("control Verify: %v", err)
			}
			if ctl.Status != vocab.StatusValid {
				t.Fatalf("control: want valid/accepted (an answer), got %s/%s", ctl.Status, ctl.Reason)
			}
			if _, has := ctl.Signals[vocab.SignalFailureOrigin]; has {
				t.Errorf("control: an answer carries no failure_origin, got %v", ctl.Signals[vocab.SignalFailureOrigin])
			}
		})
	}

	// THE SAME ADDRESS, TWO CAUSES, TWO ORIGINS: the key is sensitive to who failed, not to the domain.
	oRemote, _, defR := originOrch(t, layers.NewNetResolverAt(srv.Addr()), []byte(originValidBody), nil)
	ctxR, cancelR := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancelR()
	rRemote, err := oRemote.Verify(ctxR, "jane@servfail.test", defR)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	oOwn, _, defO := originOrch(t, layers.NewNetResolverAt(dead), []byte(originValidBody), nil)
	ctxO, cancelO := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancelO()
	rOwn, err := oOwn.Verify(ctxO, "jane@servfail.test", defO)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rRemote.Signals[vocab.SignalFailureOrigin] != "remote" || rOwn.Signals[vocab.SignalFailureOrigin] != "own" {
		t.Fatalf("the same address must read remote behind a SERVFAIL and own behind a dead resolver, got %v / %v",
			rRemote.Signals[vocab.SignalFailureOrigin], rOwn.Signals[vocab.SignalFailureOrigin])
	}
}

// The key is not only for unknowns: a risky/plausible that is a DEGRADED cheap-only fallback caused by us says `own`, and a settled routing
// outcome does not. Each capacity token is driven through the real router / limiter path where the existing tests already do (a red node;
// a warmup cap); the settled-routing control is a Gmail address, which the policy routes :mx and never probes. In every case the verdict
// is exactly what the capacity tests have always asserted — only the key is new.
func TestFailureOrigin_DegradedCheapOnlyAnswersAreOwn_AndSettledRoutingIsNot(t *testing.T) {
	ctx := context.Background()

	// a red node → audit_red
	o, mem, def := smtpSetup(t, plausibleBody, nil, time.Hour)
	mem.SeedNodeHealth(store.NodeHealth{NodeID: "n1", Status: "active", AuditStatus: "red", Port25Open: true, LastAuditAt: time.Now(), LastAuditHostIP: testEgressIP})
	red, err := o.Verify(ctx, "jane@acmecorp.com", def)
	if err != nil {
		t.Fatalf("Verify (red node): %v", err)
	}
	if red.Status != vocab.StatusRisky || red.Reason != vocab.ReasonPlausible || red.Signals[vocab.SignalSMTPSkippedReason] != vocab.SkipAuditRed {
		t.Fatalf("red node: got %s/%s skip=%v, want risky/plausible with audit_red", red.Status, red.Reason, red.Signals[vocab.SignalSMTPSkippedReason])
	}
	if red.Signals[vocab.SignalFailureOrigin] != string(vocab.OriginOwn) {
		t.Errorf("red node: failure_origin = %v, want own", red.Signals[vocab.SignalFailureOrigin])
	}

	// the warmup cap is reached → warmup_cap (the second request is over a cap of 1)
	ow, memw, defw := smtpSetup(t, plausibleBody, nil, time.Hour)
	ow.smtp.WarmupCap = 1
	if _, err := ow.Verify(ctx, "a@acmecorp.com", defw); err != nil {
		t.Fatalf("Verify (first, under the cap): %v", err)
	}
	capped, err := ow.Verify(ctx, "b@acmecorp.com", defw)
	if err != nil {
		t.Fatalf("Verify (over the cap): %v", err)
	}
	if capped.Signals[vocab.SignalSMTPSkippedReason] != vocab.SkipWarmupCap || capped.Signals[vocab.SignalFailureOrigin] != string(vocab.OriginOwn) {
		t.Errorf("warmup cap: skip=%v origin=%v, want warmup_cap/own", capped.Signals[vocab.SignalSMTPSkippedReason], capped.Signals[vocab.SignalFailureOrigin])
	}
	if memw.CacheLen() != 1 {
		t.Errorf("warmup cap: only the answered first request is cached, got cache len %d", memw.CacheLen())
	}

	// CONTROL: provider_mx_routed is a SETTLED routing outcome, not a failure: no failure_origin.
	og, _, defg := setup(t, gmailResolver(), time.Hour)
	routed, err := og.Verify(ctx, "someone@gmail.com", defg)
	if err != nil {
		t.Fatalf("Verify (gmail): %v", err)
	}
	if routed.Reason != vocab.ReasonProviderNoSMTP || routed.Signals[vocab.SignalSMTPSkippedReason] != vocab.SkipProviderMX {
		t.Fatalf("gmail: got %s skip=%v, want provider_no_smtp with provider_mx_routed", routed.Reason, routed.Signals[vocab.SignalSMTPSkippedReason])
	}
	if _, has := routed.Signals[vocab.SignalFailureOrigin]; has {
		t.Errorf("a settled routing outcome carries no failure_origin, got %v", routed.Signals[vocab.SignalFailureOrigin])
	}
}
