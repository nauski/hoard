// Command hoard-agent is the desktop backup client. It serves a local web GUI
// where you choose what to back up and when, and pushes those backups to a
// hoard server's restic REST endpoint. Run it as a per-user service; open the
// GUI at http://127.0.0.1:7420.
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

	"github.com/nauski/hoard/internal/agent"
)

//go:embed all:web
var webFiles embed.FS

func main() {
	listen := flag.String("listen", "127.0.0.1:7420", "address for the local GUI")
	cfgPath := flag.String("config", defaultConfigPath(), "path to agent config JSON")
	resticBin := flag.String("restic", "restic", "path to restic binary")
	debug := flag.Bool("debug", false, "verbose logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	a, err := agent.Load(*cfgPath, *resticBin, log)
	if err != nil {
		log.Error("load agent config", "err", err)
		os.Exit(1)
	}

	sub, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Error("web assets", "err", err)
		os.Exit(1)
	}
	srv := agent.NewServer(a, log, sub)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go a.RunScheduler(ctx)

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("hoard-agent GUI listening", "addr", *listen, "config", *cfgPath)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
}

// defaultConfigPath honors XDG_CONFIG_HOME, falling back to ~/.config.
func defaultConfigPath() string {
	if d := os.Getenv("HOARD_AGENT_CONFIG"); d != "" {
		return d
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = home + "/.config"
		}
	}
	return base + "/hoard-agent/config.json"
}
