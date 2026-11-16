package queue

// M1-G7: a policy-resolve, decrypt or compute error used to requeue FOREVER — no result, a job that never completes, no terminal state.
// Each such error is now a counted attempt of the item (the existing attempt column), and the failure that is the ErrorMaxAttempts-th
// SETTLES the item through the same compare-and-set settle path as every other terminal: unknown / unknown_error, failure_origin=own,
// error_capped=true, never cached.
//
// THE TABLE drives a REAL Drainer against the real MemStore (the same CAS, the same settle, the same requeue every other drainer test
// uses), injecting the failure through the existing seams: the Policy resolver, the Decryptor and the Computer. For every stage and every
// cap it asserts, tick by tick:
//   - ONE BELOW THE CAP (the control): the item is still retried — waiting_retry, attempt = the number of failures so far, no LOG row, and
//     the faulty component was called exactly that many times;
//   - AT THE CAP: terminal "dead", exactly ONE LOG row of the right shape, the faulty component called EXACTLY cap times, the stored attempt
//     is cap-1 (the settle does not bump — the same convention as the retry-exhausted greylist), the ciphertext purged, nothing cached;
//   - AFTER the cap: the item is never attempted again.
// MaxAttempts (the greylist ceiling) is deliberately set BELOW the cap: errors are governed by their own cap, not by the retry ceiling.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

const capItemID = "018f0000-0000-7000-8000-000000000c01"

// capComputer fails every call (until okFrom, if set) and counts its calls. When it succeeds it returns a settled valid/accepted outcome.
type capComputer struct {
	calls  int
	okFrom int // calls numbered >= okFrom succeed (0 => never succeed)
}

func (c *capComputer) ComputeForQueue(_ context.Context, _ string, _ *policy.Definition, _ string) (orchestrator.QueueOutcome, error) {
	c.calls++
	if c.okFrom > 0 && c.calls >= c.okFrom {
		row := store.VerificationRow{
			ID: "018f0000-0000-7000-8000-000000000ca1", RequestID: "018f0000-0000-7000-8000-000000000fff",
			CheckedAt: time.Unix(1, 0), CreatedAt: time.Unix(1, 0),
			Status: "valid", Reason: "accepted", Confidence: 90, Recommendation: "send",
			PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg",
		}
		return orchestrator.QueueOutcome{Class: orchestrator.QSettled, LastReason: "accepted", Row: row}, nil
	}
	return orchestrator.QueueOutcome{}, errors.New("compute: injected failure")
}

func (c *capComputer) EscalateExhausted(_ context.Context, _ string, _ *policy.Definition, out orchestrator.QueueOutcome, _, _ string) orchestrator.QueueOutcome {
	return out
}

// capPolicy fails every Resolve and counts them.
type capPolicy struct{ calls int }

func (p *capPolicy) Resolve(context.Context, string, int) (*policy.Definition, error) {
	p.calls++
	return nil, errors.New("policy: injected failure")
}

// capCipher fails every Open and counts them.
type capCipher struct{ calls int }

func (c *capCipher) Open([]byte, []byte) (string, error) {
	c.calls++
	return "", errors.New("cipher: injected failure")
}

type capRig struct {
	mem   *store.MemStore
	dr    *Drainer
	clock *time.Time
	comp  *capComputer
	// failing returns how many times the faulty component has been called so far.
	failing func() int
}

// newCapRig builds a drainer whose stage `stage` ("compute" | "decrypt" | "policy") fails on every call, with the given error cap.
func newCapRig(t *testing.T, stage string, errorCap int) *capRig {
	t.Helper()
	clock := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	mem := store.NewMem(func() time.Time { return clock })
	rig := &capRig{mem: mem, clock: &clock, comp: &capComputer{}}
	deps := Deps{
		Store: mem, Compute: rig.comp, Policy: fakePolicy{}, Cipher: plainCipher{},
		Backoff: testBackoff(), Now: func() time.Time { return clock },
		WorkerID: "w1", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		// The greylist ceiling is BELOW every cap tested: an error is governed by its own cap, never by this.
		MaxAttempts: 3, MaxRetryAge: 24 * time.Hour, BatchSize: 5, ReclaimEvery: time.Hour,
		ErrorMaxAttempts: errorCap,
	}
	switch stage {
	case "compute":
		rig.failing = func() int { return rig.comp.calls }
	case "decrypt":
		c := &capCipher{}
		deps.Cipher = c
		rig.failing = func() int { return c.calls }
	case "policy":
		p := &capPolicy{}
		deps.Policy = p
		rig.failing = func() int { return p.calls }
	default:
		t.Fatalf("unknown stage %q", stage)
	}
	dr, err := NewDrainer(deps)
	if err != nil {
		t.Fatalf("NewDrainer: %v", err)
	}
	rig.dr = dr
	enqueue(t, mem, capItemID, "cap1", "ex.com", "jane@ex.com", "", clock)
	return rig
}

