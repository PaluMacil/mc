// Command mc-invite lets trusted friends (the inviter role) mint single-use,
// time-limited invite links; an invitee opens a link, enters their Minecraft
// Java username, and is resolved against Mojang and added to the server
// whitelist over RCON, transactionally with marking the invite used. Admins
// manage everyone and see the audit log. Auth is OIDC against Authentik.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Embed the timezone database so LoadLocation works on a distroless image
	// that ships no tzdata.
	_ "time/tzdata"

	"github.com/alexedwards/scs/v2"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return fmt.Errorf("loading timezone %q: %w", cfg.TZ, err)
	}

	ctx := context.Background()

	store, err := NewStore(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}

	sessions := scs.New()
	sessions.Lifetime = 12 * time.Hour
	sessions.IdleTimeout = 2 * time.Hour
	sessions.Cookie.Name = "mc_invite_session"
	sessions.Cookie.Path = cfg.BasePath + "/"
	sessions.Cookie.HttpOnly = true
	sessions.Cookie.Secure = true
	sessions.Cookie.SameSite = http.SameSiteLaxMode

	// OIDC discovery needs Authentik reachable; bound the wait so a bad config
	// fails fast rather than hanging the pod's startup.
	discCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	auth, err := NewAuth(discCtx, cfg, sessions, log)
	cancel()
	if err != nil {
		return fmt.Errorf("oidc setup: %w", err)
	}

	// The Downloads page mints presigned R2 URLs. If R2 is not configured, leave
	// the presigner nil and let the page show as unavailable rather than blocking
	// the rest of the portal from starting.
	var presign presigner
	if cfg.R2Endpoint != "" && cfg.R2Bucket != "" && cfg.R2AccessKeyID != "" && cfg.R2SecretAccessKey != "" {
		presign = r2Presigner{
			endpoint:  cfg.R2Endpoint,
			bucket:    cfg.R2Bucket,
			accessKey: cfg.R2AccessKeyID,
			secretKey: cfg.R2SecretAccessKey,
		}
	} else {
		log.Warn("R2 not configured; the downloads page will show as unavailable")
	}

	srv := &Server{
		cfg:      cfg,
		store:    store,
		auth:     auth,
		sessions: sessions,
		mojang:   MojangResolver{},
		rcon:     RCONClient{Addr: cfg.RCONAddr, Password: cfg.RCONPassword},
		presign:  presign,
		players:  &playersCache{ttl: 10 * time.Second},
		limiter:  newIPLimiter(5, 30*time.Second),
		loc:      loc,
		log:      log,
	}

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdownCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("mc-invite listening", "addr", cfg.ListenAddr, "base", cfg.BasePath)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-shutdownCtx.Done():
		log.Info("shutting down")
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		return httpSrv.Shutdown(sctx)
	}
}
