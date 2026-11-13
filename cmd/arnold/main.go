package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/nickdirienzo/arnold/internal/arnold"
)

var version = "dev"

func main() {
	if e := run(os.Args[1:], os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, "arnold:", e)
		os.Exit(1)
	}
}
func value(flagValue, envValue, fallback string) string {
	if flagValue != "" {
		return flagValue
	}
	if envValue != "" {
		return envValue
	}
	return fallback
}
func run(args []string, out io.Writer) error {
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(out, "arnold", version)
		return nil
	}
	if len(args) > 0 && args[0] == "launchd" {
		return renderLaunchd(args[1:], out)
	}
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	flags := flag.NewFlagSet("arnold", flag.ContinueOnError)
	flags.SetOutput(out)
	envFile := flags.String("env-file", ".env", "environment file (existing environment variables take precedence)")
	config := flags.String("config", "", "routing configuration (or ARNOLD_CONFIG)")
	state := flags.String("state", "", "SQLite state path (or ARNOLD_STATE)")
	binary := flags.String("paddock", "", "Paddock executable (or PADDOCK_BIN)")
	interval := flags.Duration("poll-interval", 5*time.Second, "output polling interval")
	flags.Usage = func() {
		fmt.Fprintln(out, "Usage: arnold [run] [options]\n       arnold launchd [options]\n       arnold version\n\nSlack conversations delegated to Paddock/OpenShell.")
		flags.PrintDefaults()
	}
	if e := flags.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *interval <= 0 {
		return fmt.Errorf("poll interval must be positive")
	}
	if *envFile != "" {
		if e := godotenv.Load(*envFile); e != nil && !errors.Is(e, os.ErrNotExist) {
			return fmt.Errorf("could not parse environment file %s", *envFile)
		}
	}
	c, e := arnold.LoadConfig(value(*config, os.Getenv("ARNOLD_CONFIG"), "config/local.json"))
	if e != nil {
		return e
	}
	bot, app := os.Getenv("SLACK_BOT_TOKEN"), os.Getenv("SLACK_APP_TOKEN")
	if bot == "" || app == "" {
		return fmt.Errorf("set SLACK_BOT_TOKEN and SLACK_APP_TOKEN")
	}
	statePath := value(*state, os.Getenv("ARNOLD_STATE"), "state/arnold.sqlite")
	store, e := arnold.OpenStore(statePath)
	if e != nil {
		return e
	}
	defer store.Close()
	log := slog.New(slog.NewTextHandler(out, nil))
	slack := &arnold.Slack{BotToken: bot, AppToken: app, Log: log}
	p := arnold.Paddock{Binary: value(*binary, os.Getenv("PADDOCK_BIN"), "paddock")}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return arnold.Serve(ctx, c, store, p, slack, *interval, log)
}
func renderLaunchd(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("arnold launchd", flag.ContinueOnError)
	flags.SetOutput(out)
	directory := flags.String("directory", ".", "directory containing .env, config/ and state/")
	binary := flags.String("binary", "", "installed Arnold executable (defaults to this binary)")
	if e := flags.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected launchd argument")
	}
	dir, e := filepath.Abs(*directory)
	if e != nil {
		return e
	}
	exe := *binary
	if exe == "" {
		exe, e = os.Executable()
		if e != nil {
			return e
		}
	}
	exe, e = filepath.Abs(exe)
	if e != nil {
		return e
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	esc := html.EscapeString
	path := strings.Join([]string{filepath.Join(home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}, ":")
	_, e = fmt.Fprintf(out, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
 <key>Label</key><string>dev.arnold.slack</string>
 <key>ProgramArguments</key><array><string>%s</string><string>run</string><string>--env-file</string><string>%s</string></array>
 <key>WorkingDirectory</key><string>%s</string>
 <key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string></dict>
 <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
 <key>ThrottleInterval</key><integer>30</integer>
 <key>StandardOutPath</key><string>%s</string>
 <key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, esc(exe), esc(filepath.Join(dir, ".env")), esc(dir), esc(path), esc(filepath.Join(dir, "state", "stdout.log")), esc(filepath.Join(dir, "state", "stderr.log")))
	return e
}
