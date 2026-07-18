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
	"github.com/mnfi/codmailling/control-plane/internal/health"
	"github.com/mnfi/codmailling/control-plane/internal/node"
	"github.com/mnfi/codmailling/control-plane/internal/orchestrator"
	"github.com/mnfi/codmailling/control-plane/internal/policy"
	"github.com/mnfi/codmailling/control-plane/internal/provrouting"
	"github.com/mnfi/codmailling/control-plane/internal/ratelimit"
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
	tokenFor := func(id string) string {
		if t, ok := sec.NodeToken(id); ok {
			return t
		}
		return ""
	}
	client := node.NewHTTPClient(cfg.SMTPClientDeadline())

	// §4: the per-provider rate limiter (durable budget in Postgres, ephemeral in-flight in-process).
	// Its caps come from the policy rate_limits key (§4.7, global-with-override OQ-P4-5). §3.2/§3.4:
	// the router's Strategy is now sticky-by-domain (HRW) with a least-inflight fallback; it consults
	// the limiter for per-node rate headroom (§3.3) + in-flight ranking (§3.4). This is the "one
	// mechanism in two parts" — sticky concentrates, the limiter paces (§4.1). A nil rate_limits key
	// makes the limiter inert (no rate limiting), and the strategy degrades to pure sticky affinity.
	var rateLimits *policy.RateLimits
	if def, derr := policy.ColdEmailDefinition(); derr == nil {
		rateLimits = def.RateLimits
	}
	limiter := ratelimit.New(ratelimit.Config{Budget: pg, Limits: rateLimits})
	sticky := router.NewSticky(
		func(rctx context.Context, nodeID string, plan router.Plan) bool {
			ok, herr := limiter.HasHeadroom(rctx, nodeID, ratelimit.BucketForDomain(plan.Provider, plan.Domain))
			return herr == nil && ok // a headroom-read error fails closed (no headroom → not this home)
		},
		func(nodeID string, plan router.Plan) int {
			return limiter.Inflight(nodeID, ratelimit.BucketForDomain(plan.Provider, plan.Domain))
		},
	)
	reg := router.New(cfg.Nodes, sticky)

	// The bootstrap /v1/config fetch just needs SOME configured node's base URL; use the first enabled
	// one directly (the eligibility filter that gates :smtp is applied per request via SMTPRoute, not
	// here). This keeps N=1 and N>1 identical — the config-fetch node is a bootstrap detail.
	var n config.NodeDescriptor
	for _, nd := range cfg.Nodes {
		if nd.Enabled {
			n = nd
			break
		}
	}
	if n.ID == "" {
		log.Printf("verify: SMTP enabled but no enabled node in the config list — staying cheap-layers-only")
		return nil, fallback
	}

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

	// §5: start the CONTINUOUS health-watcher (the P4 control loop). It REPLACES the P3 minimal
	// re-audit ticker (§1.2): it owns the audit cadence AND adds liveness polling + block-rate-driven
	// quarantine, with the three guards (sample gate, hysteresis, fleet-wide brake) + canary recovery.
	// The block-rate (statistical, A1) trigger is recommend-only by default (DR-2 shadow-mode) — the
	// watcher performs ZERO live block-rate mutation until an owner promotes A1 to auto (the
	// fresh-system invariant). Audit-red (A2) + canary (A3) stay auto. Runs on the process-lifetime
	// appCtx (cancelled on shutdown), the single writer of nodes.status (§5.1).
	startHealthWatcher(appCtx, cfg, client, pg, tokenFor)

	return &orchestrator.SMTPDeps{
		Client:         client,
		Registry:       reg,
		TokenFor:       tokenFor,
		SafeCheckOn:    false, // OQ-P2-7: smtp_safe_check OFF by default
		ClientDeadline: cfg.SMTPClientDeadline(),
		// Phase 3 (§4.4/§4.5/§5.2): the eligibility gate reads health from the store; MaxAuditAge is
		// the staleness window; WarmupCap is the per-node/per-day ascending :smtp ceiling.
		Health:        pg,
		MaxAuditAge:   cfg.AuditMaxAge(),
		WarmupCap:     cfg.Dials.WarmupDailyCap,
		CanaryCap:     cfg.Dials.CanaryWarmupCap, // §5.4: the LOW cap a recovering canary node is metered by
		CanarySampleN: cfg.Dials.CanarySampleN,   // §5.4/GAP-30: canary domain-sample rate (real samples in a healthy pool)
		// Phase 4 (§4): the per-provider rate limiter + a real jitter sleeper.
		Limiter: limiter,
		Sleep:   time.Sleep,
	}, fingerprint
}

