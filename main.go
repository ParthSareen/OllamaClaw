package main

import (
	"fmt"
	"os"

	"github.com/ParthSareen/OllamaClaw/internal/cli"
)

var (
	version = "0.1.0"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	cli.BuildVersion = version
	cli.BuildCommit = commit
	cli.BuildDate = date
	app := cli.New()
	if err := app.Run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
