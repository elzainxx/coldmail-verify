package external

import "github.com/coldmail-verify/control-plane/internal/vocab"

// This file: the ZeroBounce (status, sub_status) -> M1 normalization table (§8.2: the vendor's own
// strings are normalized at THIS boundary and never reach status/reason). The table is the code half
// of Docs/vendor-zerobounce.md; TestZB_MapTable_DocInSync keeps the two in step.
//
// SOURCE AND ITS LIMITS. ZeroBounce's documentation pages were not reachable for our tooling
// (Cloudflare 403), so the status and sub_status sets and the example (status, sub_status) pairs come
// from ZeroBounce's own official SDK repositories (Go, Rust, Ruby, Python, JavaScript, PHP, Java and
// the Postman collection), read 2026-11-16; Docs/vendor-zerobounce.md records the repo, commit and what
// each source confirms. The owner must check this table against the live documentation before the
// vendor is switched on.
//
// THE RULE THE TABLE ENFORCES: the table is a WHITELIST of (status, sub_status) pairs that an official
// SDK shows ZeroBounce actually answering with. Any pair that is not listed (an `unknown` status, a
// sub_status with no documented pairing, a status or sub_status this build has never seen) is a
// NON-ANSWER: the call is recorded as an error, no verdict is formed, and M1's own residue verdict
// stands. Absence from the table is never read as a verdict.
//
// What a row's Result means (vocab.Status the adapter hands to Resolve, which then applies the
// anti-laundering and OQ-P6-3 rules; the adapter itself never produces a final verdict):
//
//	StatusValid   the vendor says the mailbox is good. Resolve turns this into risky/external_verified,
//	              capped — a rented "yes" is never SMTP-grade valid (§3.4).
//	StatusInvalid a hard negative: positive evidence the mailbox cannot receive (or a spam trap, which
//	              M1's own policy already treats as invalid:spam_trap). Resolve accepts it as invalid
//	              only on retry_exhausted / gave_up_rate_limited and downgrades it to risky on
//	              catch_all / provider_no_smtp (OQ-P6-3).
//	StatusRisky   deliverable-but-unwanted, or ambiguous between "unwanted" and "does not exist". The
//	              cautious side. PROJECT_RULES §2 / OQ-7: "won't deliver" and "don't want" are never
//	              conflated, so a disposable, role, toxic or suppressed address is never invalid.
//
// ZeroBounce `status` strings (the union of the SDKs; every SDK agrees on these seven).
const (
	zbStatusValid     = "valid"
	zbStatusInvalid   = "invalid"
	zbStatusCatchAll  = "catch-all"
	zbStatusUnknown   = "unknown"
	zbStatusSpamtrap  = "spamtrap"
	zbStatusAbuse     = "abuse"
	zbStatusDoNotMail = "do_not_mail"
)

// ZBRow is one answered (status, sub_status) pair and what the adapter normalizes it to.
type ZBRow struct {
	Status string       // ZeroBounce `status`, verbatim
	Sub    string       // ZeroBounce `sub_status`, verbatim ("" when the vendor sends none)
	Result vocab.Status // what the adapter hands to Resolve (valid | invalid | risky)
}

// zbRows is the ONLY place a pair becomes an answer. Keep Docs/vendor-zerobounce.md in step.
var zbRows = []ZBRow{
	// valid: a rented "yes" — risky/external_verified, capped, in Resolve.
	{zbStatusValid, "", vocab.StatusValid},
	{zbStatusValid, "leading_period_removed", vocab.StatusValid},
	{zbStatusValid, "gold", vocab.StatusValid},
	{zbStatusValid, "role_based_accept_all", vocab.StatusValid},
	{zbStatusValid, "ai_agent_mailbox", vocab.StatusValid},

	// catch-all: the domain accepts everything; risky.
	{zbStatusCatchAll, "", vocab.StatusRisky},
	{zbStatusCatchAll, "ai_agent_mailbox", vocab.StatusRisky},

	// invalid, hard negatives: positive evidence the mailbox cannot receive.
	{zbStatusInvalid, "mailbox_not_found", vocab.StatusInvalid},
	{zbStatusInvalid, "no_dns_entries", vocab.StatusInvalid},
	{zbStatusInvalid, "failed_syntax_check", vocab.StatusInvalid},
	{zbStatusInvalid, "does_not_accept_mail", vocab.StatusInvalid},

	// invalid, ambiguous: ZeroBounce says invalid, but the sub_status is not proof the address does not
	// exist, so the cautious side (risky) is taken and the row is listed as "not honestly mappable".
	{zbStatusInvalid, "mailbox_quota_exceeded", vocab.StatusRisky}, // the mailbox EXISTS and is full (M1: 552 mailbox_full is valid)
	{zbStatusInvalid, "possible_typo", vocab.StatusRisky},          // "possible": a suggestion, not proof of absence
	{zbStatusInvalid, "unroutable_ip_address", vocab.StatusRisky},  // ADR-0021 treats a non-public MX as risky, never invalid

	// spamtrap: a hard negative. M1's own policy maps a spam-trap hit to invalid:spam_trap.
	{zbStatusSpamtrap, "", vocab.StatusInvalid},

	// abuse: meaning is not stated in any SDK; deliverable-but-unwanted is the cautious reading.
	{zbStatusAbuse, "", vocab.StatusRisky},

	// do_not_mail: PROJECT_RULES §2 / OQ-7 win. Deliverable-but-unwanted is risky, never invalid.
	{zbStatusDoNotMail, "disposable", vocab.StatusRisky},
	{zbStatusDoNotMail, "role_based", vocab.StatusRisky},
	{zbStatusDoNotMail, "role_based_catch_all", vocab.StatusRisky},
	{zbStatusDoNotMail, "toxic", vocab.StatusRisky},
	{zbStatusDoNotMail, "global_suppression", vocab.StatusRisky}, // ambiguous (suppressed because it bounced, or because it complained?)
	{zbStatusDoNotMail, "possible_trap", vocab.StatusRisky},      // "possible": not proof
}

type zbKey struct{ status, sub string }

var zbIndex = func() map[zbKey]vocab.Status {
	m := make(map[zbKey]vocab.Status, len(zbRows))
	for _, r := range zbRows {
		m[zbKey{r.Status, r.Sub}] = r.Result
	}
	return m
}()

// MapZeroBounce is the pure normalization function: ok=false means the pair is a NON-ANSWER (no
// verdict may be formed from it). Matching is exact and case-sensitive; ZeroBounce answers in lower
// case and a differently-cased string is something this build has not seen, so it is a non-answer.
func MapZeroBounce(status, subStatus string) (vocab.Status, bool) {
	r, ok := zbIndex[zbKey{status, subStatus}]
	return r, ok
}

// ZeroBounceRows returns a copy of every answered pair (for the doc-sync test and for tooling).
func ZeroBounceRows() []ZBRow {
	out := make([]ZBRow, len(zbRows))
	copy(out, zbRows)
	return out
}