// startHealthWatcher launches the §5 CONTINUOUS health-watcher in the background — the P4 control
// loop that REPLACES the P3 minimal re-audit ticker (§1.2). It owns the audit cadence (via the P3
// scheduler's Observe: fetch + record, no transition) AND adds liveness polling + block-rate-driven
// quarantine, with the three guards (sample gate GAP-27, hysteresis, fleet-wide brake GAP-28) and
// the canary recovery (GAP-30). It is the SINGLE WRITER of nodes.status (§5.1). The block-rate (A1)
// trigger is recommend-only by default (DR-2) — zero live block-rate mutation until an owner
// promotes A1 to auto. The client is the live HTTPClient (real /v1/audit + /healthcheck); on the
// local build with no real box the audit fetch fails → no promotion (honest, harden-toward-unknown).
func startHealthWatcher(ctx context.Context, cfg config.Config, client *node.HTTPClient, pg *store.PG, tokenFor func(string) string) {
	pool := make([]health.WatchedNode, 0, len(cfg.Nodes))
	for _, nd := range cfg.Nodes {
		if !nd.Enabled {
			continue
		}
		pool = append(pool, health.WatchedNode{
			NodeRef:    audit.NodeRef{ID: nd.ID, BaseURL: nd.BaseURL, Token: tokenFor(nd.ID), PublicEgressIP: nd.PublicEgressIP},
			VerifierIP: nd.PublicEgressIP, // == the node's egress IP (the block-rate GROUP BY key, §5.6)
		})
	}
	if len(pool) == 0 {
		return
	}
	sched := audit.NewScheduler(audit.Deps{Client: client, Store: pg}, cfg.Dials.CanaryRecoverGreens)
	blockMode := health.ModeRecommendOnly // DR-2: A1 statistical trigger defaults to recommend-only
	if cfg.ShadowModes.A1BlockRateQuarantine == config.ModeAuto {
		blockMode = health.ModeAuto // owner-promoted (admin-audited under DR-3)
	}
	params := health.Params{
		MinSamples:        cfg.Dials.BlockMinSamples,
		QuarThreshold:     float64(cfg.Dials.BlockQuarThresholdPct) / 100.0,
		RecoverThreshold:  float64(cfg.Dials.BlockRecoverThreshPct) / 100.0,
		RecoverGreens:     cfg.Dials.CanaryRecoverGreens,
		MinCanarySamples:  cfg.Dials.CanaryMinSamples, // §5.4/GAP-30 (no zero-sample promotion)
		LivenessMisses:    cfg.Dials.LivenessMisses,
		FleetWideFraction: float64(cfg.Dials.FleetWidePct) / 100.0,
		MinActiveFloor:    cfg.Dials.PoolEligibleFloor, // §5.5 backstop: block-rate never drops below the serving floor
		BlockMode:         blockMode,
	}
	w := health.NewWatcher(health.Deps{
		Audit: sched, Liveness: client, BlockRate: pg, Status: pg,
	}, params, cfg.BlockRateWindow(), cfg.Dials.CanaryWarmupCap)
	log.Printf("health-watcher: starting (cadence=%s window=%s block_mode=%s fleet_brake=%d%%)",
		cfg.WatcherCadence(), cfg.BlockRateWindow(), blockMode, cfg.Dials.FleetWidePct)
	go w.Run(ctx, pool, cfg.WatcherCadence())
}
