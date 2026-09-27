// Package spec reads and writes tent specs: multi-document YAML with one Cluster and its NodeGroups.
package spec

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	sigsjson "sigs.k8s.io/json"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/api/v1alpha1"
)

// Objects are the contents of a spec file: at most one Cluster and any number of NodeGroups.
type Objects struct {
	Cluster    *v1alpha1.Cluster
	NodeGroups []*v1alpha1.NodeGroup
}

// Decode parses multi-document YAML strictly. A document is an object or a list of objects, as tent get -o json
// prints them. Unknown fields, keys in the wrong case, duplicate keys, null keys and values, unknown kinds, other API
// versions and a second Cluster are errors that name the document (1-based) and the item of a list. Empty documents
// are skipped.
func Decode(data []byte) (Objects, error) {
	var d decoder
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for doc := 1; ; doc++ {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return d.objs, nil
		}
		if err != nil {
			return Objects{}, fmt.Errorf("document %d: %w", doc, err)
		}
		root, where := n.Content[0], fmt.Sprintf("document %d", doc)
		switch {
		case root.ShortTag() == "!!null":
			continue // an empty document
		case root.Kind == yaml.SequenceNode:
			for i, item := range root.Content {
				if err := d.add(fmt.Sprintf("%s, item %d", where, i+1), item); err != nil {
					return Objects{}, err
				}
			}
		default:
			if err := d.add(where, root); err != nil {
				return Objects{}, err
			}
		}
	}
}

// decoder collects the objects of a spec file.
type decoder struct {
	objs      Objects
	clusterAt string // where the Cluster is, such as "document 1"
}

// add decodes the object at root, which where names in errors.
func (d *decoder) add(where string, root *yaml.Node) error {
	j, err := toJSON(root)
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	obj, err := decodeObject(where, root, j)
	if err != nil {
		return err
	}
	switch obj := obj.(type) {
	case *v1alpha1.Cluster:
		if d.objs.Cluster != nil {
			return fmt.Errorf("%s (%s): a second Cluster, the first is %s", where, v1alpha1.KindCluster, d.clusterAt)
		}
		d.objs.Cluster, d.clusterAt = obj, where
	case *v1alpha1.NodeGroup:
		d.objs.NodeGroups = append(d.objs.NodeGroups, obj)
	}
	return nil
}

// toJSON converts the node of an object to JSON. It rejects anything but a mapping, null or duplicate keys, and
// null values.
func toJSON(root *yaml.Node) ([]byte, error) {
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("not a mapping")
	}
	if err := checkEntries(root, ""); err != nil {
		return nil, err
	}
	quoteStrings(root)
	y, err := write(root)
	if err != nil {
		return nil, fmt.Errorf("re-encode: %w", err)
	}
	return sigsyaml.YAMLToJSONStrict(y) // strict: a safety net for key collisions that checkEntries does not see
}

// decodeObject decodes the JSON of the object at where into a Cluster or a NodeGroup, strictly and case-sensitively.
// root is the object's node, which gives the lines of the keys that errors name.
func decodeObject(where string, root *yaml.Node, j []byte) (any, error) {
	label := where
	var tm v1alpha1.TypeMeta
	if err := sigsjson.UnmarshalCaseSensitivePreserveInts(j, &tm); err != nil {
		return nil, fmt.Errorf("%s: %w", label, fieldError(root, err))
	}
	var obj any
	switch tm.Kind {
	case "":
		return nil, fmt.Errorf("%s: kind is required", label)
	case v1alpha1.KindCluster:
		obj = new(v1alpha1.Cluster)
	case v1alpha1.KindNodeGroup:
		obj = new(v1alpha1.NodeGroup)
	default:
		return nil, fmt.Errorf("%s: %w", label, specError{keyLine(root, "kind"), fmt.Sprintf(
			"unknown kind %q, want %s or %s", tm.Kind, v1alpha1.KindCluster, v1alpha1.KindNodeGroup)})
	}
	label = fmt.Sprintf("%s (%s)", where, tm.Kind)
	switch tm.APIVersion {
	case "":
		return nil, fmt.Errorf("%s: apiVersion is required", label)
	case v1alpha1.APIVersion:
	default:
		return nil, fmt.Errorf("%s: %w", label, specError{keyLine(root, "apiVersion"), fmt.Sprintf(
			"unsupported apiVersion %q, want %s", tm.APIVersion, v1alpha1.APIVersion)})
	}

	// Strict errors (unknown and duplicate fields) come apart from the main error; report every one, in file order.
	strict, err := sigsjson.UnmarshalStrict(j, obj)
	var errs []specError
	for _, e := range append(strict, err) {
		if e != nil {
			errs = append(errs, fieldError(root, e))
		}
	}
	if len(errs) == 0 {
		return obj, nil
	}
	slices.SortStableFunc(errs, func(a, b specError) int { return cmp.Compare(a.line, b.line) })
	joined := make([]error, len(errs))
	for i, e := range errs {
		joined[i] = fmt.Errorf("%s: %w", label, e)
	}
	return nil, errors.Join(joined...)
}

