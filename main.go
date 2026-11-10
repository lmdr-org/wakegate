// Command wakegate wakes a computer when a request needs it and lets it sleep when
// nothing does. See https://github.com/clauer-org/wakegate.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/clauer-org/wakegate/internal/agent"
	"github.com/clauer-org/wakegate/internal/proxy"
	"github.com/clauer-org/wakegate/internal/wol"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `wakegate wakes a computer when a request needs it and lets it sleep when nothing does.

Usage:
  wakegate proxy [-config FILE] [-check]   run on the machine that is always on
  wakegate agent [-config FILE] [-check]   run on the computer that sleeps
  wakegate wake [-broadcast ADDR,...] MAC  send a magic packet, to test Wake-on-LAN
  wakegate version

Documentation: https://github.com/clauer-org/wakegate
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "proxy":
		err = runProxy(args)
	case "agent":
		err = runAgent(args)
	case "wake":
		err = runWake(args)
	case "version", "-version", "--version":
		fmt.Println(version)
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "wakegate: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wakegate:", err)
		os.Exit(1)
	}
}

func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	path := fs.String("config", "/etc/wakegate/proxy.yaml", "configuration file")
	check := fs.Bool("check", false, "check the configuration and exit")
	_ = fs.Parse(args)
	cfg, err := proxy.LoadConfig(*path)
	if err != nil {
		return err
	}
	if *check {
		fmt.Printf("%s: target %s (%s), %s, admin page %s\n", *path, cfg.Target.Name, cfg.Target.Host,
			plural(len(cfg.Services), "service"), orNone(cfg.Admin.Listen))
		for _, s := range cfg.Services {
			mode := "http"
			if s.TCP {
				mode = "tcp"
			}
			fmt.Printf("  %-16s %-4s %-22s -> %s\n", s.Name, mode, s.Listen, s.Addr())
		}
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return proxy.Run(ctx, cfg, version, logger())
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	path := fs.String("config", "/etc/wakegate/agent.yaml", "configuration file")
	check := fs.Bool("check", false, "check the configuration and exit")
	_ = fs.Parse(args)
	cfg, err := agent.LoadConfig(*path)
	if err != nil {
		return err
	}
	if *check {
		fmt.Printf("%s: listen %s, %s after %s without requests; awake while sessions: %s, inhibitors: %v, %s\n",
			*path, cfg.Listen, cfg.Sleep, cfg.IdleTimeout, cfg.KeepAwake.Sessions, *cfg.KeepAwake.Inhibitors,
			plural(len(cfg.KeepAwake.Commands), "command"))
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return agent.Run(ctx, cfg, version, logger())
}

func runWake(args []string) error {
	fs := flag.NewFlagSet("wake", flag.ExitOnError)
	broadcast := fs.String("broadcast", "255.255.255.255", "comma-separated addresses, each with an optional :port (9 by default)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: wakegate wake [-broadcast ADDR,...] MAC")
	}
	addrs := strings.Split(*broadcast, ",")
	if err := wol.Send(fs.Arg(0), addrs, 3, 500*time.Millisecond); err != nil {
		return err
	}
	fmt.Printf("magic packet for %s sent to %s\n", fs.Arg(0), strings.Join(addrs, ", "))
	return nil
}

// logger writes text logs on stderr. Under systemd, journald adds the time itself.
func logger() *slog.Logger {
	opts := &slog.HandlerOptions{}
	if os.Getenv("JOURNAL_STREAM") != "" {
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}
	var w io.Writer = os.Stderr
	return slog.New(slog.NewTextHandler(w, opts))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func orNone(s string) string {
	if s == "" {
		return "off"
	}
	return "on " + s
}
