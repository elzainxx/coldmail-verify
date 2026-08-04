package flywheel

import (
	"context"
	"time"

	"github.com/coldmail-verify/control-plane/internal/idgen"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// This file: the OUTCOME INGESTION path (§3) — join, integrity reject-and-flag, classify, append.
//
// The transport is behind the OutcomeSource INTERFACE (ADR-0013 §B.3, the Door-5 "interfaces at the
// seams" discipline). The DECIDED mechanism is mutually-authenticated pull (mTLS) — a PullSource
// adapter — but the ingestor consumes CLASSIFIED outcomes, never the transport, so switching to signed
// webhooks later is a NEW adapter with zero change here. Local build uses SimulatedSource (an
// authenticated-by-construction in-memory feed) so the whole flywheel is buildable + fail-verifiable
// now; the real mTLS transport is wired when the sender ("Feature 2") exists — PENDING-REAL-SENDER.
//
// FAIL-CLOSED (ADR-0013 §B.2 pt 5): an unauthenticated / failed source ingests NOTHING (the flywheel
// stays cold-start on the P1 baseline) rather than ingesting unauthenticated data. The fail-verify
// test asserts an unauthenticated source's outcomes never reach the classifier.

// RawOutcome is one outcome as it arrives from the sender (§3.2 LOCKED shape) — BEFORE the join +
// classification. The sender supplies these; P7 backfills the rollup dims from the joined verdict.
type RawOutcome struct {
	VerificationID string    // the authoritative join anchor (§3.1)
	MessageID      string    // idempotency (§3.4, with Event)
	Event          string    // RAW provider event/DSN — normalized by the classifier (never trusted, §3.2)
	SentAt         time.Time // temporal attribution (§3.3)
	OccurredAt     time.Time // when the bounce/delivery happened
	Raw            []byte    // untouched provider event (fidelity copy)
}

// OutcomeSource is the swappable, AUTHENTICATED outcome feed (ADR-0013 §B.3). Authenticated() reports
// whether the source's transport authentication is satisfied — a false answer means the ingestor
// refuses to pull (fail-closed). Pull returns a batch of outcomes since the given cursor plus the next
// cursor (the durable watermark, replay-safe). The mTLS PullSource, the SimulatedSource, and a future
// WebhookSource all satisfy this — the classifier/calibration never see the difference.
type OutcomeSource interface {
	// Authenticated reports whether the source's principal is authenticated (mTLS handshake ok / a real
	// credential is present). The ingestor pulls NOTHING when this is false (ADR-0013 fail-closed).
	Authenticated() bool
	// SourceKey identifies the authenticated feed source (the watermark key).
	SourceKey() string
	// Pull returns outcomes after `cursor` (opaque, sender-defined ordering) and the next cursor. An
	// empty batch with the same cursor means "caught up." A transport error means "could not pull" —
	// the ingestor ingests nothing this cycle (never fabricates).
	Pull(ctx context.Context, cursor string, limit int) (batch []RawOutcome, nextCursor string, err error)
}

// Ingestor joins, integrity-checks, classifies, and appends outcomes (§3). It reads the joined verdict
// (for the join + the §4.4 exclusions), classifies via the bounce classifier (§4), and appends the
// classified outcome (deduped on message_id+event, §3.4). It NEVER mutates the LOG (Door 2) and never
// calibrates here — recalibration is a separate, gated pass (§5).
type Ingestor struct {
	Store       store.Store
	Now         func() time.Time
	OnCatchAll  func(ctx context.Context, domain string) bool // reads catchall_domains (§4.5 weighting); nil => treat as not catch-all
}

// IngestResult summarizes one pull-and-classify cycle (observability §10: ingestion health).
type IngestResult struct {
	Pulled     int // outcomes pulled from the source
	Ingested   int // appended (passed the join; classified — includes flagged/excluded rows, recorded)
	Duplicates int // §3.4 no-ops (replayed deliveries)
	Flagged    int // §3.4/GAP-51 integrity rejects (stored raw + ingest_error, EXCLUDED from calibration)
	SkippedAuth bool // true => the source was not authenticated → NOTHING ingested (fail-closed)
}

// IngestOnce pulls one batch from the source and ingests it (§3). It is FAIL-CLOSED: if the source is
// not authenticated it returns immediately having ingested nothing (ADR-0013 — recommend-only mitigates
// but does not authenticate; authentication is the root defense). It advances + persists the durable
// watermark only after a successful batch, so a replay re-reads nothing and the (message_id,event)
// dedup is belt-and-suspenders.
func (ing *Ingestor) IngestOnce(ctx context.Context, src OutcomeSource, limit int) (IngestResult, error) {
	var res IngestResult
	// FAIL-CLOSED root defense (ADR-0013 §B.2 pt 5 / TB-7): never ingest from an unauthenticated source.
	if src == nil || !src.Authenticated() {
		res.SkippedAuth = true
		return res, nil
	}
	cursor, _, err := ing.Store.GetOutcomeCursor(ctx, src.SourceKey())
	if err != nil {
		return res, err
	}
	batch, nextCursor, err := src.Pull(ctx, cursor, limit)
	if err != nil {
		// A transport error → ingest nothing this cycle (never fabricate). The watermark is unchanged,
		// so the next cycle retries from the same point.
		return res, err
	}
	res.Pulled = len(batch)
	for _, raw := range batch {
		outcome, err := ing.classifyOne(ctx, raw)
		if err != nil {
			return res, err
		}
		if appendErr := ing.Store.AppendSendOutcome(ctx, outcome); appendErr != nil {
			if appendErr == store.ErrOutcomeDuplicate {
				res.Duplicates++ // §3.4 replay no-op
				continue
			}
			return res, appendErr
		}
		res.Ingested++
		if outcome.IngestError != "" {
			res.Flagged++
		}
	}
	// Advance the durable watermark only after the batch is appended (replay-safe).
	if nextCursor != "" && nextCursor != cursor {
		if err := ing.Store.SetOutcomeCursor(ctx, src.SourceKey(), nextCursor, ing.now()); err != nil {
			return res, err
		}
	}
	return res, nil
}

// classifyOne performs the join + integrity checks (§3.3/§3.4/GAP-51) + classification (§4) for one raw
// outcome, producing the store row. An integrity failure is stored with an ingest_error marker and
// EXCLUDED from calibration (a poisoned join is worse than a missing one) — it is NEVER guessed or
// dropped silently. The §4.4 exclusions (from_cache, override, external_verified-separate) are applied
// from the joined verdict.
func (ing *Ingestor) classifyOne(ctx context.Context, raw RawOutcome) (store.SendOutcomeRow, error) {
	now := ing.now()
	row := store.SendOutcomeRow{
		ID:             newOutcomeID(now),
		VerificationID: raw.VerificationID,
		MessageID:      raw.MessageID,
		Event:          raw.Event,
		SentAt:         raw.SentAt,
		OccurredAt:     raw.OccurredAt,
		Raw:            raw.Raw,
		IngestedAt:     now,
	}

	// §3.4/GAP-51 integrity: an outcome with no verification_id is a send with no prior verdict — a
	// join contradiction. Store raw + flag; exclude from calibration.
	if raw.VerificationID == "" {
		row.IngestError = "unknown_verification_id"
		return row, nil
	}
	vj, known, err := ing.Store.GetVerdictForJoin(ctx, raw.VerificationID)
	if err != nil {
		return store.SendOutcomeRow{}, err
	}
	if !known {
		// §3.4: unknown verification_id (no such verdict) → reject-and-flag (GAP-51). A poisoned join
		// is worse than a missing one — never guess which verdict it "meant."
		row.IngestError = "unknown_verification_id"
		return row, nil
	}
	// §3.3: temporal integrity — a send attributed to a verdict that didn't exist yet is a contradiction.
	if !raw.SentAt.IsZero() && !vj.CreatedAt.IsZero() && raw.SentAt.Before(vj.CreatedAt) {
		row.IngestError = "sent_before_verdict"
		return row, nil
	}

	// Backfill the rollup dims from the JOINED verdict (captured at verify time — §3.2: "the rest I can
	// derive from the verdict"). This is the authoritative provenance for the calibration bucket.
	row.EmailHash = vj.EmailHash
	row.Domain = vj.Domain
	row.MXProvider = vj.MXProvider
	row.FromCache = vj.FromCache

	// §4.4 external-verified separation (GAP-54): a rented verdict's outcome buckets SEPARATELY (by
	// vendor × residue_class), never merged into SMTP-earned buckets — else we train on the vendor's
	// errors and can never wean (§9). Carry the provenance from the joined verdict's signals.
	if vj.Reason == string(vocab.ReasonExternalVerified) {
		row.IsExternalVerified = true
		row.ResidueClass = vj.ResidueClass
		row.Vendor = vj.Vendor
	}

	// Classify (§4): normalize the RAW event (never trust a pre-classified label, §3.2), then map to a
	// calibration class + strength. onCatchAll down-weights a delivered-on-catch-all to near-worthless.
	ev := NormalizeEvent(raw.Event)
	onCatchAll := false
	if ing.OnCatchAll != nil && vj.Domain != "" {
		onCatchAll = ing.OnCatchAll(ctx, vj.Domain)
	}
	cls := Classify(ev, onCatchAll)

	// §4.4 exclusion discipline stamped onto the row (enforced again at the aggregate read):
	//  - from_cache / override verdicts → 'excluded' (recorded, NOT calibrated).
	//  - a near-worthless valid-signal (delivered-on-catch-all, strength 0) → 'excluded' (§4.5).
	//  - everything else → the classifier's class.
	switch {
	case vj.FromCache || vj.IsOverride:
		row.CalibrationClass = "excluded"
	case !cls.Calibrates():
		if cls.Class == ClassZeroInformation {
			row.CalibrationClass = string(ClassZeroInformation) // the majority; recorded for sender-health
		} else {
			row.CalibrationClass = "excluded" // delivered-on-catch-all (§4.5): recorded, not calibrated
		}
	default:
		row.CalibrationClass = string(cls.Class)
	}
	return row, nil
}

func (ing *Ingestor) now() time.Time {
	if ing.Now != nil {
		return ing.Now()
	}
	return time.Now()
}

// newOutcomeID mints the send_outcomes row id (UUIDv7, time-ordered). A crypto/rand failure is
// vanishingly unlikely; fall back to a nil-ish UUID rather than panicking the ingestion loop.
func newOutcomeID(now time.Time) string {
	id, err := idgen.NewV7String(now)
	if err != nil {
		return "00000000-0000-7000-8000-000000000000"
	}
	return id
}
