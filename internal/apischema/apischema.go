// Package apischema generates the JSON Schema of the tent API types, for editors that check spec files.
package apischema

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/invopop/jsonschema"

	"github.com/ingvarch/tent/api/v1alpha1"
)

// schemaID is the $id of the schema and the URL editors load it from.
const schemaID = "https://raw.githubusercontent.com/ingvarch/tent/main/api/v1alpha1/tent.schema.json"

// Generate returns the JSON Schema of the Cluster and NodeGroup kinds. apiDir is the directory of the v1alpha1 Go
// files, whose doc comments become the descriptions.
func Generate(apiDir string) ([]byte, error) {
	comments, err := readComments(apiDir)
	if err != nil {
		return nil, err
	}
	r := &jsonschema.Reflector{Mapper: enums, CommentMap: comments}

	defs := jsonschema.Definitions{}
	root := &jsonschema.Schema{Version: jsonschema.Version, ID: schemaID, Definitions: defs}
	kinds := []struct {
		kind string
		obj  any
	}{{v1alpha1.KindCluster, v1alpha1.Cluster{}}, {v1alpha1.KindNodeGroup, v1alpha1.NodeGroup{}}}
	for _, k := range kinds {
		s := r.Reflect(k.obj)
		maps.Copy(defs, s.Definitions) // all types are in one package, so a name is one type
		if err := setTypeMeta(defs, k.kind); err != nil {
			return nil, err
		}
		root.OneOf = append(root.OneOf, &jsonschema.Schema{Ref: s.Ref})
	}
	if err := checkDescriptions(defs); err != nil {
		return nil, fmt.Errorf("%s: %w", apiDir, err)
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the schema: %w", err)
	}
	return append(out, '\n'), nil
}

// checkDescriptions returns an error for the first type or field, in name order, without a description: editors
// show them, and a missing one means a doc comment is missing or the comments came from another directory.
func checkDescriptions(defs jsonschema.Definitions) error {
	for _, name := range slices.Sorted(maps.Keys(defs)) {
		if defs[name].Description == "" {
			return fmt.Errorf("type %s has no doc comment", name)
		}
		for field, p := range defs[name].Properties.FromOldest() {
			if p.Description == "" {
				return fmt.Errorf("type %s: field %s has no doc comment", name, field)
			}
		}
	}
	return nil
}

// setTypeMeta gives kind and apiVersion in the definition of kind the only value each may have.
func setTypeMeta(defs jsonschema.Definitions, kind string) error {
	def := defs[kind]
	if def == nil || def.Properties == nil {
		return fmt.Errorf("no object definition of %s", kind)
	}
	for _, c := range []struct{ prop, value string }{{"kind", kind}, {"apiVersion", v1alpha1.APIVersion}} {
		p, ok := def.Properties.Get(c.prop)
		if !ok {
			return fmt.Errorf("%s has no %s property", kind, c.prop)
		}
		p.Const = c.value
	}
	return nil
}

// readComments returns the doc comments of the Go files in apiDir, keyed as the Reflector looks them up, each on one
// line.
func readComments(apiDir string) (map[string]string, error) {
	var r jsonschema.Reflector
	if err := r.AddGoComments("", apiDir, jsonschema.WithFullComment()); err != nil {
		return nil, fmt.Errorf("read the doc comments in %s: %w", apiDir, err)
	}
	// The prefix mirrors how AddGoComments keys a comment: by the directory its file was walked in, not by the import
	// path the Reflector looks up.
	dir := filepath.Clean(apiDir) + "."
	pkg := reflect.TypeFor[v1alpha1.Cluster]().PkgPath() + "."
	comments := make(map[string]string, len(r.CommentMap))
	for key, text := range r.CommentMap {
		if name, ok := strings.CutPrefix(key, dir); ok {
			comments[pkg+name] = strings.Join(strings.Fields(text), " ")
		}
	}
	return comments, nil
}

// enums maps the string types with a fixed set of values to enums of those values.
func enums(t reflect.Type) *jsonschema.Schema {
	switch t {
	case reflect.TypeFor[v1alpha1.Role]():
		return stringEnum(v1alpha1.Roles())
	case reflect.TypeFor[v1alpha1.Provider]():
		return stringEnum(v1alpha1.Providers())
	case reflect.TypeFor[v1alpha1.ClientIntroduction]():
		return stringEnum(v1alpha1.ClientIntroductions())
	}
	return nil
}

func stringEnum[T ~string](values []T) *jsonschema.Schema {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = string(v)
	}
	return &jsonschema.Schema{Type: "string", Enum: enum}
}
