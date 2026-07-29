// Command hoardd is the hoard central backup server: it exposes a control API
// and dashboard, mirrors the local (hot) restic repo that clients push to up to
// an IDrive e2 (cold) repo on a schedule, verifies integrity, applies
// retention, and alerts on failures or stale clients.
//
// It intentionally does NOT run the restic REST server itself — run restic's
// rest-server alongside it (see README) so clients have a push target. hoardd
// reads and mirrors the repo that rest-server writes.
package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nauski/hoard/internal/api"
	"github.com/nauski/hoard/internal/config"
	"github.com/nauski/hoard/internal/restic"
	"github.com/nauski/hoard/internal/scheduler"
	"github.com/nauski/hoard/internal/state"
)

//go:embed all:web
var webFiles embed.FS

func main() {
	cfgPath := flag.String("config", "/data/hoard.json", "path to config file")
	resticBin := flag.String("restic", "restic", "path to restic binary")
	debug := flag.Bool("debug", false, "verbose logging")
	initRepos := flag.Bool("init", false, "initialize hot and cold repos if missing, then exit")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	hot := restic.New(*resticBin, cfg.Hot)
	cold := restic.New(*resticBin, cfg.Cold)

	if *initRepos {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := hot.EnsureInit(ctx); err != nil {
			log.Error("init hot repo", "err", err)
			os.Exit(1)
		}
		if err := cold.EnsureInit(ctx); err != nil {
			log.Error("init cold repo", "err", err)
			os.Exit(1)
		}
		log.Info("repos initialized")
		return
	}

	store, err := state.Load(cfg.StatePath)
	if err != nil {
		log.Error("load state", "err", err)
		os.Exit(1)
	}

	sub, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Error("web assets", "err", err)
		os.Exit(1)
	}

	// The API server doubles as the alert Notifier for the scheduler; wire it
	// in after construction so both share one scheduler (and one job lock).
	sched := scheduler.New(cfg, hot, cold, store, log)
	srv := api.New(cfg, sched, store, hot, cold, log, sub)
	sched.SetNotifier(srv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go sched.Run(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
}
