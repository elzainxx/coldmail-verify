package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/flywheel"
	"github.com/mnfi/codmailling/control-plane/internal/hash"
	"github.com/mnfi/codmailling/control-plane/internal/policy"
	"github.com/mnfi/codmailling/control-plane/internal/store"
	"github.com/mnfi/codmailling/control-plane/internal/vocab"
)

func fwFixedNow() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) }

func setupFlywheel(t *testing.T, fw *FlywheelDeps) (*Orchestrator, *store.MemStore, *policy.Definition) {
	t.Helper()
	h, err := hash.NewHasher([]byte("orch-fw-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	mem := store.NewMem(fwFixedNow)
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	o := New(Deps{
		Hasher: h, Store: mem, Resolver: corpResolver(), CacheTTL: time.Hour,
		ConfigFingerprint: "cfg_sha256:test", Now: fwFixedNow, Flywheel: fw,
	})
	return o, mem, def
}

// TestFreshSystem_FlywheelWiredButInert is the DR-2 / pre-takeoff §3.5 FRESH-SYSTEM INVARIANT: with the
// flywheel WIRED but on a fresh system (no historical bounces, no effective calibration), behavior is
// IDENTICAL to end-of-P6 — zero live mutation. This is the regression gate for shipping A6/A8/A9.
// FAIL-VERIFY: make the calibration read return a fabricated confidence on cold-start → this breaks.
func TestFreshSystem_FlywheelWiredButInert(t *testing.T) {
	// Wire the flywheel with BOTH touches ENABLED — but the store is empty.
	fw := &FlywheelDeps{
		GateEnabled: true, CalibrationEnabled: true, Now: fwFixedNow,
		GateParams: flywheel.HistoricalGateParams{TTL: 180 * 24 * time.Hour, MinIndependent: 2, PenaltyPoints: 30},
	}
	o, mem, def := setupFlywheel(t, fw)

	// Compare against a flywheel-OFF orchestrator on the same input.
	oOff, _, _ := setupFlywheel(t, nil)
	respOff, err := oOff.Verify(context.Background(), "jane.doe@acmecorp.com", def)
	if err != nil {
		t.Fatal(err)
	}
	respOn, err := o.Verify(context.Background(), "jane.doe@acmecorp.com", def)
	if err != nil {
		t.Fatal(err)
	}
	// Byte-identical verdict fields (the master invariant on a fresh system).
	if respOn.Status != respOff.Status || respOn.Reason != respOff.Reason || respOn.Confidence != respOff.Confidence || respOn.Recommendation != respOff.Recommendation {
		t.Fatalf("fresh-system flywheel changed the verdict: on=%s/%s/%d/%s off=%s/%s/%d/%s — DR-2 fresh-system invariant violated",
			respOn.Status, respOn.Reason, respOn.Confidence, respOn.Recommendation,
			respOff.Status, respOff.Reason, respOff.Confidence, respOff.Recommendation)
	}
	if respOn.Signals["calibrated"] != nil {
		t.Fatal("fresh system marked a verdict 'calibrated' — no effective version exists (cold-start, §5.6)")
	}
	_ = mem
}

// TestHistoricalGate_FiresOnLivePath (§4.6): with ≥N confirmed invalid-signal bounces on record within
// the TTL, a re-verify short-circuits to invalid/historical_bounce (ground-truth) — using the RESERVED
// reason (zero new vocab, Door 4). FAIL-VERIFY: disable the gate (GateEnabled=false) → it never fires.
func TestHistoricalGate_FiresOnLivePath(t *testing.T) {
	fw := &FlywheelDeps{
		GateEnabled: true, CalibrationEnabled: false, Now: fwFixedNow,
		GateParams: flywheel.HistoricalGateParams{TTL: 180 * 24 * time.Hour, MinIndependent: 2, PenaltyPoints: 30},
	}
	o, mem, def := setupFlywheel(t, fw)

	// Compute the address's email_hash so we can seed its rollup directly (2 confirmed invalid-signal).
	eh := o.hasher.EmailHash(mustNormalize(t, o, "target@acmecorp.com"))
	seedOutcomeRollup(t, mem, eh, 2, fwFixedNow().Add(-24*time.Hour)) // within TTL

	resp, err := o.Verify(context.Background(), "target@acmecorp.com", def)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != vocab.StatusInvalid || resp.Reason != vocab.ReasonHistoricalBounce {
		t.Fatalf("historical-bounce gate did not fire: got %s/%s, want invalid/historical_bounce (§4.6)", resp.Status, resp.Reason)
	}
	if resp.Recommendation != vocab.RecSkip {
		t.Fatalf("historical_bounce recommendation = %s, want skip (invalid⇒skip composability)", resp.Recommendation)
	}
	// The reserved reason mints NO new vocab — it must be a legal Door-4 value.
	if !vocab.ValidReason(vocab.ReasonHistoricalBounce) {
		t.Fatal("historical_bounce is not a valid vocab reason — Door 4 violated")
	}
	// A SINGLE confirmed bounce (below N=2) must NOT fire the gate (F-17).
	eh2 := o.hasher.EmailHash(mustNormalize(t, o, "single@acmecorp.com"))
	seedOutcomeRollup(t, mem, eh2, 1, fwFixedNow().Add(-24*time.Hour))
	resp2, err := o.Verify(context.Background(), "single@acmecorp.com", def)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.Reason == vocab.ReasonHistoricalBounce {
		t.Fatal("gate fired on ONE outcome — F-17 requires ≥N independent (a single forged outcome must not blacklist)")
	}
}

// TestCalibrationRead_OverridesBaselineWhenEffective (§5.4): when an EFFECTIVE calibration version
// exists for the (reason × provider) bucket, the verdict path reads it INSTEAD of the P1 baseline, and
// the recommendation is re-derived. FAIL-VERIFY: skip the calibrationConfidence call → baseline stands.
func TestCalibrationRead_OverridesBaselineWhenEffective(t *testing.T) {
	fw := &FlywheelDeps{GateEnabled: false, CalibrationEnabled: true, Now: fwFixedNow}
	o, mem, def := setupFlywheel(t, fw)

	// A clean corp address computes to risky/plausible with the P1 baseline confidence (40).
	baseResp, err := o.Verify(context.Background(), "cal@acmecorp.com", def)
	if err != nil {
		t.Fatal(err)
	}
	baselineConf := baseResp.Confidence

	// Seed + approve an effective calibration for the bucket the corp address lands in. acmecorp.com's
	// MX (mx.acmecorp.com) is self-hosted → provider "self"; the residue reason is "plausible".
	id, err := mem.AppendCalibrationVersion(context.Background(), store.CalibrationRow{
		ID: "cal-1", Reason: "plausible", MXProvider: "self", Confidence: 15, SampleN: 800, Mode: "auto", CreatedAt: fwFixedNow(),
	})
	_ = id
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.ApproveCalibration(context.Background(), "cal-1", fwFixedNow()); err != nil {
		t.Fatal(err)
	}

	resp, err := o.Verify(context.Background(), "cal2@acmecorp.com", def)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Confidence != 15 {
		t.Fatalf("calibrated confidence = %d, want 15 (the effective version, not the baseline %d) — §5.4", resp.Confidence, baselineConf)
	}
	if resp.Signals["calibrated"] != true {
		t.Fatal("a calibrated verdict must carry signals.calibrated=true (provenance, §5.4)")
	}
}

// ── helpers ──

func mustNormalize(t *testing.T, o *Orchestrator, raw string) string {
	t.Helper()
	n, _, err := o.hasher.Normalize(raw)
	if err != nil {
		t.Fatalf("normalize %q: %v", raw, err)
	}
	return n
}

// seedOutcomeRollup appends `n` confirmed invalid-signal outcomes for an email_hash (the §4.6 rollup),
// each a distinct (message_id, event) so they count as independent. They must join to a real verdict
// (GetVerdictForJoin), so we seed a matching verdict per outcome — but the rollup COUNT is what the
// gate reads via HistoricalBounceCount, which counts calibration_class='invalid_signal' rows directly.
func seedOutcomeRollup(t *testing.T, mem *store.MemStore, emailHash []byte, n int, occurredAt time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		mem.SeedSendOutcome(store.SendOutcomeRow{
			ID:               "o-" + string(rune('a'+i)),
			EmailHash:        emailHash,
			MessageID:        "mid-" + string(rune('a'+i)),
			Event:            "550 5.1.1 user unknown",
			CalibrationClass: "invalid_signal",
			OccurredAt:       occurredAt,
		})
	}
}
