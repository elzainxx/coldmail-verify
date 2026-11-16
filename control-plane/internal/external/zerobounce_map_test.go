package external

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// These tests need no stand-in: they pin the pure half of the ZeroBounce adapter — the normalization table,
// its relationship to Resolve (anti-laundering, OQ-P6-3, OQ-7), the doc that records it, and the pin on the
// stand-in's base images. They also hold the INDEPENDENT copy of the expectations (typed from
// Docs/vendor-zerobounce.md, not derived from zbRows) that the stand-in tests reuse; a test that read its
// expectations back out of the table it checks would pass whatever the table said.

// zbDocStatuses / zbDocSubs are the union of ZeroBounce's status and sub_status values across the official
// SDKs (Go, Rust, Ruby: all 7 and all 31; Python/JS/PHP/Java: the classic subset). "" is the sub_status the
// vendor sends when it has none.
var zbDocStatuses = []string{"valid", "invalid", "catch-all", "unknown", "spamtrap", "abuse", "do_not_mail"}

var zbDocSubs = []string{"",
	"antispam_system", "greylisted", "mail_server_temporary_error", "forcible_disconnect",
	"mail_server_did_not_respond", "timeout_exceeded", "failed_smtp_connection", "mailbox_quota_exceeded",
	"exception_occurred", "possible_trap", "role_based", "global_suppression", "mailbox_not_found",
	"no_dns_entries", "failed_syntax_check", "possible_typo", "unroutable_ip_address",
	"leading_period_removed", "does_not_accept_mail", "alias_address", "role_based_catch_all",
	"disposable", "toxic", "alternate", "mx_forward", "blocked", "allowed", "accept_all",
	"role_based_accept_all", "gold", "ai_agent_mailbox",
}

type zbWant struct {
	status, sub string
	result      vocab.Status
}

// zbWantRows is every ANSWERED pair, as the doc table lists them. Anything not here is a non-answer.
var zbWantRows = []zbWant{
	{"valid", "", vocab.StatusValid},
	{"valid", "leading_period_removed", vocab.StatusValid},
	{"valid", "gold", vocab.StatusValid},
	{"valid", "role_based_accept_all", vocab.StatusValid},
	{"valid", "ai_agent_mailbox", vocab.StatusValid},
	{"catch-all", "", vocab.StatusRisky},
	{"catch-all", "ai_agent_mailbox", vocab.StatusRisky},
	{"invalid", "mailbox_not_found", vocab.StatusInvalid},
	{"invalid", "no_dns_entries", vocab.StatusInvalid},
	{"invalid", "failed_syntax_check", vocab.StatusInvalid},
	{"invalid", "does_not_accept_mail", vocab.StatusInvalid},
	{"invalid", "mailbox_quota_exceeded", vocab.StatusRisky},
	{"invalid", "possible_typo", vocab.StatusRisky},
	{"invalid", "unroutable_ip_address", vocab.StatusRisky},
	{"spamtrap", "", vocab.StatusInvalid},
	{"abuse", "", vocab.StatusRisky},
	{"do_not_mail", "disposable", vocab.StatusRisky},
	{"do_not_mail", "role_based", vocab.StatusRisky},
	{"do_not_mail", "role_based_catch_all", vocab.StatusRisky},
	{"do_not_mail", "toxic", vocab.StatusRisky},
	{"do_not_mail", "global_suppression", vocab.StatusRisky},
	{"do_not_mail", "possible_trap", vocab.StatusRisky},
}

func zbWantMap() map[[2]string]vocab.Status {
	m := make(map[[2]string]vocab.Status, len(zbWantRows))
	for _, r := range zbWantRows {
		m[[2]string{r.status, r.sub}] = r.result
	}
	return m
}

// behaviour is the stand-in local-part for a pair: status, or status--sub_status.
func (w zbWant) behaviour() string {
	if w.sub == "" {
		return w.status
	}
	return w.status + "--" + w.sub
}