// specError is an error about a key of a document, with the key's line in the file (0 when the file has no such key).
type specError struct {
	line int
	msg  string
}

func (e specError) Error() string {
	if e.line == 0 {
		return e.msg
	}
	return fmt.Sprintf("line %d: %s", e.line, e.msg)
}

// fieldError turns an error of sigs.k8s.io/json into a specError at the line of the field it names, and a type error
// into plain words: spec.size: must be a whole number, not a string.
func fieldError(root *yaml.Node, err error) specError {
	if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		msg := fmt.Sprintf("%s: must be %s, not %s", te.Field, typeWords(te.Type), valueWords(te.Value))
		return specError{keyLine(root, te.Field), msg}
	}
	if fe, ok := errors.AsType[sigsjson.FieldError](err); ok {
		return specError{keyLine(root, fe.FieldPath()), err.Error()}
	}
	return specError{msg: err.Error()}
}

// typeWords says in plain words what a field of type t holds.
func typeWords(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return "true or false"
	case reflect.Int:
		return "a whole number"
	case reflect.String:
		return "a string"
	case reflect.Slice:
		return "a list"
	case reflect.Map, reflect.Struct:
		return "a mapping"
	default:
		return t.String()
	}
}

// valueWords says in plain words what a JSON value, as sigs.k8s.io/json describes it, is.
func valueWords(value string) string {
	switch value {
	case "string":
		return "a string"
	case "number":
		return "a number"
	case "bool":
		return "a boolean"
	case "array":
		return "a list"
	case "object":
		return "a mapping"
	default:
		return strings.TrimPrefix(value, "number ") // number 3.5 for a whole number
	}
}

// keyLine returns the line of the key at path, keys joined by dots from the document root, or 0 if there is none.
func keyLine(root *yaml.Node, path string) int {
	n, line := root, 0
	for key := range strings.SplitSeq(path, ".") {
		k, v := lookup(n, key)
		if k == nil {
			return 0
		}
		n, line = v, k.Line
	}
	return line
}

// lookup returns the key and value nodes of key in the mapping n, or nils.
func lookup(n *yaml.Node, key string) (k, v *yaml.Node) {
	if n.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i], n.Content[i+1]
		}
	}
	return nil, nil
}

