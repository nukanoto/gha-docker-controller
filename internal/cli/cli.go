// Package cli implements the serve, check, and version commands.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/nukanoto/gha-docker-controller/internal/app"
	"github.com/nukanoto/gha-docker-controller/internal/buildinfo"
	"github.com/nukanoto/gha-docker-controller/internal/config"
)

// DefaultConfigPath is used when --config is omitted.
const DefaultConfigPath = "/etc/gha-docker-controller/config.yaml"

// Exit codes are part of the systemd and monitoring contract.
const (
	// ExitOK indicates success.
	ExitOK = 0
	// ExitError indicates a runtime failure.
	ExitError = 1
	// ExitUsage indicates invalid command-line usage.
	ExitUsage = 2
)

// Run parses the command line, runs a subcommand, and returns its exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return ExitUsage
	}
	if args[0] == "--config" || strings.HasPrefix(args[0], "--config=") {
		path := strings.TrimPrefix(args[0], "--config=")
		if args[0] == "--config" {
			if len(args) < 2 || args[1] == "" {
				fmt.Fprintln(stderr, "--config requires a path")
				printUsage(stderr)
				return ExitUsage
			}
			path = args[1]
			args = args[2:]
		} else {
			args = args[1:]
		}
		if path == "" {
			fmt.Fprintln(stderr, "--config requires a path")
			printUsage(stderr)
			return ExitUsage
		}
		if len(args) == 0 {
			fmt.Fprintln(stderr, "--config must be followed by a command")
			printUsage(stderr)
			return ExitUsage
		}
		args = append([]string{args[0], "--config", path}, args[1:]...)
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "-h", "--help":
		printUsage(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		printUsage(stderr)
		return ExitUsage
	}
}

// runServe loads configuration and delegates lifecycle management to app.Serve.
func runServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printServeUsage(fs) }
	cfgPath := addConfigFlag(fs)
	if code, ok := parseArgs(fs, args); !ok {
		return code
	}
	cfg, warnings, err := config.Load(*cfgPath)
	if err != nil {
		// Logger settings are unavailable when loading the config fails.
		fmt.Fprintf(stderr, "load config: %v\n", err)
		return ExitError
	}
	logger := newLogger(cfg.Log.Format, cfg.Log.Level, stderr)
	logWarnings(logger, warnings)
	logger.Info("serve starting", "config", *cfgPath, "version", buildinfo.Version, "commit", buildinfo.Commit)
	if err := app.Serve(cfg, buildinfo.Version, buildinfo.Commit, logger); err != nil {
		logger.Error("serve failed", "error", err)
		return ExitError
	}
	return ExitOK
}

// runCheck loads configuration and delegates validation to app.Check.
func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printCheckUsage(fs) }
	cfgPath := addConfigFlag(fs)
	if code, ok := parseArgs(fs, args); !ok {
		return code
	}
	cfg, warnings, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config: %v\n", err)
		return ExitError
	}
	logger := newLogger(cfg.Log.Format, cfg.Log.Level, stderr)
	logWarnings(logger, warnings)
	if err := app.Check(cfg, buildinfo.Version, buildinfo.Commit, logger); err != nil {
		logger.Error("check failed", "error", err)
		return ExitError
	}
	return ExitOK
}

// runVersion prints build information to stdout.
func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printVersionUsage(fs) }
	if code, ok := parseArgs(fs, args); !ok {
		return code
	}
	fmt.Fprintf(stdout, "gha-docker-controller %s\n", buildinfo.Version)
	fmt.Fprintf(stdout, "commit: %s\n", buildinfo.Commit)
	fmt.Fprintf(stdout, "build date: %s\n", buildinfo.Date)
	fmt.Fprintf(stdout, "go version: %s\n", buildinfo.GoVersion())
	return ExitOK
}

// addConfigFlag defines the shared --config flag.
func addConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", DefaultConfigPath, "設定 file の path (既定: "+DefaultConfigPath+")")
}

// parseArgs parses subcommand flags and rejects extra arguments.
func parseArgs(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK, false
		}
		return ExitUsage, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		fs.Usage()
		return ExitUsage, false
	}
	return 0, true
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: gha-docker-controller <command> [options]

Commands:
  serve     デーモンを起動し、設定された Scale Set の runner を需要に応じて管理する
  check     接続先が serve に必要な契約を満たすかを read-only で確認する
  version   build 情報 (version/commit/build date/Go version) を出力する

Run "gha-docker-controller <command> -h" for details.
`)
}

func printVersionUsage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), "Usage: gha-docker-controller version\n\nversion は build 情報 (version/commit/build date/Go version) を出力する。\n")
}

func printServeUsage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), `Usage: gha-docker-controller serve [options]

serve はデーモンを起動し、設定された Scale Set の runner を作成・削除する。
SIGINT/SIGTERM で graceful shutdown する。設定の詳細は config.example.yaml を
参照すること。

`)
	fs.PrintDefaults()
}

func printCheckUsage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), `Usage: gha-docker-controller check [options]

check は設定の接続先が serve に必要な契約を満たすかを確認する。
container、runner、Scale Set、network は作成・削除しない。ただし完全な
read-only ではない点が 1 つある。

  1. pull policy に応じて image を pull するため、Docker image store を
     変更することがある。

`)
	fs.PrintDefaults()
}

// newLogger builds a logger for the configured format and level.
func newLogger(format, level string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slogLevel(level)}
	var handler slog.Handler
	if format == config.LogFormatText {
		handler = slog.NewTextHandler(w, opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}
	return slog.New(handler)
}

// slogLevel converts a configuration level to slog.Level.
func slogLevel(level string) slog.Level {
	switch level {
	case config.LogLevelDebug:
		return slog.LevelDebug
	case config.LogLevelWarn:
		return slog.LevelWarn
	case config.LogLevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// logWarnings writes non-secret configuration warnings.
func logWarnings(logger *slog.Logger, warnings []config.Warning) {
	for _, w := range warnings {
		logger.Warn("config warning", "path", w.Path, "warning", w.Message)
	}
}