func (r *capRig) item(t *testing.T) store.WorkItemRow {
	t.Helper()
	w, known, err := r.mem.GetWorkItem(context.Background(), capItemID)
	if err != nil || !known {
		t.Fatalf("GetWorkItem: known=%v err=%v", known, err)
	}
	return w
}

// tick runs one drain cycle and moves the clock past any short error-retry wait so the item is due again.
func (r *capRig) tick() {
	r.dr.TickOnce(context.Background())
	*r.clock = r.clock.Add(time.Minute)
}

func TestDrainer_ErrorCap_SettlesExactlyAtTheCap(t *testing.T) {
	for _, stage := range []string{"compute", "decrypt", "policy"} {
		for _, errorCap := range []int{1, 3, 5} {
			t.Run(fmt.Sprintf("%s/cap=%d", stage, errorCap), func(t *testing.T) {
				r := newCapRig(t, stage, errorCap)

				// ONE BELOW THE CAP: still retried, still counted, still no verdict.
				for n := 1; n < errorCap; n++ {
					r.tick()
					w := r.item(t)
					if w.Status != "waiting_retry" {
						t.Fatalf("after %d failure(s) (cap %d) the item must still be retried, got status=%s", n, errorCap, w.Status)
					}
					if w.Attempt != n {
						t.Fatalf("after %d failure(s) the attempt count must be %d, got %d", n, n, w.Attempt)
					}
					if len(r.mem.Log) != 0 {
						t.Fatalf("a retried failure must not log a verdict, got %d LOG rows", len(r.mem.Log))
					}
					if got := r.failing(); got != n {
						t.Fatalf("the faulty %s was called %d times after %d ticks", stage, got, n)
					}
				}

				// THE CAP-th FAILURE: the item settles.
				r.tick()
				w := r.item(t)
				if w.Status != "dead" {
					t.Fatalf("the %d-th failure must SETTLE the item (dead), got status=%s attempt=%d", errorCap, w.Status, w.Attempt)
				}
				if got := r.failing(); got != errorCap {
					t.Fatalf("the faulty %s must have been called EXACTLY %d times, got %d", stage, errorCap, got)
				}
				if w.Attempt != errorCap-1 {
					t.Errorf("the stored attempt is cap-1 (the settle does not bump), want %d got %d", errorCap-1, w.Attempt)
				}
				if stage != "compute" && r.comp.calls != 0 {
					t.Errorf("the %s failure must stop the item before compute, but compute ran %d times", stage, r.comp.calls)
				}
				if len(w.AddrCiphertext) != 0 {
					t.Error("the plaintext (ciphertext) must be purged on settle")
				}
				if r.mem.CacheLen() != 0 {
					t.Error("an error-capped unknown must never be cached")
				}
				if len(r.mem.Log) != 1 {
					t.Fatalf("the settle must log EXACTLY one verdict, got %d", len(r.mem.Log))
				}
				row := r.mem.Log[0]
				if w.VerificationID != row.ID {
					t.Errorf("work_items.verification_id (%q) must point at the logged verdict (%q)", w.VerificationID, row.ID)
				}
				if row.Status != "unknown" || row.Reason != "unknown_error" {
					t.Fatalf("verdict = %s/%s, want unknown/unknown_error (never invalid, never a guessed answer)", row.Status, row.Reason)
				}
				if row.Confidence != 10 || row.Recommendation != "review" {
					t.Errorf("confidence/recommendation = %d/%s, want 10/review", row.Confidence, row.Recommendation)
				}
				if got, _ := row.Signals[vocab.SignalFailureOrigin].(string); got != "own" {
					t.Errorf("failure_origin = %q, want own (the failure is ours)", got)
				}
				if got, _ := row.Signals[vocab.SignalErrorCapped].(bool); !got {
					t.Errorf("signals.error_capped must be true, got %v", row.Signals[vocab.SignalErrorCapped])
				}
				if len(row.Signals) != 2 {
					t.Errorf("the capped verdict adds exactly error_capped and failure_origin to the bag, got %v", row.Signals)
				}
				if string(row.EmailHash) != "h-cap1" || row.Domain != "ex.com" || row.PolicyKey != "cold-email" || row.ConfigFingerprint != "cfg" || row.APIVersion != "v1" {
					t.Errorf("the row must carry the item's identity and policy provenance: hash=%q domain=%q policy=%q cfg=%q api=%q",
						row.EmailHash, row.Domain, row.PolicyKey, row.ConfigFingerprint, row.APIVersion)
				}
				if row.FromCache {
					t.Error("a capped verdict is computed, never from cache")
				}

				// AFTER THE CAP: terminal means terminal. Jump far ahead and drain again — no further attempt, no second LOG row.
				for i := 0; i < 3; i++ {
					*r.clock = r.clock.Add(2 * time.Hour)
					r.dr.TickOnce(context.Background())
				}
				if got := r.failing(); got != errorCap {
					t.Errorf("a settled item was attempted again: the faulty %s ran %d times, want %d", stage, got, errorCap)
				}
				if len(r.mem.Log) != 1 {
					t.Errorf("a settled item must never log a second verdict, got %d", len(r.mem.Log))
				}
			})
		}
	}
}

