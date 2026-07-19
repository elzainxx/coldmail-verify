package flywheel

import (
	"context"
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/store"
)

func fixedNow() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) }

// seedVerdict appends a verdict to the LOG so an outcome can join to it.
func seedVerdict(m *store.MemStore, id, status, reason, provider string, fromCache bool, createdAt time.Time, signals map[string]any) {
	_ = m.AppendVerification(context.Background(), store.VerificationRow{
		ID: id, Status: status, Reason: reason, MXProvider: provider, FromCache: fromCache,
		CreatedAt: createdAt, EmailHash: []byte(id + "-hash"), Domain: "example.com", Signals: signals,
	}, nil)
}

func newIngestor(m *store.MemStore, onCatchAll func(context.Context, string) bool) *Ingestor {
	return &Ingestor{Store: m, Now: fixedNow, OnCatchAll: onCatchAll}
}

// TestIngest_BlockedNeverCalibrates is the end-to-end fail-verify (§4.3): a blocked bounce ingested
// against a real verdict is recorded as zero-information and NEVER enters a calibration bucket. This is
// the "remove the exclusion → the test breaks" acceptance criterion, at the ingestion+aggregate level.
func TestIngest_BlockedNeverCalibrates(t *testing.T) {
	m := store.NewMem(fixedNow)
	seedVerdict(m, "v-block", "valid", "accepted", "google", false, fixedNow().Add(-time.Hour), nil)
	ing := newIngestor(m, nil)
	src := NewSimulatedSource("t")
	src.Add(RawOutcome{VerificationID: "v-block", MessageID: "m1", Event: "550 blocked by reputation",
		SentAt: fixedNow(), OccurredAt: fixedNow()})

	if _, err := ing.IngestOnce(context.Background(), src, 10); err != nil {
		t.Fatal(err)
	}
	// The outcome was recorded (for sender-health) but classified zero_information.
	if len(m.SendOutcomes) != 1 || m.SendOutcomes[0].CalibrationClass != string(ClassZeroInformation) {
		t.Fatalf("blocked outcome class = %q, want zero_information (§4.3)", m.SendOutcomes[0].CalibrationClass)
	}
	// It must NOT appear in any calibration bucket (numerator OR denominator).
	aggs, _ := m.BucketAggregatesSince(context.Background(), fixedNow().Add(-24*time.Hour), fixedNow())
	for _, a := range aggs {
		if a.QualifyingN > 0 {
			t.Fatalf("a blocked bounce entered a calibration bucket (%+v) — the GAP-49 poisoning the exclusion discipline prevents", a)
		}
	}
}

// TestIngest_MailboxNotFoundCalibrates — the gold signal DOES enter the invalid numerator.
func TestIngest_MailboxNotFoundCalibrates(t *testing.T) {
	m := store.NewMem(fixedNow)
	seedVerdict(m, "v-nf", "valid", "accepted", "google", false, fixedNow().Add(-time.Hour), nil)
	ing := newIngestor(m, nil)
	src := NewSimulatedSource("t")
	src.Add(RawOutcome{VerificationID: "v-nf", MessageID: "m1", Event: "550 5.1.1 user unknown",
		SentAt: fixedNow(), OccurredAt: fixedNow()})
	if _, err := ing.IngestOnce(context.Background(), src, 10); err != nil {
		t.Fatal(err)
	}
	aggs, _ := m.BucketAggregatesSince(context.Background(), fixedNow().Add(-24*time.Hour), fixedNow())
	if len(aggs) != 1 || aggs[0].InvalidN != 1 || aggs[0].Reason != "accepted" || aggs[0].MXProvider != "google" {
		t.Fatalf("mailbox_not_found did not land in the (accepted×google) invalid numerator: %+v", aggs)
	}
}

