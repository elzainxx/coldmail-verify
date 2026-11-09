// Package queue's drainer is the P5 background loop that turns durable work-items into checks
// through the UNCHANGED Phase-4 router (§3, §8 master invariant): dequeue → ComputeForQueue (the
// identical eligibility→sticky→rate-limit→warmup path a sync /v1/verify runs) → settle-or-requeue
// via the GAP-37 compare-and-set. The queue changes WHEN a check runs, never WHETHER the
// safeguards apply — there is no drain-mode branch that skips a gate.
package queue

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/coldmail-verify/control-plane/internal/idgen"
	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/store"
)

// Computer is the log-FREE compute path the drainer runs each item through — satisfied by
// *orchestrator.Orchestrator.ComputeForQueue. Injected so drainer tests use a fake without a real
// orchestrator/DB. The drainer NEVER calls Verify (which logs unconditionally); it settles via CAS.
//
// EscalateExhausted is the P6 give-up escalation (§2.2, §4.1 classes c/d): at retry-exhaustion the
// drainer offers the residue verdict (about to be settled "dead") to the fallback for ONE rented
// answer. It returns a possibly-replaced QueueOutcome (external_verified at capped confidence + the
// spend record recorded inside the settle CAS — spend-once, GAP-42). Not eligible / fallback off /
// breaker open / vendor unknown → the outcome is returned UNCHANGED (settle the residue as-is).
type Computer interface {
	ComputeForQueue(ctx context.Context, rawEmail string, def *policy.Definition, jobID string) (orchestrator.QueueOutcome, error)
	EscalateExhausted(ctx context.Context, addr string, def *policy.Definition, out orchestrator.QueueOutcome, jobID, workItemID string) orchestrator.QueueOutcome
}

// PolicyResolver loads a policy definition for a work-item (§3 — a queued check runs under the same
// policy it was enqueued with). Satisfied by a small adapter over the policy store/cache.
type PolicyResolver interface {
	Resolve(ctx context.Context, key string, version int) (*policy.Definition, error)
}

// Decryptor opens the DR-4-encrypted plaintext address for an attempt (§3.3). Satisfied by
// *AddrCipher. The plaintext lives ONLY here (in memory, for the attempt) — never logged, never
// persisted in clear; the ciphertext is purged from the row in the settlement tx.
type Decryptor interface {
	Open(blob []byte, aad []byte) (string, error)
}

// Deps wires the drainer. All are interfaces/values so tests inject fakes.
type Deps struct {
	Store    store.Store
	Compute  Computer
	Policy   PolicyResolver
	Cipher   Decryptor
	Backoff  Backoff
	Now      func() time.Time
	WorkerID string // identifies this drainer's lease-holder (GAP-37 CAS key). Non-empty.

	// LeaseTTL MUST exceed the brain's max SMTP client deadline (GAP-37): a *live* attempt must
	// never lose its lease, so a reclaim provably means the holder is dead, not slow. NewDrainer
	// asserts LeaseTTL > MaxSMTPDeadline (fail-fast) — a misconfiguration here degrades the
	// exactly-once guarantee to a re-attempt race.
	LeaseTTL        time.Duration
	MaxSMTPDeadline time.Duration

	// MaxAttempts / MaxRetryAge are the §4.5 ceilings; whichever trips first settles the item as
	// the reserved unknown/greylisted verdict (never invalid). At least one must be > 0.
	MaxAttempts int
	MaxRetryAge time.Duration

	// BatchSize is how many items the drainer leases per tick (§6); IdlePoll is the sleep when the
	// queue is empty; ReclaimEvery gates how often the lease-reclaim sweep runs.
	BatchSize    int
	IdlePoll     time.Duration
	ReclaimEvery time.Duration

	// Paused reports whether verdict production is paused — kill-switch Rung 4 accept-and-defer,
	// whose definition IS "the drainer is stopped: work sits durably in work_items, nothing is
	// processed" (history/kill-switch-ladder.md Rung 4). Satisfied in production by
	// cfg.KillSwitch.VerdictProductionPaused; nil => never paused (the Rung-0..3 default).
	//
	// It is a FUNC, not a bool, for two reasons. First, it keeps the queue package free of an
	// internal/config dependency — the drainer's contract is "am I allowed to produce verdicts right
	// now", not "what does the ladder look like". Second, the check is made per tick, so if the
	// process ever learns to re-read config (there is no reload today), a pull of the rung takes
	// effect at the next tick with no further plumbing.
	//
	// WHY THE PAUSE IS HERE AND NOT AT DEQUEUE. Stopping BEFORE DequeueReady means a paused drainer
	// takes no lease, so no item is left in_flight holding a lease that nothing will settle. If the
	// gate were after the lease, pausing would strand every in-flight item until the lease expired
	// and the reclaim sweep (which is also paused) put it back — a pause that quietly damages the
	// queue instead of freezing it.
	Paused func() bool
}

