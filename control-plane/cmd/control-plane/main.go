// Command control-plane is the control-plane entrypoint. Phase 0: load config + secrets, wire
// the hasher and the HTTP front door, and serve. It builds and runs empty — no real
// verification. It proves: config loads (manifest flags readable), the secrets scaffold
// works, the kill-switch defaults to cheap-layers-only, and the Door-1 endpoint responds.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/api"
	"github.com/mnfi/codmailling/control-plane/internal/audit"
	"github.com/mnfi/codmailling/control-plane/internal/config"
	"github.com/mnfi/codmailling/control-plane/internal/hash"
	"github.com/mnfi/codmailling/control-plane/internal/node"
	"github.com/mnfi/codmailling/control-plane/internal/orchestrator"
	"github.com/mnfi/codmailling/control-plane/internal/policy"
	"github.com/mnfi/codmailling/control-plane/internal/provrouting"
	"github.com/mnfi/codmailling/control-plane/internal/router"
	"github.com/mnfi/codmailling/control-plane/internal/secrets"
	"github.com/mnfi/codmailling/control-plane/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := config.LoadFromOS()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("control-plane starting: %s", cfg.Summary())

	// Secrets: at Phase 0 we do NOT require core secrets so `control-plane` can boot for a build
	// smoke without a real pepper. A real (feature-serving) deployment sets requireCore=true.
	requireCore := cfg.Env == "production"
	sec, err := secrets.Load(secrets.EnvSource{}, requireCore)
	if err != nil {
		log.Fatalf("secrets: %v", err)
	}
	log.Printf("secrets: %s", sec.String()) // redacted — presence only

	// Hasher (Door 3) is wired only if a pepper is present; otherwise readiness reports not-ready.
	var hasher *hash.Hasher
	if sec.HasPepper() {
		p, _ := sec.Pepper()
		if hasher, err = hash.NewHasher(p); err != nil {
			log.Fatalf("hash: %v", err)
		}
	} else {
		log.Printf("hash: no pepper loaded — /readyz will report not-ready (a build smoke is fine)")
	}

	// appCtx lives for the whole process (cancelled on shutdown). The P3 re-audit ticker rides it so
	// it stops cleanly on exit; setup DB ops use their own short timeout.
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           buildAPI(appCtx, cfg, sec, hasher).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	go func() {
		log.Printf("listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Printf("stopped")
}

// buildAPI wires the front door. If a database is configured AND a pepper is present, it
// attaches the real orchestrator (the cheap-layers verify lifecycle). Otherwise it returns a
// front door with NO verifier — /v1/verify honestly reports 503 (not ready) rather than
// fabricating a verdict. Cheap layers only; no port 25 (Phase 1).
func buildAPI(appCtx context.Context, cfg config.Config, sec *secrets.Secrets, hasher *hash.Hasher) *api.Server {
	srv := api.New(cfg, sec, hasher)
	if hasher == nil || cfg.DatabaseURL == "" {
		log.Printf("verify: no %s configured or no pepper — /v1/verify will report not-ready (503)",
			"TM_DATABASE_URL")
		return srv
	}
	// Setup DB ops use a short timeout; the long-lived appCtx is passed to buildSMTP for the ticker.
	ctx, cancel := context.WithTimeout(appCtx, 10*time.Second)
	defer cancel()
	pg, err := store.OpenPG(ctx, cfg.DatabaseURL)
	if err != nil {
		// A configured-but-unreachable DB is a real error, but the control-plane can still
		// serve health/readiness; verify stays 503 until the DB is reachable. Log loudly.
		log.Printf("verify: store unavailable (%v) — /v1/verify will report not-ready (503)", err)
		return srv
	}
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		log.Printf("verify: policy load failed (%v) — /v1/verify not wired", err)
		return srv
	}
	// Seed the code-owned default policy into the store if absent (idempotent, §4.6).
	if err := pg.EnsurePolicy(ctx, policy.ColdEmailKey, policy.ColdEmailVersion,
		[]byte(policy.ColdEmailDefinitionJSON), true); err != nil {
		log.Printf("verify: could not ensure default policy (%v) — /v1/verify not wired", err)
		return srv
	}
	// Phase 2/3: wire the SMTP stage IF SMTP is enabled (kill-switch not Rung 1) AND a node list
	// exists. Otherwise the orchestrator stays cheap-layers-only (P1 steady state / Rung 3). P3 also
	// registers each node in the `nodes` table (provisioning) + starts the minimal re-audit ticker
	// (which rides appCtx so it lives for the whole process, not the 10s setup ctx).
	smtpDeps, cfgFingerprint := buildSMTP(ctx, appCtx, cfg, sec, pg)

	orch := orchestrator.New(orchestrator.Deps{
		Hasher:            hasher,
		Store:             pg,
		CacheTTL:          cfg.CacheTTL(),
		ConfigFingerprint: cfgFingerprint,
		SMTP:              smtpDeps,
	})
	mode := "cheap-layers-only"
	if smtpDeps != nil {
		mode = "SMTP-enabled (against node list)"
	}
	log.Printf("verify: orchestrator wired (%s, policy=%s v%d, cache_ttl=%s)",
		mode, def.Key, def.Version, cfg.CacheTTL())
	return srv.WithVerifier(orch, def)
}

