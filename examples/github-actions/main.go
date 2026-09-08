package main

import (
	"os"

	"github.com/benjaco/devflow/internal/cli"
)

// This entrypoint keeps the demo buildable. Adapter bootstrap selects
// devflow.project.go and supplies its own generated main.go.
func main() {
	app := cli.New()
	if err := app.Run(os.Args[1:]); err != nil {
		cli.ReportError(os.Stderr, err)
		os.Exit(cli.ExitCode(err))
	}
}
