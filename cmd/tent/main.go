// Command tent provisions and operates HashiCorp Nomad clusters.
package main

import (
	"context"
	"os"

	"github.com/ingvarch/tent/internal/cli"
)

// main runs tent; cli.Execute handles Ctrl-C and SIGTERM.
func main() {
	os.Exit(cli.Execute(context.Background(), os.Args[1:], cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}))
}
