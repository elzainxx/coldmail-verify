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
// Phase-2 additive change (§7.3): `smtp` is appended to `order` and SMTP gates + confidence
// baselines are added. This is ADDITIVE — the action grammar, the open-object shape, and every
// reason used were all frozen/reserved in P1 (Door 4): ZERO new enum values, ZERO grammar change.
// `smtp` in `order` is a Type-1 core layer the ORCHESTRATOR runs as a stage AFTER the cheap layers
// (§7.1) — the engine skips it as a plain layer (no cheap `smtp` layer is registered), and the
// orchestrator resolves the non-terminal residue against a verifier-node. The `smtp:*` gates
// document the SMTP status→reason map (§7.2); the SMTP scorer emits these directly. OQ-P2-8:
// smtp is enabled in the policy, routed to the mock node in P2; P3 flips the node to real.
// Phase-6 additive change (§3.2): the `external` layer is appended to `order` (LAST — the most
// expensive layer, run only on residue) and its gate rules + the `external` policy block are added.
// This is ADDITIVE — reason=external_verified was RESERVED in P1 (Door 4: ZERO new enum values), the
// action grammar/open-object shape are frozen, and the `external` key is the extension point P1 §7.2
// explicitly reserved. The external:valid gate is `risky:external_verified:stop` — NOT valid: the
// anti-laundering rule (§3.4) is expressed in the DATA too, so even the policy grammar never lets a
// rented "valid" become SMTP-grade valid. The layer is ENABLED in the shipped policy (OQ-P6-2: all
// four residue classes at launch) — but "enabled in the policy" is NOT "egressing": the runtime layer
// only calls a vendor when the kill-switch permits (Fallback==enabled — DEFAULT off) AND a vendor key
// is present AND the DPA is confirmed (cmd/control-plane buildExternal — four independent switches).
// A policy WITHOUT this block simply never runs the layer (the mechanical privacy opt-out, §7.5).
const ColdEmailDefinitionJSON = `{
  "order": ["syntax", "null_mx", "mx", "spam_trap", "disposable", "provider_classify", "role", "typo", "free_corp", "smtp", "external"],
  "gates": {
    "syntax:fail":     "invalid:invalid_syntax:stop",
    "mx:fail":         "invalid:no_mx:stop",
    "mx:unknown":      "unknown:dns_error:stop",
    "null_mx:pass":    "invalid:null_mx:stop",
    "spam_trap:pass":  "invalid:spam_trap:stop",
    "disposable:pass": "risky:disposable:stop",
    "smtp:valid":         "valid:accepted:stop",
    "smtp:invalid":       "invalid:mailbox_not_found:stop",
    "smtp:catch_all":     "risky:catch_all:stop",
    "smtp:mailbox_full":  "valid:mailbox_full:stop",
    "smtp:unknown":       "unknown:smtp_error:stop",
    "external:valid":     "risky:external_verified:stop",
    "external:invalid":   "invalid:external_verified:stop",
    "external:risky":     "risky:external_verified:stop",
    "external:unknown":   "unknown:external_verified:stop"
  },
  "flags": ["role", "typo", "free_corp", "disposable"],
  "recommendation_overrides_by_reason": { "disposable": "skip" },
  "provider_routing": { "google": "mx", "yahoo": "mx", "o365": "mx" },
  "rate_limits": {
    "default": { "max_inflight": 2, "max_starts_per_window": 30, "window_s": 60, "start_delay_jitter_ms": [50, 500] }
  },
  "external": {
    "enabled": true,
    "route_when": ["catch_all", "provider_no_smtp", "retry_exhausted_greylisted", "gave_up_rate_limited"],
    "vendor": "millionverifier",
    "confidence_cap": 60,
    "on_vendor_unknown": "keep_residue",
    "on_budget_exhausted": "keep_residue",
    "on_vendor_error": "keep_residue"
  },
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
      "plausible": 40,
      "accepted": 90,
      "mailbox_not_found": 95,
      "mailbox_full": 90,
      "catch_all": 40,
      "greylisted": 10,
      "smtp_blocked": 10,
      "smtp_timeout": 10,
      "smtp_error": 10,
      "connection_failed": 10,
      "external_verified": 60
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
