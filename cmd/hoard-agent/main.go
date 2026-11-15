// Command hoard-agent is the desktop backup client. It serves a local web GUI
// where you choose what to back up and when, and pushes those backups to a
// hoard server's restic REST endpoint. Run it as a per-user service; open the
// GUI at http://127.0.0.1:7420. `hoard-agent [flags] backup` instead runs one
// backup with the saved config and exits (for systemd timers and cron).
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
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
	notifyBin := flag.String("notify-send", "", "path to notify-send for desktop notifications (empty = disabled)")
	debug := flag.Bool("debug", false, "verbose logging")
	enrollTok := flag.String("enroll", "", "redeem an enrollment token, then exit")
	enrollSrv := flag.String("enroll-server", "", "hoard server URL for -enroll (e.g. http://truenas:8080)")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	a, err := agent.Load(*cfgPath, *resticBin, *notifyBin, log)
	if err != nil {
		log.Error("load agent config", "err", err)
		os.Exit(1)
	}

	if *enrollTok != "" {
		if *enrollSrv == "" {
			log.Error("-enroll requires -enroll-server")
			os.Exit(2)
		}
		if err := a.Enroll(context.Background(), *enrollSrv, *enrollTok); err != nil {
			log.Error("enrollment failed", "err", err)
			os.Exit(1)
		}
		log.Info("enrolled — configuration written; start the agent normally to back up")
		os.Exit(0)
	}

	// One-shot mode: flags come first because the flag package stops parsing
	// at the first non-flag argument.
	if flag.NArg() > 0 {
		if flag.NArg() != 1 || flag.Arg(0) != "backup" {
			fmt.Fprintln(os.Stderr, "usage: hoard-agent [flags] [backup]")
			os.Exit(2)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err := a.Backup(ctx) // logs its own outcome; reports to the server like the daemon
		stop()
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
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
