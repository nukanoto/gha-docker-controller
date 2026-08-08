// Command gha-docker-controller delegates command handling to internal/cli.
package main

import (
	"os"

	"github.com/nukanoto/gha-docker-controller/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
