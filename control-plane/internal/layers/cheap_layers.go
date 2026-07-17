package layers

import (
	"context"
	"sort"
	"strings"
)

// This file: the layers that need no DNS — syntax, role, free/corp, typo (built-in seed
// lists, §6.2/6.3/6.4) and the list-backed disposable + spam-trap (§6.7/6.8, from the store).
// Each ONLY emits a LayerOutput; the policy decides gate/flag/stop.

// ── 6.1 Syntax / regex (Type-1) ──────────────────────────────────────────────────────
// The normalization already happened (Door 3, hash.Normalize) before Evidence was built —
// an unparseable address never reaches a layer (the orchestrator logs it as invalid_syntax
// with a NULL hash). So this layer's job is the RESIDUAL structural sanity of the parts.

type SyntaxLayer struct{}

func (SyntaxLayer) ID() string { return "syntax" }

func (SyntaxLayer) Run(_ context.Context, ev *Evidence) LayerOutput {
	ok := ev.LocalPart != "" && ev.Domain != "" &&
		len(ev.LocalPart) <= 64 && len(ev.Domain) <= 255 && // RFC 5321 length limits
		strings.Contains(ev.Domain, ".") // a mailable domain has at least one dot
	res := Pass
	sev := Info
	if !ok {
		res = Fail
		sev = Critical
	}
	return LayerOutput{LayerID: "syntax", SignalKey: "syntax_valid", Result: res, SeverityHint: sev, Value: ok}
}

// ── 6.3 Role account (Type-2 flag) ─────────────────────────────────────────────────────

// roleLocalParts is the built-in role-account list (§6.3, DIAL — a list, extensible later).
var roleLocalParts = map[string]struct{}{
	"admin": {}, "administrator": {}, "info": {}, "sales": {}, "support": {}, "contact": {},
	"help": {}, "billing": {}, "office": {}, "hello": {}, "mail": {}, "team": {}, "hr": {},
	"jobs": {}, "careers": {}, "marketing": {}, "noreply": {}, "no-reply": {}, "postmaster": {},
	"webmaster": {}, "abuse": {}, "security": {}, "root": {}, "sysadmin": {}, "hostmaster": {},
	"orders": {}, "enquiries": {}, "inquiries": {}, "feedback": {}, "service": {}, "accounts": {},
}

type RoleLayer struct{}

func (RoleLayer) ID() string { return "role" }

func (RoleLayer) Run(_ context.Context, ev *Evidence) LayerOutput {
	// Compare the local part with any +tag stripped (role@ vs role+x@ are both role).
	base := ev.LocalPart
	if i := strings.IndexByte(base, '+'); i >= 0 {
		base = base[:i]
	}
	_, isRole := roleLocalParts[base]
	res := Pass
	if isRole {
		res = Flag // a flag, never a gate by itself (policy may weight it)
	}
	return LayerOutput{LayerID: "role", SignalKey: "is_role", Result: res, SeverityHint: Info, Value: isRole}
}

// ── 6.4 Free-vs-corp (Type-2 flag) ─────────────────────────────────────────────────────

// freeProviderDomains: built-in free consumer mailbox providers (§6.4, DIAL).
var freeProviderDomains = map[string]struct{}{
	"gmail.com": {}, "googlemail.com": {}, "yahoo.com": {}, "yahoo.co.uk": {}, "ymail.com": {},
	"hotmail.com": {}, "outlook.com": {}, "live.com": {}, "msn.com": {}, "hotmail.co.uk": {},
	"aol.com": {}, "icloud.com": {}, "me.com": {}, "mac.com": {}, "proton.me": {},
	"protonmail.com": {}, "gmx.com": {}, "gmx.net": {}, "mail.com": {}, "zoho.com": {},
	"yandex.com": {}, "yandex.ru": {}, "fastmail.com": {}, "tutanota.com": {},
}

type FreeCorpLayer struct{}

func (FreeCorpLayer) ID() string { return "free_corp" }

func (FreeCorpLayer) Run(_ context.Context, ev *Evidence) LayerOutput {
	_, isFree := freeProviderDomains[ev.Domain]
	res := Pass
	if isFree {
		res = Flag
	}
	return LayerOutput{LayerID: "free_corp", SignalKey: "is_free_provider", Result: res, SeverityHint: Info, Value: isFree}
}

// ── 6.2 Typo / suggestion (Type-2 flag) ────────────────────────────────────────────────

