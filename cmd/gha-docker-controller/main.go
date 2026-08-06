// Command gha-docker-controller is the daemon with three subcommands:
// serve, check, and version. Implementation lives in internal/cli; main only
// passes cli.Run's return value to os.Exit (testability). cli is designed not
// to panic, so there is no recover path here.
package main

import (
	"os"

	"github.com/nukanoto/gha-docker-controller/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