// TestIngest_ReplayNoDoubleCount is the §3.4 idempotency test: a replayed (message_id, event) is a
// no-op — it does not double-count into a bucket. FAIL-VERIFY: drop the dedup in AppendSendOutcome →
// the replayed outcome doubles the numerator → this breaks.
func TestIngest_ReplayNoDoubleCount(t *testing.T) {
	m := store.NewMem(fixedNow)
	seedVerdict(m, "v-r", "valid", "accepted", "google", false, fixedNow().Add(-time.Hour), nil)
	ing := newIngestor(m, nil)
	src := NewSimulatedSource("t")
	dup := RawOutcome{VerificationID: "v-r", MessageID: "same", Event: "550 5.1.1 user unknown", SentAt: fixedNow(), OccurredAt: fixedNow()}
	src.Add(dup)
	src.Add(dup) // the SAME (message_id, event) delivered twice (ESP retry)
	res, err := ing.IngestOnce(context.Background(), src, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicates != 1 {
		t.Fatalf("duplicates = %d, want 1 (the §3.4 replay no-op)", res.Duplicates)
	}
	aggs, _ := m.BucketAggregatesSince(context.Background(), fixedNow().Add(-24*time.Hour), fixedNow())
	if len(aggs) != 1 || aggs[0].InvalidN != 1 {
		t.Fatalf("replay double-counted into the bucket: %+v (want InvalidN=1)", aggs)
	}
}

// TestIngest_IntegrityRejectAndFlag (§3.4/GAP-51): an unknown verification_id / a sent-before-verdict
// contradiction is stored raw + flagged, and EXCLUDED from calibration — never guessed, never dropped.
func TestIngest_IntegrityRejectAndFlag(t *testing.T) {
	m := store.NewMem(fixedNow)
	seedVerdict(m, "v-ok", "valid", "accepted", "google", false, fixedNow(), nil)
	ing := newIngestor(m, nil)
	src := NewSimulatedSource("t")
	src.Add(RawOutcome{VerificationID: "nonexistent", MessageID: "m1", Event: "550 5.1.1 user unknown", SentAt: fixedNow(), OccurredAt: fixedNow()})
	// sent_at BEFORE the verdict's created_at → temporal contradiction.
	src.Add(RawOutcome{VerificationID: "v-ok", MessageID: "m2", Event: "550 5.1.1 user unknown", SentAt: fixedNow().Add(-time.Hour), OccurredAt: fixedNow()})
	res, err := ing.IngestOnce(context.Background(), src, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Flagged != 2 {
		t.Fatalf("flagged = %d, want 2 (unknown verification_id + sent-before-verdict, GAP-51)", res.Flagged)
	}
	aggs, _ := m.BucketAggregatesSince(context.Background(), fixedNow().Add(-24*time.Hour), fixedNow())
	if len(aggs) != 0 {
		t.Fatalf("a flagged outcome entered calibration: %+v — a poisoned join is worse than a missing one (§3.4)", aggs)
	}
}

// TestIngest_ExternalVerifiedBucketsSeparately (§4.4.3/§5.2/GAP-54): an outcome joined to a rented
// (external_verified) verdict buckets SEPARATELY (vendor set), never merged with SMTP-earned buckets.
// FAIL-VERIFY: drop the vendor/residue_class carry in classifyOne → it merges → this breaks.
func TestIngest_ExternalVerifiedBucketsSeparately(t *testing.T) {
	m := store.NewMem(fixedNow)
	// A rented verdict: reason=external_verified with signals.external.{vendor,residue_class}.
	seedVerdict(m, "v-ext", "risky", "external_verified", "google", false, fixedNow().Add(-time.Hour),
		map[string]any{"external": map[string]any{"vendor": "millionverifier", "residue_class": "catch_all"}})
	// A SMTP-earned verdict on the same provider.
	seedVerdict(m, "v-smtp", "valid", "accepted", "google", false, fixedNow().Add(-time.Hour), nil)
	ing := newIngestor(m, nil)
	src := NewSimulatedSource("t")
	src.Add(RawOutcome{VerificationID: "v-ext", MessageID: "e1", Event: "550 5.1.1 user unknown", SentAt: fixedNow(), OccurredAt: fixedNow()})
	src.Add(RawOutcome{VerificationID: "v-smtp", MessageID: "s1", Event: "550 5.1.1 user unknown", SentAt: fixedNow(), OccurredAt: fixedNow()})
	if _, err := ing.IngestOnce(context.Background(), src, 10); err != nil {
		t.Fatal(err)
	}
	aggs, _ := m.BucketAggregatesSince(context.Background(), fixedNow().Add(-24*time.Hour), fixedNow())
	var extBucket, smtpBucket bool
	for _, a := range aggs {
		if a.Vendor == "millionverifier" && a.ResidueClass == "catch_all" {
			extBucket = true
		}
		if a.Vendor == "" && a.Reason == "accepted" {
			smtpBucket = true
		}
	}
	if !extBucket || !smtpBucket {
		t.Fatalf("external and SMTP outcomes did not bucket separately: %+v (GAP-54 — merging trains on the vendor's errors)", aggs)
	}
}

// TestIngest_FromCacheExcluded (§4.4): an outcome joined to a from_cache verdict is recorded but
// EXCLUDED (it wasn't a fresh check — its accuracy isn't this system's fresh-check accuracy).
func TestIngest_FromCacheExcluded(t *testing.T) {
	m := store.NewMem(fixedNow)
	seedVerdict(m, "v-c", "valid", "accepted", "google", true /* from_cache */, fixedNow().Add(-time.Hour), nil)
	ing := newIngestor(m, nil)
	src := NewSimulatedSource("t")
	src.Add(RawOutcome{VerificationID: "v-c", MessageID: "m1", Event: "550 5.1.1 user unknown", SentAt: fixedNow(), OccurredAt: fixedNow()})
	if _, err := ing.IngestOnce(context.Background(), src, 10); err != nil {
		t.Fatal(err)
	}
	if m.SendOutcomes[0].CalibrationClass != "excluded" {
		t.Fatalf("from_cache outcome class = %q, want excluded (§4.4)", m.SendOutcomes[0].CalibrationClass)
	}
	aggs, _ := m.BucketAggregatesSince(context.Background(), fixedNow().Add(-24*time.Hour), fixedNow())
	if len(aggs) != 0 {
		t.Fatalf("a from_cache outcome entered calibration: %+v (§4.4)", aggs)
	}
}

// TestIngest_UnauthenticatedIngestNothing is the ADR-0013 fail-closed test: an unauthenticated source
// ingests ZERO outcomes (nothing reaches the classifier). FAIL-VERIFY: remove the Authenticated() guard
// in IngestOnce → poison flows in → this breaks. This is the root defense TB-7 requires.
func TestIngest_UnauthenticatedIngestNothing(t *testing.T) {
	m := store.NewMem(fixedNow)
	seedVerdict(m, "v-x", "valid", "accepted", "google", false, fixedNow().Add(-time.Hour), nil)
	ing := newIngestor(m, nil)
	src := NewSimulatedSource("t")
	src.Authed = false // an UNAUTHENTICATED feed (e.g. mTLS handshake failed / no credential)
	src.Add(RawOutcome{VerificationID: "v-x", MessageID: "m1", Event: "550 5.1.1 user unknown", SentAt: fixedNow(), OccurredAt: fixedNow()})
	res, err := ing.IngestOnce(context.Background(), src, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SkippedAuth || res.Ingested != 0 || len(m.SendOutcomes) != 0 {
		t.Fatalf("an unauthenticated source ingested data (skippedAuth=%v ingested=%d rows=%d) — TB-7 fail-closed violated (ADR-0013)", res.SkippedAuth, res.Ingested, len(m.SendOutcomes))
	}
}

// TestPullSource_FailClosedWithoutClient — the DECIDED mechanism's adapter is fail-closed when no real
// mTLS client is wired (PENDING-REAL-SENDER): Authenticated()=false → nothing ingested.
func TestPullSource_FailClosedWithoutClient(t *testing.T) {
	ps := &PullSource{Key: "prod"}
	if ps.Authenticated() {
		t.Fatal("PullSource with nil Client reported authenticated — must be fail-closed (PENDING-REAL-SENDER, ADR-0013)")
	}
	m := store.NewMem(fixedNow)
	ing := newIngestor(m, nil)
	res, err := ing.IngestOnce(context.Background(), ps, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SkippedAuth {
		t.Fatal("ingestor pulled from a client-less PullSource — fail-closed violated")
	}
}
