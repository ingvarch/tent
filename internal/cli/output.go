package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"sigs.k8s.io/yaml"
)

// printObject writes obj as JSON or YAML, or calls table for the human format.
func printObject(w io.Writer, format string, obj any, table func(io.Writer) error) error {
	switch format {
	case outputTable:
		return table(w)
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(obj); err != nil {
			return fmt.Errorf("encoding JSON: %w", err)
		}
		return nil
	case outputYAML:
		b, err := yaml.Marshal(obj)
		if err != nil {
			return fmt.Errorf("encoding YAML: %w", err)
		}
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("writing YAML: %w", err)
		}
		return nil
	default:
		return invalidOutputError(format)
	}
}
