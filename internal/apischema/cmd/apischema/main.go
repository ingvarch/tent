// Command apischema writes the JSON Schema of the tent API types to a file.
//
//	apischema -api api/v1alpha1 -out api/v1alpha1/tent.schema.json
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ingvarch/tent/internal/apischema"
)

func main() {
	apiDir := flag.String("api", "", "directory of the v1alpha1 Go files")
	out := flag.String("out", "", "file to write the schema to")
	flag.Parse()
	if *apiDir == "" || *out == "" || flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*apiDir, *out); err != nil {
		fmt.Fprintln(os.Stderr, "apischema:", err)
		os.Exit(1)
	}
}

func run(apiDir, out string) error {
	schema, err := apischema.Generate(apiDir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, schema, 0o644); err != nil {
		return fmt.Errorf("write the schema: %w", err)
	}
	return nil
}
