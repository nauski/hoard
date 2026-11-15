// Command hoard-agent is the desktop backup client. It serves a local web GUI
// where you choose what to back up and when, and pushes those backups to a
// hoard server's restic REST endpoint. Run it as a per-user service; open the
// GUI at http://127.0.0.1:7420. `hoard-agent [flags] backup` instead runs one
// backup with the saved config and exits (for systemd timers and cron).
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nauski/hoard/internal/agent"
	"github.com/nauski/hoard/internal/restic"
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

	// A distro-packaged restic can be far behind. Backups work on old versions,
	// but restores do not, and that must not be discovered on the day of a restore.
	if maj, min, err := restic.Version(context.Background(), *resticBin); err != nil {
		log.Warn("cannot determine restic version", "err", err)
	} else if maj == 0 && min < 17 {
		log.Warn("restic is older than 0.17: backups work, but restore from this agent needs 0.17 or newer; install a current restic (e.g. `restic self-update` or the release binary) and point -restic at it",
			"version", fmt.Sprintf("%d.%d", maj, min))
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

	// One-shot subcommands. Global flags come first because the flag package
	// stops parsing at the first non-flag argument.
	if flag.NArg() > 0 {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err := runCommand(ctx, a, flag.Args())
		stop()
		if errors.Is(err, errUsage) {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
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

const usage = `usage: hoard-agent [flags]                       run the GUI and scheduler
       hoard-agent [flags] backup [-pin NOTE]    back up once and exit; -pin keeps the snapshot past retention
       hoard-agent [flags] find PATTERN          search this host's backups by file name ("wt-perf", "*.jsonl")
       hoard-agent [flags] restore -path PATH [-before TIME] [-to DIR] [-inplace]
                                                 restore PATH from the newest backup that has it
                                                 (default destination: PATH.restored-<timestamp>)
`

var errUsage = errors.New("usage")

// runCommand executes one subcommand with the saved agent config.
func runCommand(ctx context.Context, a *agent.Agent, args []string) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch args[0] {
	case "backup":
		pin := fs.String("pin", "", "")
		if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
			return errUsage
		}
		return a.BackupPin(ctx, *pin) // logs its own outcome; reports to the server like the daemon
	case "find":
		if fs.Parse(args[1:]) != nil || fs.NArg() != 1 {
			return errUsage
		}
		found, err := a.Find(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		if len(found) == 0 {
			fmt.Println("no match in any backup")
		}
		const stamp = "2006-01-02 15:04"
		for _, f := range found {
			state := "still present"
			if f.GoneTime != nil {
				state = "MISSING since " + f.GoneTime.Local().Format(stamp)
			}
			fmt.Printf("%s\n    %s, in %d backups, last in %s (%s), %s\n",
				f.Path, f.Type, f.Versions, f.LastID, f.LastTime.Local().Format(stamp), state)
		}
		return nil
	case "restore":
		p := fs.String("path", "", "")
		beforeS := fs.String("before", "", "")
		to := fs.String("to", "", "")
		inplace := fs.Bool("inplace", false, "")
		if fs.Parse(args[1:]) != nil || fs.NArg() != 0 || *p == "" {
			return errUsage
		}
		var before time.Time
		if *beforeS != "" {
			var err error
			if before, err = parseWhen(*beforeS); err != nil {
				return err
			}
		}
		dest, err := a.RestorePath(ctx, *p, before, *to, *inplace)
		if err != nil {
			return err
		}
		fmt.Println("restored to", dest)
		return nil
	}
	return errUsage
}

// parseWhen accepts RFC 3339 or local "2006-01-02 15:04" / "2006-01-02".
func parseWhen(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read time %q: use 2026-11-15 14:00, 2026-11-15, or RFC 3339", s)
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
