// Command tent provisions and operates HashiCorp Nomad clusters.
package main

import (
	"os"

	"github.com/ingvarch/tent/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], cli.Streams{Out: os.Stdout, Err: os.Stderr}))
}
