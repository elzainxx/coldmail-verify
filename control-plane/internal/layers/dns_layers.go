package layers

import (
	"context"
	"strings"
)

// This file: the DNS-backed layers (§6.5 MX/DNS, §6.6 null-MX, §6.9 provider classification).
// All three read the shared, memoized Evidence.mx() so DNS is hit at most once per request.
// The MX layer is the harden-toward-unknown centerpiece: only DEFINITIVE non-existence is a
// fail; every transient failure is `unknown`, never `invalid`.

// ── 6.5 MX / DNS (Type-1 core gate) ────────────────────────────────────────────────────

type MXLayer struct{}

func (MXLayer) ID() string { return "mx" }

func (MXLayer) Run(ctx context.Context, ev *Evidence) LayerOutput {
	_, outcome := ev.mx(ctx)
	switch outcome {
	case mxFound:
		return LayerOutput{LayerID: "mx", SignalKey: "mx_found", Result: Pass, SeverityHint: Info, Value: true}
	case mxNone:
		// Positive proof: the domain exists in DNS but has no mail route → policy gates invalid/no_mx.
		return LayerOutput{LayerID: "mx", SignalKey: "mx_found", Result: Fail, SeverityHint: Critical, Value: false, Detail: "no MX and no A/AAAA fallback"}
	case mxNullMX:
		// Reported by the null_mx layer as the dominant reason; here mx just reports "no usable route".
		return LayerOutput{LayerID: "mx", SignalKey: "mx_found", Result: Fail, SeverityHint: Critical, Value: false, Detail: "null MX (RFC 7505)"}
	default: // mxError — transient/unresolvable → UNKNOWN, never invalid (the whole discipline)
		return LayerOutput{LayerID: "mx", SignalKey: "mx_found", Result: Unknown, SeverityHint: Warn, Value: false, Detail: "DNS lookup failed (transient) — not proof of non-existence"}
	}
}

// ── 6.6 Null-MX RFC 7505 (Type-1 core gate) ────────────────────────────────────────────

type NullMXLayer struct{}

func (NullMXLayer) ID() string { return "null_mx" }

func (NullMXLayer) Run(ctx context.Context, ev *Evidence) LayerOutput {
	_, outcome := ev.mx(ctx)
	isNull := outcome == mxNullMX
	// Grammar: null_mx:pass → invalid:null_mx:stop (P1 §7.2). "pass" = the null-MX test fired.
	res := Fail // "fail" = not a null-MX (nothing to gate)
	if isNull {
		res = Pass
	}
	return LayerOutput{LayerID: "null_mx", SignalKey: "null_mx", Result: res, SeverityHint: Critical, Value: isNull}
}

// ── 6.9 Provider classification (Type-2, drives routing) ───────────────────────────────

// providerMXSuffixes maps an MX-host suffix to a provider class (§6.9: google|o365|yahoo|
// self|other). Order matters only for readability; matching is by suffix. DIAL (a list).
var providerMXSuffixes = []struct {
	suffix   string
	provider string
}{
	{"google.com", "google"},        // aspmx.l.google.com, *.googlemail.com
	{"googlemail.com", "google"},
	{"outlook.com", "o365"},         // *.mail.protection.outlook.com
	{"protection.outlook.com", "o365"},
	{"office365.com", "o365"},
	{"yahoodns.net", "yahoo"},
	{"yahoo.com", "yahoo"},
	{"olc.protection.outlook.com", "o365"},
}

type ProviderClassifyLayer struct{}

func (ProviderClassifyLayer) ID() string { return "provider_classify" }

func (ProviderClassifyLayer) Run(ctx context.Context, ev *Evidence) LayerOutput {
	hosts, outcome := ev.mx(ctx)
	if outcome != mxFound {
		// No usable MX → we cannot classify a provider. Not a flag, not a guess.
		return LayerOutput{LayerID: "provider_classify", SignalKey: "mx_provider", Result: Skip, SeverityHint: Info, Value: ""}
	}
	provider := classifyProvider(ev.Domain, hosts)
	return LayerOutput{LayerID: "provider_classify", SignalKey: "mx_provider", Result: Flag, SeverityHint: Info, Value: provider}
}

// classifyProvider inspects the MX hosts (and the domain) to bucket the provider. "self"
// means the domain runs its own mail (MX points within the domain); "other" is the honest
// catch-all when no known provider matches — never guessed.
func classifyProvider(domain string, hosts []MXHost) string {
	for _, h := range hosts {
		host := strings.ToLower(h.Host)
		for _, p := range providerMXSuffixes {
			if host == p.suffix || strings.HasSuffix(host, "."+p.suffix) {
				return p.provider
			}
		}
	}
	// Self-hosted: an MX whose host is within the domain (or the implicit-MX == domain case).
	for _, h := range hosts {
		host := strings.ToLower(h.Host)
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return "self"
		}
	}
	return "other"
}

// DefaultOrder is the layer execution order used by the cold-email policy (P1 §7.2). The
// policy's `order` is authoritative; this is the built-in default the orchestrator uses when
// building the layer set. Cheap/definitive layers first (cheapest-sufficient stop-early).
func DefaultOrder() []Layer {
	return []Layer{
		SyntaxLayer{},
		MXLayer{},
		NullMXLayer{},
		DisposableLayer{},
		SpamTrapLayer{},
		ProviderClassifyLayer{},
		RoleLayer{},
		TypoLayer{},
		FreeCorpLayer{},
	}
}
