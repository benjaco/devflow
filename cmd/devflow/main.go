package main

import (
	"os"

	"github.com/benjaco/devflow/internal/cli"
)

func main() {
	app := cli.New()
	if err := app.Run(os.Args[1:]); err != nil {
		cli.ReportError(os.Stderr, err)
		os.Exit(cli.ExitCode(err))
	}
}