// buildSMTP wires the Phase-2 SMTP stage when SMTP is enabled and a node list exists. It fetches
// the fork's /v1/config (§4.6, GAP-3) from the first eligible node and computes the CANONICAL
// config_fingerprint over the effective engine config — retiring the P1 "engine=unconfigured"
// placeholder. If SMTP is off, no node exists, or the config fetch fails, it returns
// (nil, P1-fingerprint): the brain degrades to cheap-layers-only rather than SMTP on an unknown
// engine config (harden-toward-unknown / §4.6 "refuse a confident fingerprint" on failure).
func buildSMTP(ctx, appCtx context.Context, cfg config.Config, sec *secrets.Secrets, pg *store.PG) (*orchestrator.SMTPDeps, string) {
	fallback := cfg.ConfigFingerprint() // P1 engine=unconfigured fingerprint
	if !cfg.SMTPEnabled() || len(cfg.Nodes) == 0 {
		return nil, fallback
	}
	reg := router.New(cfg.Nodes, nil)
	plan := router.Plan{}
	n, err := reg.Route(ctx, plan)
	if err != nil {
		log.Printf("verify: SMTP enabled but no eligible node — staying cheap-layers-only")
		return nil, fallback
	}
	tokenFor := func(id string) string {
		if t, ok := sec.NodeToken(id); ok {
			return t
		}
		return ""
	}
	client := node.NewHTTPClient(cfg.SMTPClientDeadline())

	// §4.6: fingerprint the engine's effective config. On failure, refuse a confident engine
	// fingerprint — degrade to cheap-only (don't run SMTP against an unknown config).
	fpCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ec, ferr := client.FetchConfig(fpCtx, n.BaseURL, tokenFor(n.ID))
	if ferr != nil {
		log.Printf("verify: /v1/config fetch failed (%v) — cheap-layers-only (won't SMTP on an unknown engine config)", ferr)
		return nil, fallback
	}
	fingerprint := cfg.EngineConfigFingerprint(ec.CanonicalFingerprint())
	log.Printf("verify: SMTP node=%s config_fingerprint=%s", n.ID, fingerprint)

	// §5.1/GAP-19/OQ-P3-8: provider :mx routing has ONE source (the policy provider_routing map);
	// the engine's validation_type_for is a GENERATED render of it. Assert the box's reported
	// validation_type_for matches the render — a mismatch is a LOGGED drift alarm (the brain is
	// authoritative; the box's map is a synced backstop), never silent divergence, never auto-fix.
	if def, derr := policy.ColdEmailDefinition(); derr == nil {
		expected := provrouting.Render(def.ProviderRouting)
		if drifts := provrouting.Drift(expected, ec.ValidationTypeFor()); len(drifts) > 0 {
			log.Printf("verify: WARNING provider-routing drift (engine validation_type_for out of sync with policy source, §5.1): %v", drifts)
		} else {
			log.Printf("verify: provider-routing backstop in sync (validation_type_for matches policy source)")
		}
	}

	// §3.6/§4.5: register each configured node in the `nodes` table as `provisioning` (config-list
	// is authoritative — P1 OQ-5; this mirrors it + gives the audit summary a row to update). The
	// node stays provisioning → NOT :smtp-eligible until the first green audit ∧ port25_open. NOTE:
	// port25_open (§3.4 egress smoke test) is PENDING-REAL-BOX — false here in the local build, so no
	// node is :smtp-eligible until a real host proves egress-25 and audits green. The eligibility
	// LOGIC that consumes port25_open is fully built + tested (audit/router tests); only the real
	// value is deferred. Harden-toward-unknown: an unregisterable node just isn't eligible.
	for _, nd := range cfg.Nodes {
		if !nd.Enabled {
			continue
		}
		if rerr := pg.UpsertNodeState(ctx, nd.ID, nd.BaseURL, "provisioning", false); rerr != nil {
			log.Printf("verify: WARNING could not register node %s (%v) — it will not be :smtp-eligible", nd.ID, rerr)
		}
	}

	// §4.5: start the MINIMAL scheduled re-audit ticker (NOT the P4 continuous health-watcher — no
	// /health poll, no block-rate). It re-runs /v1/audit on a cadence, records the append-only
	// history, and drives auto-quarantine-on-red / N-green-recover. Runs in the background on the
	// process-lifetime appCtx (cancelled on shutdown). On a real box this is what promotes
	// provisioning→active once PTR/DNS align and stops :smtp the moment an audit goes red.
	startReAuditTicker(appCtx, cfg, client, pg, tokenFor)

	return &orchestrator.SMTPDeps{
		Client:         client,
		Registry:       reg,
		TokenFor:       tokenFor,
		SafeCheckOn:    false, // OQ-P2-7: smtp_safe_check OFF by default
		ClientDeadline: cfg.SMTPClientDeadline(),
		// Phase 3 (§4.4/§4.5/§5.2): the eligibility gate reads health from the store; MaxAuditAge is
		// the staleness window; WarmupCap is the per-node/per-day ascending :smtp ceiling.
		Health:      pg,
		MaxAuditAge: cfg.AuditMaxAge(),
		WarmupCap:   cfg.Dials.WarmupDailyCap,
	}, fingerprint
}