// typoKnownGood: built-in seed of common domains to suggest corrections toward (§6.2, OQ-18
// — built-in seed now, Supabase-backed later). Kept small and high-value (the domains users
// most often fat-finger); a full list is a later DIAL.
var typoKnownGood = []string{
	"gmail.com", "googlemail.com", "yahoo.com", "hotmail.com", "outlook.com", "live.com",
	"aol.com", "icloud.com", "protonmail.com", "proton.me", "msn.com", "comcast.net",
}

type TypoLayer struct{}

func (TypoLayer) ID() string { return "typo" }

func (TypoLayer) Run(_ context.Context, ev *Evidence) LayerOutput {
	// Never suggest against a domain that is itself a known-good target.
	for _, good := range typoKnownGood {
		if ev.Domain == good {
			return LayerOutput{LayerID: "typo", SignalKey: "has_typo", Result: Pass, SeverityHint: Info, Value: false}
		}
	}
	best, dist := "", 3 // only suggest within edit distance <= 2 (avoids noisy far matches)
	for _, good := range typoKnownGood {
		if d := boundedLevenshtein(ev.Domain, good, dist); d < dist {
			best, dist = good, d
		}
	}
	if best == "" {
		return LayerOutput{LayerID: "typo", SignalKey: "has_typo", Result: Pass, SeverityHint: Info, Value: false}
	}
	suggestion := ev.LocalPart + "@" + best
	return LayerOutput{
		LayerID: "typo", SignalKey: "has_typo", Result: Flag, SeverityHint: Warn,
		Value: true, Detail: suggestion,
	}
}

// ── 6.7 Disposable / DEA (Type-2 flag → risky; OQ-7 — NEVER a gate to invalid) ──────────

type DisposableLayer struct{}

func (DisposableLayer) ID() string { return "disposable" }

func (DisposableLayer) Run(ctx context.Context, ev *Evidence) LayerOutput {
	if ev.lists == nil {
		// No list wired → we cannot assert disposability; emit unknown, never a false flag.
		return LayerOutput{LayerID: "disposable", SignalKey: "is_disposable", Result: Unknown, SeverityHint: Info, Value: false}
	}
	is, err := ev.lists.IsDisposable(ctx, ev.Domain)
	if err != nil {
		return LayerOutput{LayerID: "disposable", SignalKey: "is_disposable", Result: Unknown, SeverityHint: Warn, Value: false, Detail: err.Error()}
	}
	res := Pass
	if is {
		res = Flag // OQ-7: a flag → risky; the policy SKIPS it, never labels it invalid.
	}
	return LayerOutput{LayerID: "disposable", SignalKey: "is_disposable", Result: res, SeverityHint: Info, Value: is}
}

// ── 6.8 Spam-trap / honeypot (Type-1 gate) ─────────────────────────────────────────────

type SpamTrapLayer struct{}

func (SpamTrapLayer) ID() string { return "spam_trap" }

func (SpamTrapLayer) Run(ctx context.Context, ev *Evidence) LayerOutput {
	if ev.lists == nil {
		return LayerOutput{LayerID: "spam_trap", SignalKey: "is_spam_trap", Result: Unknown, SeverityHint: Info, Value: false}
	}
	is, err := ev.lists.IsSpamTrap(ctx, ev.Domain)
	if err != nil {
		return LayerOutput{LayerID: "spam_trap", SignalKey: "is_spam_trap", Result: Unknown, SeverityHint: Warn, Value: false, Detail: err.Error()}
	}
	// The policy decides that spam_trap:pass gates hard; the layer only reports the finding.
	res := Pass // "pass" here means "the trap test fired" per the policy grammar (spam_trap:pass → invalid)
	if !is {
		res = Fail // "fail" = not a trap (nothing to gate on)
	}
	return LayerOutput{LayerID: "spam_trap", SignalKey: "is_spam_trap", Result: res, SeverityHint: Critical, Value: is}
}

// boundedLevenshtein returns the edit distance between a and b, capped at max (returns max
// as soon as it is certain the distance is >= max — cheap for the typo layer's small list).
func boundedLevenshtein(a, b string, max int) int {
	la, lb := len(a), len(b)
	if abs(la-lb) >= max {
		return max
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		rowMin := curr[0]
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
			if curr[j] < rowMin {
				rowMin = curr[j]
			}
		}
		if rowMin >= max {
			return max
		}
		prev, curr = curr, prev
	}
	if prev[lb] > max {
		return max
	}
	return prev[lb]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// knownGoodSorted is a stable, sorted copy of the typo seed (used only by a test to assert
// determinism of suggestions; kept here so the seed stays the single source).
func knownGoodSorted() []string {
	out := append([]string(nil), typoKnownGood...)
	sort.Strings(out)
	return out
}