// Drainer is the background work-item loop. Single-brain (FOR UPDATE SKIP LOCKED, no broker —
// GAP-34/OQ-P5-1); the WorkerID + the CAS are what make settlement exactly-once (GAP-37).
type Drainer struct {
	d           Deps
	now         func() time.Time
	lastReclaim time.Time
	pausedLast  bool // last observed pause state, so the transition is logged once, not per tick
}

// ErrLeaseTTLTooShort is returned by NewDrainer when LeaseTTL <= MaxSMTPDeadline — the GAP-37
// invariant. Refusing to start is the harden-toward-safety choice: a too-short lease silently
// reintroduces the slow-drainer double-attempt race the CAS exists to close.
var ErrLeaseTTLTooShort = errors.New("queue: lease TTL must exceed the brain's max SMTP deadline (GAP-37)")

// NewDrainer validates the GAP-37 lease invariant and wires the loop.
func NewDrainer(d Deps) (*Drainer, error) {
	if d.LeaseTTL <= d.MaxSMTPDeadline {
		return nil, ErrLeaseTTLTooShort
	}
	if d.MaxAttempts <= 0 && d.MaxRetryAge <= 0 {
		return nil, errors.New("queue: at least one of MaxAttempts / MaxRetryAge must be set (§4.5)")
	}
	if d.WorkerID == "" {
		return nil, errors.New("queue: WorkerID is required (the GAP-37 CAS lease-holder key)")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	if d.BatchSize <= 0 {
		d.BatchSize = 10
	}
	if d.IdlePoll <= 0 {
		d.IdlePoll = 2 * time.Second
	}
	if d.ReclaimEvery <= 0 {
		d.ReclaimEvery = d.LeaseTTL // sweep about once per lease lifetime
	}
	if d.Paused == nil {
		// Never paused is the correct default (Rungs 0–3), and it must be a func that ANSWERS rather
		// than a nil we test for at every call site.
		d.Paused = func() bool { return false }
	}
	return &Drainer{d: d, now: now}, nil
}

// Run loops until ctx is cancelled: reclaim expired leases (periodically), lease a batch, and drain
// each item. It is the ONLY writer of work-item transitions on this brain (single-brain). Errors
// are logged and the loop continues (a transient store error must not wedge the queue).
func (dr *Drainer) Run(ctx context.Context) {
	log.Printf("queue drainer: starting (worker=%s lease_ttl=%s batch=%d)", dr.d.WorkerID, dr.d.LeaseTTL, dr.d.BatchSize)
	for {
		select {
		case <-ctx.Done():
			log.Printf("queue drainer: stopping (%v)", ctx.Err())
			return
		default:
		}
		worked := dr.tick(ctx)
		if !worked {
			select {
			case <-ctx.Done():
				return
			case <-time.After(dr.d.IdlePoll):
			}
		}
	}
}

// tick runs one drain cycle: an occasional reclaim sweep, then a lease-and-drain batch. It returns
// whether it did any work (false → the caller idle-sleeps). Exported-for-test via TickOnce.
//
// RUNG 4 (accept-and-defer) IS ENFORCED FIRST, before anything touches the queue: the rung's
// definition is that work sits durably in work_items and nothing is processed. A paused tick does no
// reclaim, takes no lease, runs no check, and writes no LOG row — it does nothing at all and reports
// "no work", so Run idle-sleeps. That is a freeze, not a drop: every item keeps its status, attempt
// count, and not_before, so unpausing resumes with zero loss (§4.4 restart-safety, same property).
func (dr *Drainer) tick(ctx context.Context) bool {
	if paused := dr.d.Paused(); paused || dr.pausedLast {
		// Log only the TRANSITIONS. An operator needs to see the drainer agree that it is paused —
		// D2 was exactly the failure of a control that changed nothing and said nothing — but a line
		// every IdlePoll would bury the incident's real signal.
		if paused != dr.pausedLast {
			if paused {
				log.Printf("queue drainer: PAUSED (kill-switch Rung 4 accept-and-defer) — submissions still accepted and stored durably; NOTHING is processed until the rung is released")
			} else {
				log.Printf("queue drainer: RESUMED (Rung 4 released) — draining the work that accumulated while paused")
			}
			dr.pausedLast = paused
		}
		if paused {
			return false // no work done; Run idle-sleeps. Nothing was leased, so nothing is stranded.
		}
	}
	now := dr.now()
	// Periodic lease-reclaim sweep (§5.2): an in_flight item whose lease predates now-LeaseTTL is
	// presumed dead and re-readied. Because LeaseTTL > MaxSMTPDeadline, a live attempt is never
	// reclaimed (GAP-37) — a reclaim provably means the holder crashed.
	if now.Sub(dr.lastReclaim) >= dr.d.ReclaimEvery {
		if n, err := dr.d.Store.ReclaimExpiredLeases(ctx, dr.d.LeaseTTL, now); err != nil {
			log.Printf("queue drainer: reclaim error: %v", err)
		} else if n > 0 {
			log.Printf("queue drainer: reclaimed %d expired lease(s)", n)
		}
		dr.lastReclaim = now
	}

	items, err := dr.d.Store.DequeueReady(ctx, dr.d.WorkerID, dr.d.BatchSize, now)
	if err != nil {
		log.Printf("queue drainer: dequeue error: %v", err)
		return false
	}
	if len(items) == 0 {
		return false
	}
	for _, it := range items {
		if ctx.Err() != nil {
			return true
		}
		dr.drainOne(ctx, it)
	}
	return true
}

// TickOnce runs exactly one drain cycle (test entry point): reclaim + one batch. Returns whether it
// did work. Production uses Run.
func (dr *Drainer) TickOnce(ctx context.Context) bool { return dr.tick(ctx) }

// drainOne runs one leased item through the P4 gate path and applies the resulting transition via
// the GAP-37 CAS. A compute error requeues as a short capacity wait (the item is not lost). An
// ErrLeaseLost from the CAS is EXPECTED and benign (another attempt owns the item — GAP-37): we log
// at debug level and move on; the item is NOT ours to transition.
func (dr *Drainer) drainOne(ctx context.Context, it store.WorkItemRow) {
	def, err := dr.d.Policy.Resolve(ctx, it.PolicyKey, it.PolicyVersion)
	if err != nil {
		dr.requeueCapacity(ctx, it, "policy_unavailable")
		return
	}
	// DR-4: open the plaintext address for THIS attempt (AAD = the row id, anti-replay). The
	// plaintext lives only here, in memory, for the attempt — never logged, never re-persisted.
	addr, err := dr.d.Cipher.Open(it.AddrCiphertext, []byte(it.ID))
	if err != nil {
		// A decrypt failure (missing/rotated-out key, corrupt/moved ciphertext) can't run SMTP —
		// requeue as a capacity wait so a transient key issue self-heals; if it persists the item
		// eventually hits max_retry_age and settles unknown (never invalid, never a guessed verdict).
		log.Printf("queue drainer: addr decrypt failed for item %s (attempt %d): %v", it.ID, it.Attempt, err)
		dr.requeueCapacity(ctx, it, "addr_undecryptable")
		return
	}

	out, err := dr.d.Compute.ComputeForQueue(ctx, addr, def, it.JobID)
	if err != nil {
		dr.requeueCapacity(ctx, it, "compute_error")
		return
	}
	dr.apply(ctx, it, out, addr, def)
}

// apply maps a QueueOutcome onto a work-item transition (§3.5), all via the GAP-37 CAS. See the
// table in the P5 spec §4/§8.3. Every branch's store call is a CAS on (id, locked_by=<me>,
// status='in_flight'); an ErrLeaseLost means a reclaim already re-owned the item — we discard.
func (dr *Drainer) apply(ctx context.Context, it store.WorkItemRow, out orchestrator.QueueOutcome, addr string, def *policy.Definition) {
	now := dr.now()
	switch out.Class {
	case orchestrator.QSettled:
		// A terminal verdict. compute() ALREADY escalated the terminal residue (catch_all/
		// provider_no_smtp) inline (out.ExternalCall may be set) — nothing more to do here; settle.
		dr.settle(ctx, it, "settled", out)

	case orchestrator.QGreylisted, orchestrator.QTransientNet:
		// A provider-transient: retry on backoff, bumping the attempt count — UNLESS the retry
		// budget is exhausted, in which case settle as the reserved unknown/greylisted verdict
		// ComputeForQueue already produced (§4.5 — retrying doesn't manufacture certainty; exhausting
		// is itself an honest terminal unknown, NEVER invalid). At exhaustion (the give-up path), P6
		// offers the retry-exhausted-greylisted residue (§4.1c) to the fallback for ONE rented answer.
		age := now.Sub(it.CreatedAt)
		if Exhausted(it.Attempt+1, dr.d.MaxAttempts, age, dr.d.MaxRetryAge) {
			out = dr.d.Compute.EscalateExhausted(ctx, addr, def, out, it.JobID, it.ID)
			dr.settle(ctx, it, "dead", out)
			return
		}
		due := now.Add(dr.d.Backoff.NextGreylist(it.Attempt, out.RetryHintS))
		dr.requeue(ctx, it, due, out.LastReason, true /* bump attempt */)

	case orchestrator.QRateLimited, orchestrator.QNoEligibleNode:
		// A capacity/health wait (§4.6/OQ-P5-7): the limiter created backpressure, the queue absorbs
		// it. Requeue on a SHORT backoff WITHOUT bumping the attempt count (a capacity wait must not
		// burn the greylist retry budget). It is still bounded by max_retry_age so it can't linger
		// forever — if the pool never recovers the item eventually settles the honest cheap verdict.
		age := now.Sub(it.CreatedAt)
		if dr.d.MaxRetryAge > 0 && age >= dr.d.MaxRetryAge {
			// Give up waiting for capacity → settle the cheap/degraded verdict compute produced
			// (risky/plausible + smtp_skipped_reason). Terminal, honest, Door-4-clean. At this give-up
			// point P6 offers the gave-up-capacity residue (§4.1d) to the fallback for a rented answer.
			out = dr.d.Compute.EscalateExhausted(ctx, addr, def, out, it.JobID, it.ID)
			dr.settle(ctx, it, "dead", out)
			return
		}
		due := now.Add(dr.d.Backoff.NextRateWait())
		dr.requeue(ctx, it, due, out.LastReason, false /* capacity wait: no bump */)
	}
}

// settle commits the terminal verdict via the GAP-37 CAS settlement (log-once, purge plaintext,
// bump job counter), then writes the cache post-settle if the verdict is cacheable (§5.5, mirrors
// the sync path's separate cache write). terminal ∈ {settled, dead}.
func (dr *Drainer) settle(ctx context.Context, it store.WorkItemRow, terminal string, out orchestrator.QueueOutcome) {
	attempt := dr.attemptRow(it, out.Row, out.LastReason)
	// P6 (§5.4/GAP-42): stamp the work-item id on the spend record (the drainer owns it) so a queued
	// escalation's charge is traceable to its item. SettleWorkItem records it INSIDE the CAS — a lost
	// lease discards the settle AND the charge (spend-once).
	if out.ExternalCall != nil {
		out.ExternalCall.WorkItemID = it.ID
	}
	err := dr.d.Store.SettleWorkItem(ctx, it.ID, dr.d.WorkerID, terminal, out.Row, out.Raw, attempt, out.ExternalCall)
	if errors.Is(err, store.ErrLeaseLost) {
		return // another attempt owns it (GAP-37) — discard, do not re-settle or double-log
	}
	if err != nil {
		log.Printf("queue drainer: settle error for item %s: %v", it.ID, err)
		return
	}
	// Cache the settled verdict (only when cacheable — ComputeForQueue set out.Cache accordingly).
	// A cache write failure is best-effort (the verdict is already logged authoritatively).
	if out.Cache != nil {
		if cerr := dr.d.Store.PutCachedVerdict(ctx, *out.Cache); cerr != nil {
			log.Printf("queue drainer: cache write for item %s: %v", it.ID, cerr)
		}
	}
}

// requeue transitions the item to waiting_retry via the GAP-37 CAS. bumpAttempt distinguishes a
// provider-transient (true) from a capacity wait (false, §4.6/OQ-P5-7).
func (dr *Drainer) requeue(ctx context.Context, it store.WorkItemRow, due time.Time, reason string, bumpAttempt bool) {
	attempt := dr.attemptRow(it, store.VerificationRow{}, reason)
	err := dr.d.Store.RequeueWorkItem(ctx, it.ID, dr.d.WorkerID, due, reason, bumpAttempt, attempt)
	if errors.Is(err, store.ErrLeaseLost) {
		return // another attempt owns it (GAP-37) — discard
	}
	if err != nil {
		log.Printf("queue drainer: requeue error for item %s: %v", it.ID, err)
	}
}

// requeueCapacity is the short-backoff capacity wait used when the item couldn't even be computed
// (policy/decrypt/compute error) — treated like QNoEligibleNode: wait, don't bump, don't lose.
func (dr *Drainer) requeueCapacity(ctx context.Context, it store.WorkItemRow, reason string) {
	now := dr.now()
	if dr.d.MaxRetryAge > 0 && now.Sub(it.CreatedAt) >= dr.d.MaxRetryAge {
		// Exhausted the wait window with no computable verdict — release the lease back to ready so
		// the reclaim/next tick can retry; we do NOT fabricate a verdict here (harden-toward-unknown).
		dr.requeue(ctx, it, now, reason, false)
		return
	}
	dr.requeue(ctx, it, now.Add(dr.d.Backoff.NextRateWait()), reason, false)
}

// attemptRow builds the per-attempt transcript (OQ-P5-8). Node/verifier_ip come from the settled
// verdict row when SMTP ran; reason is the attempt's outcome. Signals are omitted here (the verdict
// LOG carries the full settled signals; the transcript is a lightweight per-try record).
func (dr *Drainer) attemptRow(it store.WorkItemRow, v store.VerificationRow, reason string) store.WorkItemAttemptRow {
	id, err := idgen.NewV7String(dr.now())
	if err != nil {
		return store.WorkItemAttemptRow{} // no transcript (settle/requeue still holds — insertAttemptTx skips empty id)
	}
	return store.WorkItemAttemptRow{
		ID: id, WorkItemID: it.ID, Attempt: it.Attempt, AttemptedAt: dr.now(),
		NodeID: v.NodeID, VerifierIP: v.VerifierIP, Reason: reason,
	}
}