// The whole 7 x 32 product (plus pairs nobody has seen) against the independent expectation. The count at
// the end is what keeps the loop from passing vacuously: every expected row must have been REACHED.
func TestZB_MapTable_CrossProduct(t *testing.T) {
	want := zbWantMap()
	statuses := append(append([]string{}, zbDocStatuses...), "totally_new_status", "Valid", "VALID", "")
	subs := append(append([]string{}, zbDocSubs...), "some_future_sub_status", "Mailbox_Not_Found")
	reached := 0
	for _, st := range statuses {
		for _, sub := range subs {
			w, wantOK := want[[2]string{st, sub}]
			got, ok := MapZeroBounce(st, sub)
			if ok != wantOK || (ok && got != w) {
				t.Errorf("MapZeroBounce(%q, %q) = (%s, %v), want (%s, %v)", st, sub, got, ok, w, wantOK)
			}
			if ok {
				reached++
			}
		}
	}
	if reached != len(zbWantRows) || len(ZeroBounceRows()) != len(zbWantRows) {
		t.Fatalf("the product reached %d answered pairs and the table holds %d, want %d: a row was added or lost without the doc and this test", reached, len(ZeroBounceRows()), len(zbWantRows))
	}

	// Paired controls: the same sub_status means different things under different statuses, and the same
	// status with and without a documented sub_status differs. The mapping is on the PAIR, not on a word.
	if r, ok := MapZeroBounce("valid", "ai_agent_mailbox"); !ok || r != vocab.StatusValid {
		t.Errorf("control: valid/ai_agent_mailbox must answer valid, got (%s, %v)", r, ok)
	}
	if r, ok := MapZeroBounce("catch-all", "ai_agent_mailbox"); !ok || r != vocab.StatusRisky {
		t.Errorf("control: catch-all/ai_agent_mailbox must answer risky, got (%s, %v)", r, ok)
	}
	if _, ok := MapZeroBounce("unknown", "ai_agent_mailbox"); ok {
		t.Error("control: unknown/ai_agent_mailbox must be a non-answer")
	}
	if _, ok := MapZeroBounce("valid", ""); !ok {
		t.Error("control: valid/'' must answer")
	}
	if _, ok := MapZeroBounce("unknown", ""); ok {
		t.Error("`unknown` is the vendor's own non-answer and must never become a verdict, whatever its sub_status")
	}
}

// Every `unknown` pair is a non-answer — each documented sub_status of it, by name, so a future edit that
// maps one (say greylisted) fails here and not in an incident.
func TestZB_MapTable_UnknownIsNeverAnAnswer(t *testing.T) {
	for _, sub := range zbDocSubs {
		if _, ok := MapZeroBounce("unknown", sub); ok {
			t.Errorf("unknown/%q is mapped: a vendor `unknown` is not a verdict (M1's residue verdict stands)", sub)
		}
	}
	// Paired control: the same sub_status words under a status that DOES answer are answers.
	for _, sub := range []string{"", "ai_agent_mailbox"} {
		if _, ok := MapZeroBounce("valid", sub); !ok {
			t.Errorf("control: valid/%q should answer", sub)
		}
	}
}

// The whole table through the real Resolve: no row can produce status:valid (anti-laundering §3.4), every
// verdict is external_verified at <= the cap, and the vendor's "no" is trusted only where non-existence is
// plausible (OQ-P6-3). The expected hard-negative set is typed here, not read from the table.
func TestZB_MapTable_ThroughResolve_AntiLaundering_AndAsymmetry(t *testing.T) {
	const capConf = 60
	classes := []ResidueClass{ResidueCatchAll, ResidueProviderNoSMTP, ResidueRetryExhausted, ResidueGaveUpRateLimited}
	hard := map[[2]string]bool{
		{"invalid", "mailbox_not_found"}: true, {"invalid", "no_dns_entries"}: true,
		{"invalid", "failed_syntax_check"}: true, {"invalid", "does_not_accept_mail"}: true,
		{"spamtrap", ""}: true,
	}
	hardSeen, softSeen := 0, 0
	for _, row := range ZeroBounceRows() {
		isHard := row.Result == vocab.StatusInvalid
		if isHard != hard[[2]string{row.Status, row.Sub}] {
			t.Errorf("%s/%q: Result=%s but the hard-negative set says hard=%v", row.Status, row.Sub, row.Result, hard[[2]string{row.Status, row.Sub}])
		}
		for _, class := range classes {
			res := Resolve(VendorResult{Result: row.Result}, class, capConf)
			if res.KeepResidue {
				t.Errorf("%s/%q on %s: an answered row kept the residue", row.Status, row.Sub, class)
			}
			if res.Status == vocab.StatusValid {
				t.Errorf("ANTI-LAUNDERING BROKEN: %s/%q on %s resolved to status:valid", row.Status, row.Sub, class)
			}
			if res.Reason != vocab.ReasonExternalVerified {
				t.Errorf("%s/%q on %s: reason %s, want external_verified", row.Status, row.Sub, class, res.Reason)
			}
			if res.Confidence != capConf {
				t.Errorf("%s/%q on %s: confidence %d, want the cap %d (ZeroBounce reports no confidence of its own)", row.Status, row.Sub, class, res.Confidence, capConf)
			}
			wantInvalid := isHard && (class == ResidueRetryExhausted || class == ResidueGaveUpRateLimited)
			if (res.Status == vocab.StatusInvalid) != wantInvalid {
				t.Errorf("%s/%q on %s: status %s, want invalid=%v (OQ-P6-3: a vendor 'no' is accepted only where non-existence is plausible)", row.Status, row.Sub, class, res.Status, wantInvalid)
			}
		}
		if isHard {
			hardSeen++
		} else {
			softSeen++
		}
	}
	if hardSeen != len(hard) || softSeen == 0 {
		t.Fatalf("hard=%d (want %d) soft=%d: the loop did not reach both kinds of row", hardSeen, len(hard), softSeen)
	}
}

