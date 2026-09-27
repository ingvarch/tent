package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/internal/spec"
)

// printObject writes obj as JSON or YAML, or calls table for the human format.
func printObject(w io.Writer, format string, obj any, table func(io.Writer) error) error {
	switch format {
	case outputTable:
		return table(w)
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
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

// printSpecs writes objects as spec files are written: YAML documents in field order, or with -o json a list.
func printSpecs(w io.Writer, format string, objs spec.Objects) error {
	if format == outputJSON {
		list := make([]any, 0, len(objs.NodeGroups)+1)
		if objs.Cluster != nil {
			list = append(list, objs.Cluster)
		}
		for _, g := range objs.NodeGroups {
			list = append(list, g)
		}
		return printObject(w, format, list, nil)
	}
	data, err := spec.Encode(objs)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("writing YAML: %w", err)
	}
	return nil
}
