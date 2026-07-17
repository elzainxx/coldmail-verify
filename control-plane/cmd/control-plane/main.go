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
	"github.com/mnfi/codmailling/control-plane/internal/config"
	"github.com/mnfi/codmailling/control-plane/internal/hash"
	"github.com/mnfi/codmailling/control-plane/internal/node"
	"github.com/mnfi/codmailling/control-plane/internal/orchestrator"
	"github.com/mnfi/codmailling/control-plane/internal/policy"
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

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           buildAPI(cfg, sec, hasher).Handler(),
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
func buildAPI(cfg config.Config, sec *secrets.Secrets, hasher *hash.Hasher) *api.Server {
	srv := api.New(cfg, sec, hasher)
	if hasher == nil || cfg.DatabaseURL == "" {
		log.Printf("verify: no %s configured or no pepper — /v1/verify will report not-ready (503)",
			"TM_DATABASE_URL")
		return srv
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	// Phase 2: wire the SMTP stage IF SMTP is enabled (kill-switch not Rung 1) AND a node list
	// exists. Otherwise the orchestrator stays cheap-layers-only (P1 steady state / Rung 3).
	smtpDeps, cfgFingerprint := buildSMTP(ctx, cfg, sec)

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
func buildSMTP(ctx context.Context, cfg config.Config, sec *secrets.Secrets) (*orchestrator.SMTPDeps, string) {
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
	return &orchestrator.SMTPDeps{
		Client:         client,
		Registry:       reg,
		TokenFor:       tokenFor,
		SafeCheckOn:    false, // OQ-P2-7: smtp_safe_check OFF by default
		ClientDeadline: cfg.SMTPClientDeadline(),
	}, fingerprint
}