// checkEntries returns an error for the first entry under n, the node at path, whose key is null or repeats a key of
// its mapping, or whose value is null. It checks the file's own nodes, so the line is the file's: sigs.k8s.io/yaml
// sees only a re-encoded copy of the document.
func checkEntries(n *yaml.Node, path string) error {
	switch n.Kind {
	case yaml.MappingNode:
		seen := make(map[string]bool, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, key, v := n.Content[i], n.Content[i], n.Content[i+1]
			if k.Kind == yaml.AliasNode {
				key = k.Alias
			}
			if key.Kind != yaml.ScalarNode {
				continue // sigs.k8s.io/yaml rejects such keys
			}
			if key.ShortTag() == "!!null" {
				return specError{k.Line, "a key must not be null"}
			}
			id := keyValue(key)
			if seen[id] {
				return specError{k.Line, fmt.Sprintf("duplicate key %q", key.Value)}
			}
			seen[id] = true
			field := key.Value
			if path != "" {
				field = path + "." + field
			}
			if err := checkValue(v, k.Line, field); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, v := range n.Content {
			if err := checkValue(v, v.Line, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkValue returns an error if v, the value at path on the given line, is null, and checks the entries under it. A
// null value is almost always a forgotten entry.
func checkValue(v *yaml.Node, line int, path string) error {
	if v.ShortTag() == "!!null" {
		return specError{line, path + ": must not be null"}
	}
	return checkEntries(v, path)
}

// keyValue returns what a scalar key stands for: the text of a string, the value of anything else. So 1 and 0x1, or
// true and True, are one key, as they are in JSON.
func keyValue(k *yaml.Node) string {
	var v any
	if k.ShortTag() != "!!str" && k.Decode(&v) == nil {
		return fmt.Sprint(v)
	}
	return k.Value
}

// quoteStrings double-quotes every string under n, for the copy of a document that Decode gives sigs.k8s.io/yaml.
// Double quotes hold any string, and YAML 1.1 reads them as strings too; it reads a plain yes as a bool.
func quoteStrings(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode && n.ShortTag() == "!!str" {
		n.Style = yaml.DoubleQuotedStyle
	}
	for _, c := range n.Content {
		quoteStrings(c)
	}
}

// Encode writes the objects as multi-document YAML: the Cluster first, then the node groups by name. It skips nil
// node groups.
func Encode(o Objects) ([]byte, error) {
	var objs []any
	if o.Cluster != nil {
		objs = append(objs, o.Cluster)
	}
	groups := slices.DeleteFunc(slices.Clone(o.NodeGroups), func(g *v1alpha1.NodeGroup) bool { return g == nil })
	slices.SortStableFunc(groups, func(a, b *v1alpha1.NodeGroup) int {
		return strings.Compare(a.Metadata.Name, b.Metadata.Name)
	})
	for _, g := range groups {
		objs = append(objs, g)
	}

	docs := make([]*yaml.Node, len(objs))
	for i, obj := range objs {
		n, err := toNode(obj)
		if err != nil {
			return nil, fmt.Errorf("encode %T: %w", obj, err)
		}
		docs[i] = n
	}
	out, err := write(docs...)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return out, nil
}

// toNode converts an object to a YAML node with the struct field order. JSON is YAML, except for the runes that JSON
// writes raw and YAML must escape, so the node keeps the key order json.Marshal writes once they are escaped.
func toNode(obj any) (*yaml.Node, error) {
	j, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var n yaml.Node
	if err := yaml.Unmarshal(escapeForYAML(j), &n); err != nil {
		return nil, err
	}
	restyle(&n)
	return &n, nil
}

// escapeForYAML writes as \uXXXX escapes the runes that json.Marshal writes raw but YAML may not hold raw: DEL, the C1
// controls (NEL, U+0085, is a line break in YAML), U+FFFE and U+FFFF. They occur only inside JSON strings.
func escapeForYAML(j []byte) []byte {
	var b bytes.Buffer
	for _, r := range string(j) {
		if r >= 0x7f && r <= 0x9f || r == 0xfffe || r == 0xffff {
			fmt.Fprintf(&b, `\u%04x`, r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.Bytes()
}

// restyle gives every node under n the style yaml.v3 gives a Go value in place of the flow style and double quotes
// of JSON: block collections, plain scalars, and for strings the style stringStyle picks.
func restyle(n *yaml.Node) {
	n.Style = 0
	if n.Kind == yaml.ScalarNode && n.ShortTag() == "!!str" {
		n.Style = stringStyle(n.Value)
	}
	for _, c := range n.Content {
		restyle(c)
	}
}

// stringStyle returns the style yaml.v3 writes the Go string s in, which quotes words such as yes and 123 that YAML
// 1.1 or 1.2 would read as something else. When s does not read back from that style as the same string, as with a
// literal block that starts with a tab, it returns double quotes, which hold any string.
func stringStyle(s string) yaml.Style {
	var n yaml.Node
	if err := n.Encode(s); err != nil || n.ShortTag() != "!!str" || n.Value != s {
		return yaml.DoubleQuotedStyle
	}
	return n.Style
}

// write encodes nodes as a YAML stream with indent 2. With indent 4, yaml.v3 writes list items that start with a
// space or a line break as literal blocks it cannot read back.
func write(nodes ...*yaml.Node) ([]byte, error) {
	if len(nodes) == 0 {
		return nil, nil // yaml.v3 fails to close a stream without documents
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, n := range nodes {
		if err := enc.Encode(n); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
