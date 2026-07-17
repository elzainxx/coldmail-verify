package policy

import "github.com/mnfi/codmailling/control-plane/internal/layers"

// ColdEmailKey/Version identify the one shipped Phase-1 policy (§7.3).
const (
	ColdEmailKey     = "cold-email"
	ColdEmailVersion = 1
)

// ColdEmailDefinitionJSON is the canonical `cold-email` v1 definition (§7.2/§7.3), stored in
// the policies table as the single system default (is_default). It is DATA, not code — this
// constant is the seed the migrations/bootstrap insert. The action grammar
// "<status>:<reason>:<stop|continue>" and the open-object shape are LOCKED; the specific
// mappings + confidence numbers are DIALs (§7.4, recalibrated by P7).
//
// OQ-7 is visible here: disposable:pass → risky:disposable:stop (RISKY, never invalid), and
// recommendation_overrides_by_reason.disposable = skip (the policy skips it without lying).
// NOTE on order (a DIAL — manifest §1.3 "LOCKED shape / DIAL contents"): null_mx runs
// BEFORE mx. The spec's §7.2 illustrative order lists mx first, but with mx first the
// general mx:fail gate (no_mx) fires on a null-MX domain and MASKS the more specific
// null_mx:pass gate, making it unreachable. Checking the explicit RFC-7505 signal first
// yields the correct, specific reason. This is a dial choice (surfaced in phase-1-status.md),
// not a frozen-door change. All four cases verified: good / no-mx / null-mx / transient.
const ColdEmailDefinitionJSON = `{
  "order": ["syntax", "null_mx", "mx", "spam_trap", "disposable", "provider_classify", "role", "typo", "free_corp"],
  "gates": {
    "syntax:fail":     "invalid:invalid_syntax:stop",
    "mx:fail":         "invalid:no_mx:stop",
    "mx:unknown":      "unknown:dns_error:stop",
    "null_mx:pass":    "invalid:null_mx:stop",
    "spam_trap:pass":  "invalid:spam_trap:stop",
    "disposable:pass": "risky:disposable:stop"
  },
  "flags": ["role", "typo", "free_corp", "disposable"],
  "recommendation_overrides_by_reason": { "disposable": "skip" },
  "provider_routing": { "google": "mx", "yahoo": "mx", "o365": "mx" },
  "confidence": {
    "default": 30,
    "baseline_by_reason": {
      "invalid_syntax": 99,
      "no_mx": 96,
      "null_mx": 97,
      "spam_trap": 95,
      "dns_error": 10,
      "disposable": 40,
      "provider_no_smtp": 35,
      "plausible": 40
    },
    "flag_deltas": { "is_role": -5, "has_typo": -10 }
  },
  "recommendation_thresholds": { "send": 70, "review": 30 }
}`

// ColdEmailDefinition parses the shipped policy (a convenience for tests + bootstrap).
func ColdEmailDefinition() (*Definition, error) {
	return Parse(ColdEmailKey, ColdEmailVersion, []byte(ColdEmailDefinitionJSON))
}

// LayerSet maps layer IDs → the built-in layer implementations, for the engine's `order`
// lookup. It is the bridge between the policy's data (`order`) and the code (layers.*).
func LayerSet() map[string]layers.Layer {
	set := map[string]layers.Layer{}
	for _, l := range layers.DefaultOrder() {
		set[l.ID()] = l
	}
	return set
}