// startReAuditTicker launches the §4.5 minimal re-audit loop in the background. The CADENCE lives
// here (in the caller), keeping the scheduler itself a pure, testable actor — and keeping the
// "not a P4 daemon" boundary honest (this is a simple ticker over a fixed node list, not a
// health-watcher). It audits once immediately (so a box that's already green promotes at boot),
// then every AuditInterval. Each pass is audit.Scheduler.AuditAll; auto-quarantine is a state
// transition inside it. Block-rate-driven quarantine is explicitly NOT here (GAP-18 → P4).
func startReAuditTicker(ctx context.Context, cfg config.Config, client node.AuditClient, s audit.AuditStore, tokenFor func(string) string) {
	refs := make([]audit.NodeRef, 0, len(cfg.Nodes))
	for _, nd := range cfg.Nodes {
		if !nd.Enabled {
			continue
		}
		refs = append(refs, audit.NodeRef{ID: nd.ID, BaseURL: nd.BaseURL, Token: tokenFor(nd.ID), PublicEgressIP: nd.PublicEgressIP})
	}
	if len(refs) == 0 {
		return
	}
	sched := audit.NewScheduler(audit.Deps{Client: client, Store: s}, cfg.Dials.AuditRecoverGreens)
	interval := cfg.AuditInterval()
	go func() {
		auditPass := func() {
			for _, out := range sched.AuditAll(ctx, refs) {
				if out.Err != nil {
					log.Printf("audit: node=%s error: %v", out.NodeID, out.Err)
					continue
				}
				if out.EgressMismatch {
					log.Printf("audit: node=%s WARNING egress-IP != audited current_host_ip (GAP-24) — will NOT promote", out.NodeID)
				}
				if out.From != out.To {
					log.Printf("audit: node=%s %s -> %s (audit=%s)", out.NodeID, out.From, out.To, out.Status)
				}
			}
		}
		auditPass() // immediate first audit at boot
		if interval <= 0 {
			return
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				auditPass()
			}
		}
	}()
}
