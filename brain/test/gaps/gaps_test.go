// Package gaps holds the behavioral/integration harness stubs, one per NAMED GAP the
// specs call load-bearing. They are REAL-SHAPED but EMPTY: each test enumerates the
// scenarios it will assert and t.Skip()s with an explicit PENDING reason naming the phase
// that fills it. This makes the scenario lists un-driftable and gives each GAP a home,
// while never faking green. See Docs/test-strategy.md §2.
//
// Run with `go test ./...` — these appear as SKIP (not PASS), so CI honestly shows the
// behavioral layer is scaffolded-not-proven.
package gaps

import "testing"

// P5 GAP-37 — the sharpest catch: idempotency / exactly-once settlement under a SLOW
// (not dead) lease-holder + a TTL reclaim. SKIP_LOCKED alone is NOT enough.
func TestGAP37_ExactlyOnceSettlementUnderReclaim(t *testing.T) {
	scenarios := []string{
		"two workers, same ready row: SKIP LOCKED gives it to exactly one",
		"slow (not dead) holder + TTL reclaim: second attempt runs, but only the CAS-on-lease-holder commits",
		"lost attempt (reclaimed) discards its result: no settle, no requeue, no LOG row",
		"crash after SMTP-success, before settle: reclaim re-attempts; settles exactly once (GAP-33)",
		"lease TTL must exceed the brain's max SMTP client deadline (a live attempt never loses its lease)",
	}
	t.Logf("GAP-37 scenarios to assert (%d): %v", len(scenarios), scenarios)
	t.Skip("PENDING P5: needs the work_items store + drainer + CAS. Harness is real-shaped; assertions land in Phase 5.")
}

// P7 GAP-49 — the bounce-classifier exclusion discipline (the P7 correctness heart).
func TestGAP49_BounceClassifierExclusions(t *testing.T) {
	// Mirrors the P7 Appendix B truth table.
	type tc struct{ event, wantClass string }
	table := []tc{
		{"mailbox_not_found (550 user unknown)", "invalid-signal"},
		{"mailbox_full (552 over quota)", "valid-signal"}, // the counterintuitive one — NOT invalid
		{"blocked / IP-blocklisted", "zero-information"},  // about the sender, not the address
		{"policy / spam reject", "zero-information"},
		{"greylisted / transient 4xx", "zero-information"},
		{"delivered on catch-all domain", "near-zero-information"},
		{"complaint (spam report)", "zero-information (for validity)"},
	}
	for _, c := range table {
		t.Logf("classify %-40q -> %s", c.event, c.wantClass)
	}
	t.Skip("PENDING P7: needs the classifier. Real-shaped truth table; a zero-info bounce must NEVER create a historical-bounce record.")
}

// P3 GAP-24 — the multi-homed audit cross-check: a green audit on the wrong (private) IP
// while the public egress IP is unaudited must be CAUGHT.
func TestGAP24_MultiHomedAuditCrossCheck(t *testing.T) {
	invariant := "host_audit.current_host_ip == verifier A-record == PTR target == SMTP egress IP (all = PUBLIC egress IP)"
	t.Logf("GAP-24 invariant: %s", invariant)
	t.Logf("failure to catch: audit green on the private IP; egress IP unaudited/unclean; verifier_ip lies")
	t.Skip("PENDING P3: needs dns_mock PTR/A fixtures + a two-IP fixture. Harness real-shaped; assertion lands in Phase 3.")
}

// P5 GAP-35 — greylist round-trip. Loop MECHANICS via a by-attempt stub; the true
// retry-then-accept behavior is PROVABLE-ONLY-AT-ROLLOUT (smtp_mock has no stateful greylist).
func TestGAP35_GreylistLoopMechanics(t *testing.T) {
	t.Logf("by-attempt stub: return 450 on attempt<N, 250 on attempt==N")
	t.Logf("assert MECHANICS: backoff schedule, requeue at not_before, ceiling->settle unknown/greylisted")
	t.Logf("PROVABLE-ONLY-AT-ROLLOUT: the real reject-then-accept round-trip (never faked on a mock)")
	t.Skip("PENDING P5: needs the retry scheduler. Mechanics harness real-shaped; behavioral truth deferred to a real greylisting provider.")
}

// P2 GAP-11/14 — the socket-level mock substrate + the accept-all `valid`-path caveat.
func TestGAP14_SmtpMockValidPathNeedsSeededProbe(t *testing.T) {
	t.Logf("smtp_mock 250-accepts all except pre-listed -> it is catch-all-shaped")
	t.Logf("the ONLY valid path (target-250 + probe-REJECTED) needs a SEEDED probe pre-listed for rejection")
	t.Logf("a RANDOM probe against the mock is always 250'd -> valid unreachable (a test artifact, not realism)")
	t.Skip("PENDING P2: needs the Dockerized smtp_mock/dns_mock substrate. Harness real-shaped; seeded-probe valid path asserted in Phase 2.")
}
