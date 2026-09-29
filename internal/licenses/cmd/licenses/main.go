// Command licenses fails when a module the packages link, on any platform the release builds for, has a licence
// tent does not allow. With -notices it also writes the third-party notices the release ships.
//
//	licenses [-notices THIRD_PARTY_NOTICES] ./cmd/tent ./cmd/tent-node
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/ingvarch/tent/internal/licenses"
)

func main() {
	notices := flag.String("notices", "", "file to write the third-party notices to")
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(context.Background(), *notices, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "licenses:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, notices string, patterns []string) error {
	mods, err := licenses.Linked(ctx, licenses.Platforms, patterns...)
	if err != nil {
		return err
	}
	if err := licenses.Check(mods, licenses.Allowed); err != nil {
		return err
	}
	if notices == "" {
		return nil
	}
	if err := os.WriteFile(notices, licenses.Notices(mods), 0o644); err != nil {
		return fmt.Errorf("write the notices: %w", err)
	}
	return nil
}
