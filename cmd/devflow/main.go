package main

import (
	"fmt"
	"os"

	_ "devflow/examples/bikecoach"
	_ "devflow/examples/go-next-monorepo"
	"devflow/internal/cli"
)

func main() {
	app := cli.New()
	if err := app.Run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