// The cap cannot be switched off by omission: Deps.ErrorMaxAttempts == 0 means the default (5), not "unbounded".
func TestDrainer_ErrorCap_UnsetMeansTheDefault(t *testing.T) {
	if DefaultErrorMaxAttempts != 5 {
		t.Fatalf("DefaultErrorMaxAttempts = %d, want 5 (mirrors config.Defaults)", DefaultErrorMaxAttempts)
	}
	r := newCapRig(t, "compute", 0)
	for i := 0; i < DefaultErrorMaxAttempts-1; i++ {
		r.tick()
		if w := r.item(t); w.Status != "waiting_retry" {
			t.Fatalf("failure %d: want waiting_retry, got %s", i+1, w.Status)
		}
	}
	r.tick()
	if w := r.item(t); w.Status != "dead" || r.comp.calls != DefaultErrorMaxAttempts {
		t.Fatalf("with the cap unset the 5th failure must settle: status=%s calls=%d", w.Status, r.comp.calls)
	}
}

// CONTROL: the cap counts FAILURES. An item that errors and then recovers settles normally — it is not capped, and its verdict is the
// real one. (Without this, "settles at the cap" could be a drainer that settles everything unknown_error after a few ticks.)
func TestDrainer_ErrorCap_ARecoveredItemSettlesNormally(t *testing.T) {
	ctx := context.Background()
	r := newCapRig(t, "compute", 5)
	r.comp.okFrom = 5 // calls 1..4 fail (one below the cap), call 5 succeeds
	for i := 0; i < 4; i++ {
		r.tick()
		if w := r.item(t); w.Status != "waiting_retry" || w.Attempt != i+1 {
			t.Fatalf("failure %d: status=%s attempt=%d, want waiting_retry/%d", i+1, w.Status, w.Attempt, i+1)
		}
	}
	r.dr.TickOnce(ctx)
	w := r.item(t)
	if w.Status != "settled" {
		t.Fatalf("an item that recovers on the 5th try is settled normally, got %s", w.Status)
	}
	if len(r.mem.Log) != 1 || r.mem.Log[0].Reason != "accepted" {
		t.Fatalf("the settled verdict must be the real one (accepted), got %+v", r.mem.Log)
	}
	if _, has := r.mem.Log[0].Signals[vocab.SignalErrorCapped]; has {
		t.Error("a recovered item must not carry error_capped")
	}
}

// The capped row is legal by construction: it must pass the same Door-1 validation every logged verdict passes, and it is built from the
// work item alone (the address may never have been decryptable, and no plaintext is needed: the item's hash and domain are the identity).
func TestErrorCappedOutcome_IsALegalVerdictBuiltFromTheItemAlone(t *testing.T) {
	it := store.WorkItemRow{
		ID: capItemID, JobID: "", Domain: "ex.com", EmailHash: []byte("h"), PolicyKey: "cold-email", PolicyVersion: 7,
		ConfigFingerprint: "cfg-x", Status: "in_flight", Attempt: 4, AddrCiphertext: []byte("sealed"),
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	out, err := ErrorCappedOutcome(it, now, "addr_undecryptable")
	if err != nil {
		t.Fatalf("ErrorCappedOutcome: %v", err)
	}
	row := out.Row
	if out.Cache != nil || out.Raw != nil || out.ExternalCall != nil {
		t.Error("a capped outcome carries no cache row, no raw blob, no spend record")
	}
	if out.LastReason != "addr_undecryptable" {
		t.Errorf("last_reason = %q: the operational reason of the failing stage must be kept", out.LastReason)
	}
	if row.ID == "" || row.RequestID == "" || row.ID == row.RequestID {
		t.Errorf("the row needs its own UUIDv7 id and request id, got id=%q request=%q", row.ID, row.RequestID)
	}
	if !vocab.ValidReason(vocab.Reason(row.Reason)) || !vocab.ValidStatus(vocab.Status(row.Status)) || !vocab.ValidRecommendation(vocab.Recommendation(row.Recommendation)) {
		t.Errorf("out-of-vocabulary verdict %s/%s/%s", row.Status, row.Reason, row.Recommendation)
	}
	if !vocab.ComposabilityOK(vocab.Status(row.Status), vocab.Recommendation(row.Recommendation)) {
		t.Errorf("forbidden status/recommendation pair %s/%s", row.Status, row.Recommendation)
	}
	if row.PolicyVersion != 7 || row.ConfigFingerprint != "cfg-x" || row.HashScheme != hash.SchemeTag {
		t.Errorf("provenance wrong: policy_version=%d cfg=%q scheme=%q", row.PolicyVersion, row.ConfigFingerprint, row.HashScheme)
	}
	if !row.CheckedAt.Equal(now) {
		t.Errorf("checked_at = %v, want %v", row.CheckedAt, now)
	}
}
