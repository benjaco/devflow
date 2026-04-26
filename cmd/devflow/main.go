package main

import (
	"fmt"
	"os"

	"github.com/benjaco/devflow/internal/cli"
)

func main() {
	app := cli.New()
	if err := app.Run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