// PROJECT_RULES §2 / OQ-7: a do_not_mail address (disposable, role, toxic, suppressed, possible trap) is a
// deliverable-but-unwanted address, so it is risky and NEVER invalid — in every residue class. The control is
// spamtrap, which M1's own policy already treats as invalid.
func TestZB_MapTable_DoNotMailIsNeverInvalid_OQ7(t *testing.T) {
	seen := 0
	for _, row := range ZeroBounceRows() {
		if row.Status != "do_not_mail" {
			continue
		}
		seen++
		if row.Result == vocab.StatusInvalid {
			t.Errorf("do_not_mail/%s maps to invalid: a 'don't want' is never a 'won't deliver' (OQ-7)", row.Sub)
		}
		for _, class := range []ResidueClass{ResidueRetryExhausted, ResidueGaveUpRateLimited} {
			if res := Resolve(VendorResult{Result: row.Result}, class, 60); res.Status == vocab.StatusInvalid {
				t.Errorf("do_not_mail/%s resolved invalid on %s", row.Sub, class)
			}
		}
	}
	if seen != 6 {
		t.Fatalf("saw %d do_not_mail rows, want 6 (disposable, role_based, role_based_catch_all, toxic, global_suppression, possible_trap)", seen)
	}
	if res := Resolve(VendorResult{Result: vocab.StatusInvalid}, ResidueRetryExhausted, 60); res.Status != vocab.StatusInvalid {
		t.Error("control: a hard negative (spamtrap) must still resolve invalid on retry_exhausted")
	}
}

// The doc is the owner's record of the mapping; the table must not drift from it. Every answered pair must
// have its row, every documented sub_status must be named, and the owner-facing NOTE and sections must exist.
func TestZB_MapTable_DocInSync(t *testing.T) {
	const docPath = "../../../Docs/vendor-zerobounce.md"
	b, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	doc := string(b)
	for _, row := range ZeroBounceRows() {
		sub := row.Sub
		if sub == "" {
			sub = "(empty)"
		}
		needle := "| `" + row.Status + "` | `" + sub + "` | "
		if !strings.Contains(doc, needle) {
			t.Errorf("Docs/vendor-zerobounce.md has no table row starting %q for the answered pair %s/%q", needle, row.Status, row.Sub)
		}
	}
	for _, sub := range zbDocSubs {
		if sub != "" && !strings.Contains(doc, "`"+sub+"`") {
			t.Errorf("Docs/vendor-zerobounce.md never names the documented sub_status %q", sub)
		}
	}
	for _, st := range zbDocStatuses {
		if !strings.Contains(doc, "`"+st+"`") {
			t.Errorf("Docs/vendor-zerobounce.md never names the documented status %q", st)
		}
	}
	for _, must := range []string{
		"Cloudflare 403", "must be checked against the live documentation", "UNVERIFIED", "Not honestly mappable",
		"policy.vendor is informational",
	} {
		if !strings.Contains(doc, must) {
			t.Errorf("Docs/vendor-zerobounce.md lacks %q", must)
		}
	}
	// Paired control: the matcher really can fail — a pair that is NOT answered has no such row.
	if strings.Contains(doc, "| `unknown` | `greylisted` | valid |") {
		t.Error("control: the doc must not list unknown/greylisted as an answer")
	}
}

var fromLine = regexp.MustCompile(`(?im)^FROM\s+(?:--\S+\s+)*(\S+)`)
var pinnedRef = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// fromRefs returns every FROM reference in a Dockerfile and those not pinned by a full digest.
func fromRefs(dockerfile string) (all, unpinned []string) {
	for _, m := range fromLine.FindAllStringSubmatch(dockerfile, -1) {
		ref := m[1]
		all = append(all, ref)
		if !pinnedRef.MatchString(ref) {
			unpinned = append(unpinned, ref)
		}
	}
	return all, unpinned
}

// The stand-in image's bases are pinned by digest (the repo rule for every image; the base-image-pinned gate
// reads only the verifier node's Dockerfile and the workflow's `image:` lines, so this is the pin for ours).
func TestZB_StandInDockerfile_BasesPinnedByDigest(t *testing.T) {
	b, err := os.ReadFile("../../test/zbstub/Dockerfile")
	if err != nil {
		t.Fatalf("read the stand-in Dockerfile: %v", err)
	}
	all, unpinned := fromRefs(string(b))
	if len(all) != 2 {
		t.Fatalf("the stand-in Dockerfile has %d FROM lines (%v), want 2 (builder + runtime)", len(all), all)
	}
	if len(unpinned) != 0 {
		t.Errorf("FROM without a full @sha256 digest: %v (a tag moves; a digest is a content address)", unpinned)
	}
	// Paired controls: the checker rejects a tag, a truncated digest, and accepts a full one.
	for _, bad := range []string{"FROM alpine:3.24\n", "FROM alpine@sha256:abc\n", "FROM --platform=linux/amd64 golang:1.26-alpine AS b\n"} {
		if _, u := fromRefs(bad); len(u) != 1 {
			t.Errorf("control: %q should be reported unpinned, got %v", bad, u)
		}
	}
	if _, u := fromRefs("FROM alpine:3.24@sha256:" + strings.Repeat("a", 64) + "\n"); len(u) != 0 {
		t.Errorf("control: a full digest must be accepted, got %v", u)
	}
}
