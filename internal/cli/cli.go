// Package cli provides the three gha-docker-controller subcommands
// (serve/check/version). It uses the standard flag package and handles
// config loading, logger setup, app invocation, and exit code decisions.
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

// DefaultConfigPath is the default config file path when --config is not
// given.
const DefaultConfigPath = "/etc/gha-docker-controller/config.yaml"

// Exit codes. success is 0, runtime error is 1, usage error is 2. These
// values are a contract interpreted by the systemd unit and monitoring.
const (
	// ExitOK is the success exit code.
	ExitOK = 0
	// ExitError is the runtime error exit code (config load failure,
	// connection failure, fatal, etc.).
	ExitError = 1
	// ExitUsage is the exit code for command line errors (unknown command,
	// flag error, extra arguments).
	ExitUsage = 2
)

// Run parses the command line, runs serve/check/version, and returns the
// exit code. main only passes the return value to os.Exit; this function is
// the only I/O path. stdout is for version and explicit help output, stderr
// for logs and errors. An unknown or missing command prints usage to stderr
// and returns ExitUsage.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		// No command: print usage as an error and exit with a usage error.
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
		// An explicit help request is a success: print usage to stdout.
		printUsage(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		printUsage(stderr)
		return ExitUsage
	}
}

// runServe runs the serve subcommand: load config, set up the logger, and
// delegate to app.Serve. SIGINT/SIGTERM receipt and graceful shutdown are
// handled by app.Serve's internal signal.NotifyContext, so signals are not
// registered twice here; only delegation happens.
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
		// The logger depends on the config format/level, so a config load
		// failure is printed as plain text to stderr. Per config's contract
		// the error contains no secrets.
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

// runCheck runs the check subcommand: load config, set up the logger, and
// delegate to app.Check. check is read-only, but depending on the pull
// policy an image pull can change the Docker image store, so the help states
// this. The help also states that dind-runner runtimeArgs cannot be
// verified.
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

// runVersion runs the version subcommand. It prints
// version/commit/build date/Go version to stdout with fixed keys. Even
// without ldflags, buildinfo returns explicit values ("dev"/"unknown"), so
// all items are always printed.
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

// addConfigFlag defines the --config flag shared by serve/check. The flag
// can only appear after the command (before the command it would be an
// unknown command).
func addConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", DefaultConfigPath, "設定 file の path (既定: "+DefaultConfigPath+")")
}

// parseArgs parses a subcommand's flags with the standard flag package. On a
// parse error or extra arguments, the flag package or this function has
// already printed usage to stderr; the code to exit with is returned
// (ok=false). ErrHelp (-h/--help) counts as success after usage is printed
// (ExitOK). Only a successful parse proceeds with ok=true.
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

// printUsage prints the command list usage.
func printUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: gha-docker-controller <command> [options]

Commands:
  serve     デーモンを起動し、設定された Scale Set の runner を需要に応じて管理する
  check     接続先が serve に必要な契約を満たすかを read-only で確認する
  version   build 情報 (version/commit/build date/Go version) を出力する

Run "gha-docker-controller <command> -h" for details.
`)
}

// printVersionUsage prints the version description. It has no flags.
func printVersionUsage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), "Usage: gha-docker-controller version\n\nversion は build 情報 (version/commit/build date/Go version) を出力する。\n")
}

// printServeUsage prints the serve flags and description.
func printServeUsage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), `Usage: gha-docker-controller serve [options]

serve はデーモンを起動し、設定された Scale Set の runner を作成・削除する。
SIGINT/SIGTERM で graceful shutdown する。設定の詳細は config.example.yaml を
参照すること。

`)
	fs.PrintDefaults()
}

// printCheckUsage prints the check flags and description. It states the two
// ways check is not fully read-only: the Docker image store change from
// image pulls, and the non-verification of dind-runner runtimeArgs.
func printCheckUsage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), `Usage: gha-docker-controller check [options]

check は設定の接続先が serve に必要な契約を満たすかを確認する。
container、runner、Scale Set、network は作成・削除しない。ただし完全な
read-only ではない点が 2 つある。

  1. pull policy に応じて image を pull するため、Docker image store を
     変更することがある。
  2. dind-runner profile の runtimeArgs (--net-raw など) は公式 Docker
     API で introspection できないため検証しない。host 側の設定は operator
     が daemon.json を目視確認し、check は常に warning を出す。

`)
	fs.PrintDefaults()
}

// newLogger builds a slog logger with the configured format/level. The
// production default is JSON; text is chosen only when explicitly
// configured. level is limited to the 4 values debug/info/warn/error
// (config's static validation already guarantees this) and Source is not
// added. Log output is fixed to stderr.
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

// slogLevel converts a config level name to slog.Level. Config's static
// validation guarantees the 4 values (debug/info/warn/error), but unknown
// values defensively fall back to info.
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

// logWarnings logs config.Load warnings with fixed fields. Per config's
// contract warnings contain no secrets; they only have path and message.
func logWarnings(logger *slog.Logger, warnings []config.Warning) {
	for _, w := range warnings {
		logger.Warn("config warning", "path", w.Path, "warning", w.Message)
	}
}
