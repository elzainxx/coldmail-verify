package external

import (
	"fmt"
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

// zbDocStatuses / zbDocSubs are the union of ZeroBounce's status and sub_status values: the live status-codes
// page (read by the owner, 2026-11-17) plus the official SDKs (Go, Rust, Ruby: all 7 and all 31; Python/JS/PHP/Java:
// the classic subset). "" is the sub_status the vendor sends when it has none.
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

// zbLiveUnknownSubs are the eight sub-statuses the live page lists under `unknown` (owner, 2026-11-17). Every one is
// a non-answer: M1's own verdict stands, the call is an error with cost 0 and no vendor-charged marker. (ZeroBounce
// states that an unknown is not charged, which is exactly that rule.) Typed here, not read from the table.
var zbLiveUnknownSubs = []string{
	"antispam_system", "exception_occurred", "failed_smtp_connection", "forcible_disconnect", "greylisted",
	"mail_server_did_not_respond", "mail_server_temporary_error", "timeout_exceeded",
}

type zbWant struct {
	status, sub string
	result      vocab.Status
}

// zbWantRows is every ANSWERED pair, as the doc table lists them. Anything not here is a non-answer.
var zbWantRows = []zbWant{
	{"valid", "", vocab.StatusValid},
	{"valid", "leading_period_removed", vocab.StatusValid},
	{"valid", "alias_address", vocab.StatusValid},
	{"valid", "alternate", vocab.StatusValid},
	{"valid", "accept_all", vocab.StatusValid},
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
	{"do_not_mail", "mx_forward", vocab.StatusRisky},
}

// zbSDKOnlyPairs are the four answered pairs that are NOT on the live status-codes page (the owner ruled them
// SDK-only and kept them mapped). The doc labels exactly these "SDK-only" and every other row "page".
var zbSDKOnlyPairs = map[[2]string]bool{
	{"valid", "gold"}: true, {"valid", "role_based_accept_all"}: true, {"valid", "ai_agent_mailbox"}: true,
	{"catch-all", "ai_agent_mailbox"}: true,
}

// zbPageAdded are the four pairs the 2026-11-17 ruling added: valid gained alias_address, alternate and accept_all,
// do_not_mail gained mx_forward. Each is an answer ONLY with its own status.
var zbPageAdded = []zbWant{
	{"valid", "alias_address", vocab.StatusValid},
	{"valid", "alternate", vocab.StatusValid},
	{"valid", "accept_all", vocab.StatusValid},
	{"do_not_mail", "mx_forward", vocab.StatusRisky},
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

// The four pairs the live page added are answers ONLY with their own status: alias_address / alternate /
// accept_all under `valid`, mx_forward under `do_not_mail`. The same sub_status under any other status — and
// `blocked` / `allowed`, which are on neither the page nor in any pair, under every status — is still a
// non-answer. The mapping is on the PAIR, so each row has its paired control right beside it. Every added row
// also goes through the real Resolve: a vendor "yes" (even valid/accept_all) is risky/external_verified at the
// cap and never M1 `valid`.
func TestZB_MapTable_LivePageSubStatuses_AnswerOnlyWithTheirOwnStatus(t *testing.T) {
	others := append(append([]string{}, zbDocStatuses...), "totally_new_status", "Valid", "")
	checked, controls := 0, 0
	for _, a := range zbPageAdded {
		got, ok := MapZeroBounce(a.status, a.sub)
		if !ok || got != a.result {
			t.Errorf("MapZeroBounce(%q, %q) = (%s, %v), want (%s, true)", a.status, a.sub, got, ok, a.result)
			continue
		}
		checked++
		for _, class := range []ResidueClass{ResidueCatchAll, ResidueProviderNoSMTP, ResidueRetryExhausted, ResidueGaveUpRateLimited} {
			res := Resolve(VendorResult{Result: got}, class, 60)
			if res.KeepResidue || res.Status != vocab.StatusRisky || res.Reason != vocab.ReasonExternalVerified || res.Confidence != 60 {
				t.Errorf("%s/%s on %s resolved %+v, want risky/external_verified at the cap 60 (a vendor yes is never M1 valid)", a.status, a.sub, class, res)
			}
		}
		for _, st := range others {
			if st == a.status {
				continue
			}
			controls++
			if r, ok := MapZeroBounce(st, a.sub); ok {
				t.Errorf("control: %s/%s answered %s; %q is an answer only under %q", st, a.sub, r, a.sub, a.status)
			}
		}
	}
	for _, sub := range []string{"blocked", "allowed"} {
		for _, st := range others {
			controls++
			if r, ok := MapZeroBounce(st, sub); ok {
				t.Errorf("%s/%s answered %s: `blocked` and `allowed` are not on the live page and stay non-answers", st, sub, r)
			}
		}
	}
	if checked != 4 || controls < 4*len(zbDocStatuses) {
		t.Fatalf("checked %d added rows (want 4) and %d paired controls: the loop did not reach what it claims", checked, controls)
	}
}

// Every `unknown` sub-status the live page lists is a non-answer, each BY NAME, through the real interpreter (not
// only the table): the call is an error of kind no_answer carrying no result, no cost, no vendor status and no raw
// blob, so M1's own verdict stands and nothing the vendor does not charge for is recorded as charged. The control
// is the same body with status `valid`, which IS an answer costing one credit.
func TestZB_MapTable_LiveUnknownSubStatuses_AreEachANonAnswer(t *testing.T) {
	if len(zbLiveUnknownSubs) != 8 {
		t.Fatalf("the live page lists 8 unknown sub-statuses, the test names %d", len(zbLiveUnknownSubs))
	}
	known := map[string]bool{}
	for _, s := range zbDocSubs {
		known[s] = true
	}
	for _, sub := range zbLiveUnknownSubs {
		if !known[sub] {
			t.Errorf("%q is not in zbDocSubs, so the cross-product would never test it", sub)
		}
		if r, ok := MapZeroBounce("unknown", sub); ok {
			t.Errorf("unknown/%s is mapped (%s): the vendor's own non-answer is never a verdict", sub, r)
		}
		body := `{"address":"a@b.example","status":"unknown","sub_status":"` + sub + `"}`
		r, err := interpretZeroBounce([]byte(body), "idem", 5)
		if err == nil {
			t.Errorf("unknown/%s was read as an answer %+v", sub, r)
			continue
		}
		if k := ZBKind(err); k != ZBErrNoAnswer {
			t.Errorf("unknown/%s: kind %q, want %q", sub, k, ZBErrNoAnswer)
		}
		if r.Result != "" || r.CostUnits != 0 || r.StatusRaw != "" || len(r.RawJSON) != 0 || r.RequestID != "" {
			t.Errorf("unknown/%s carried a result or a vendor-charged marker: %+v", sub, r)
		}
		// Paired control: the same word under `valid` is no pair either (it is unrecognised, not no_answer), so the
		// table is keyed on the pair and the `unknown` rows above are non-answers because of the status too.
		ctl, err := interpretZeroBounce([]byte(strings.Replace(body, `"unknown"`, `"valid"`, 1)), "idem", 5)
		if err == nil || ZBKind(err) != ZBErrUnrecognised {
			t.Errorf("control: valid/%s = %+v / %v, want an unrecognised non-answer", sub, ctl, err)
		}
	}
	ctl, err := interpretZeroBounce([]byte(`{"address":"a@b.example","status":"valid","sub_status":""}`), "idem", 5)
	if err != nil || ctl.CostUnits != 1 {
		t.Errorf("control: a valid answer must cost one credit, got %+v / %v", ctl, err)
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
	if seen != 7 {
		t.Fatalf("saw %d do_not_mail rows, want 7 (disposable, role_based, role_based_catch_all, toxic, global_suppression, possible_trap, mx_forward)", seen)
	}
	if res := Resolve(VendorResult{Result: vocab.StatusInvalid}, ResidueRetryExhausted, 60); res.Status != vocab.StatusInvalid {
		t.Error("control: a hard negative (spamtrap) must still resolve invalid on retry_exhausted")
	}
}

// zbDocRowRE matches a row of the doc's mapping table (section 4): status, sub_status, adapter result. The
// non-answers table has other words in its third cell, so it never matches.
var zbDocRowRE = regexp.MustCompile(`(?m)^\| \x60(valid|invalid|catch-all|spamtrap|abuse|do_not_mail)\x60 \| \x60[^\x60]*\x60 \| (valid|invalid|risky) \| `)

// zbDocRow returns the doc's table line for an answered pair, requiring the adapter result in the third cell
// (so a doc that says "risky" where the code says "valid" is not found).
func zbDocRow(doc, status, sub string, result vocab.Status) (string, bool) {
	if sub == "" {
		sub = "(empty)"
	}
	needle := "| `" + status + "` | `" + sub + "` | " + string(result) + " | "
	i := strings.Index(doc, needle)
	if i < 0 {
		return "", false
	}
	line := doc[i:]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	return line, true
}

// zbDocProblems compares the doc with the table in BOTH directions and returns one message per difference:
// every answered pair needs its row with the right adapter result and the right source label ("page", or
// "SDK-only pair" for exactly the four pairs the owner ruled SDK-only), and the doc may not carry a mapping row
// the table does not have.
func zbDocProblems(doc string, rows []ZBRow) []string {
	var out []string
	for _, row := range rows {
		line, ok := zbDocRow(doc, row.Status, row.Sub, row.Result)
		if !ok {
			out = append(out, fmt.Sprintf("no doc row for %s/%q with adapter result %s", row.Status, row.Sub, row.Result))
			continue
		}
		sdkOnly := zbSDKOnlyPairs[[2]string{row.Status, row.Sub}]
		if has := strings.Contains(line, "**SDK-only pair**"); has != sdkOnly {
			out = append(out, fmt.Sprintf("%s/%q: the doc row's SDK-only label is %v, want %v", row.Status, row.Sub, has, sdkOnly))
		}
		if has := strings.Contains(line, "**page**"); has == sdkOnly {
			out = append(out, fmt.Sprintf("%s/%q: the doc row's page label is %v, want %v", row.Status, row.Sub, has, !sdkOnly))
		}
	}
	if n := len(zbDocRowRE.FindAllString(doc, -1)); n != len(rows) {
		out = append(out, fmt.Sprintf("the doc's mapping table has %d rows, the code's table has %d", n, len(rows)))
	}
	return out
}

// The doc is the owner's record of the mapping; the table must not drift from it. Every answered pair must
// have its row (adapter result and source label included), the doc may hold no row the table lacks, every
// documented sub_status must be named, and the owner-facing NOTE and sections must exist. The paired controls
// feed the SAME checker doctored copies of the real doc and require each to be reported.
func TestZB_MapTable_DocInSync(t *testing.T) {
	const docPath = "../../../Docs/vendor-zerobounce.md"
	b, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	doc := string(b)
	rows := ZeroBounceRows()
	for _, p := range zbDocProblems(doc, rows) {
		t.Errorf("Docs/vendor-zerobounce.md is out of step with zbRows: %s", p)
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
		"https://www.zerobounce.net/docs/email-list-validation/status-codes", "2026-11-17", "Cloudflare 403",
		"must be checked against the live documentation", "UNVERIFIED", "Not honestly mappable",
		"policy.vendor is informational", "SDK-only pair",
	} {
		if !strings.Contains(doc, must) {
			t.Errorf("Docs/vendor-zerobounce.md lacks %q", must)
		}
	}
	// The unknown-charge question moved out of UNVERIFIED: the doc must say the page states it, and the
	// UNVERIFIED table must no longer carry it as an open item.
	if !strings.Contains(doc, "does not charge for `unknown`") {
		t.Error("Docs/vendor-zerobounce.md does not record that the live page states an `unknown` is not charged")
	}
	if i := strings.Index(doc, "## 6. UNVERIFIED"); i < 0 {
		t.Error("Docs/vendor-zerobounce.md lost its UNVERIFIED section")
	} else if j := strings.Index(doc[i:], "\n## 7."); j < 0 || strings.Contains(doc[i:i+j], "Whether credits are charged for `unknown`") {
		t.Error("the UNVERIFIED section still lists the unknown-charge question as open")
	}
	// Paired control: the matcher really can fail — a pair that is NOT answered has no such row.
	if strings.Contains(doc, "| `unknown` | `greylisted` | valid |") {
		t.Error("control: the doc must not list unknown/greylisted as an answer")
	}

	// Paired controls on the checker itself: each doctored copy of the real doc must be reported, by its reason.
	line, ok := zbDocRow(doc, "valid", "accept_all", vocab.StatusValid)
	if !ok {
		t.Fatal("control setup: the doc has no valid/accept_all row")
	}
	gold, ok := zbDocRow(doc, "valid", "gold", vocab.StatusValid)
	if !ok || !strings.Contains(gold, "**SDK-only pair**") {
		t.Fatalf("control setup: valid/gold must be labelled SDK-only in the doc, got %q", gold)
	}
	mutants := []struct{ name, doc, reason string }{
		{"a row's adapter result changed", strings.Replace(doc, line, strings.Replace(line, "| valid | ", "| risky | ", 1), 1), "no doc row for valid/\"accept_all\""},
		{"a row deleted", strings.Replace(doc, line+"\n", "", 1), "no doc row for valid/\"accept_all\""},
		{"an SDK-only row labelled page", strings.Replace(doc, gold, strings.Replace(gold, "**SDK-only pair**", "**page**", 1), 1), "valid/\"gold\": the doc row's SDK-only label is false"},
		{"a page row labelled SDK-only", strings.Replace(doc, line, strings.Replace(line, "**page**", "**SDK-only pair**", 1), 1), "valid/\"accept_all\": the doc row's SDK-only label is true"},
		{"a row the table lacks", doc + "\n| `valid` | `blocked` | valid | risky | risky | external_verified | cap | x | **page** |\n", "the doc's mapping table has"},
	}
	for _, m := range mutants {
		if m.doc == doc {
			t.Errorf("control %q: the mutation did not change the doc, so it proves nothing", m.name)
			continue
		}
		found := false
		for _, p := range zbDocProblems(m.doc, rows) {
			if strings.Contains(p, m.reason) {
				found = true
			}
		}
		if !found {
			t.Errorf("control %q: the checker did not report %q", m.name, m.reason)
		}
	}
	if p := zbDocProblems(doc, rows); len(p) != 0 {
		t.Errorf("control: the undoctored doc must be clean, got %v", p)
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
