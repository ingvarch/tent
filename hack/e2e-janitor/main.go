// Command e2e-janitor lists and deletes the objects that earlier E2E runs left in the Vultr account.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/ingvarch/tent/test/e2e/janitor"
	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, newAPI)
	stop()
	os.Exit(code)
}

func newAPI(key string) janitor.API { return vultrapi.New(key) }

// Exit codes of the command.
const (
	exitError = 1 // a list or a delete failed, or the input is wrong
	exitUsage = 2 // the command line cannot be parsed
)

// run lists the objects of the E2E clusters that are older than --older-than and, with --yes, deletes them. Without
// --yes it deletes nothing.
func run(
	ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer,
	newAPI func(key string) janitor.API,
) int {
	flags := flag.NewFlagSet("e2e-janitor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	olderThan := flags.Duration("older-than", 3*time.Hour, "act on the clusters whose oldest object is older than this")
	yes := flags.Bool("yes", false, "delete; without it the command only lists what it would delete")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if *olderThan < 0 {
		return fail(stderr, errors.New("--older-than must not be negative"))
	}
	key := getenv("VULTR_API_KEY")
	if key == "" {
		return fail(stderr, errors.New("VULTR_API_KEY is not set"))
	}

	j := janitor.New(newAPI(key), stdout)
	objects, err := j.Find(ctx, *olderThan, time.Now())
	if err != nil {
		return fail(stderr, err)
	}
	if len(objects) == 0 {
		_, _ = fmt.Fprintf(stdout, "nothing older than %s\n", *olderThan)
		return 0
	}
	clusters := map[string]bool{}
	for _, o := range objects {
		clusters[o.Cluster] = true
	}
	if !*yes {
		for _, o := range objects {
			_, _ = fmt.Fprintln(stdout, o)
		}
		_, _ = fmt.Fprintf(stdout, "would delete %d objects of %d clusters older than %s; run again with --yes\n",
			len(objects), len(clusters), *olderThan)
		return 0
	}
	if err := j.Sweep(ctx, objects); err != nil {
		return fail(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "deleted %d objects of %d clusters\n", len(objects), len(clusters))
	return 0
}

func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "Error: %v\n", err)
	return exitError
}
