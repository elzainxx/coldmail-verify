-- Migration 0006: grant cp_runtime the catch-all WRITE (Phase 2, §5.6)
-- Implements: Phase-2 spec §5.6 (the brain — never the box — persists the catch-all conclusion).
--
-- WHY THIS IS NOT A FROZEN-DOOR CHANGE:
--   catchall_domains is mutable, per-domain BRAIN state (tri-state is_catchall), explicitly
--   NOT the append-only LOG (that is `verifications`, Door 2, untouched here). The P1→P2 handoff
--   (§3) states plainly: "catchall_domains is created EMPTY in P1 and P2 WRITES it (brain-side
--   only — boxes never touch the DB)." P1 granted cp_runtime SELECT-only because P1 never probed;
--   P2 introduces the probe (§5), so the writer arrives now. This is the standard additive-migration
--   pattern: grant the privilege when the code that needs it lands. No table/column/CHECK changes.
--
-- WHAT IS GRANTED (and deliberately NOT):
--   INSERT + UPDATE  — the brain upserts a domain's conclusion (§5.6): first probe INSERTs,
--                      a re-probe UPDATEs is_catchall/detected_at/expires_at. An `inconclusive`
--                      result is a no-op at the app layer (never overwrites a known value with
--                      NULL, §5.6) — enforced in Go (store.SetCatchAll), not by grant.
--   NO DELETE        — a domain's catch-all knowledge is never erased by the runtime; if a purge
--                      is ever needed it is a cp_admin (break-glass) action, mirroring the LOG's
--                      "runtime cannot destroy history" posture (PN-9/DR-3). Kept SELECT+INSERT+
--                      UPDATE only, DELETE stays admin.
--
-- disposable_domains / spam_trap_domains remain SELECT-only for cp_runtime (those lists are
-- admin/seed-curated, not brain-probed) — this migration touches ONLY catchall_domains.

GRANT INSERT, UPDATE ON catchall_domains TO cp_runtime;
REVOKE DELETE ON catchall_domains FROM cp_runtime;

COMMENT ON TABLE catchall_domains IS 'Per-domain accept-all state (tri-state is_catchall, NULL=unknown). P2 §5.6: the BRAIN probes and upserts (INSERT+UPDATE); DELETE stays cp_admin. NOT the append-only LOG. Analytics read this table, not the flattened flag.';
