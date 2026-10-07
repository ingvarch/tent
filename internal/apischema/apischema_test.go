package apischema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/invopop/jsonschema"
	validator "github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

const (
	apiDir    = "../../api/v1alpha1"
	schemaURL = "https://raw.githubusercontent.com/ingvarch/tent/main/api/v1alpha1/tent.schema.json"
)

func TestSchemaFileIsUpToDate(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(apiDir, "tent.schema.json"))
	if err != nil {
		t.Fatalf("read the committed schema: %v; run make generate", err)
	}
	if got := generate(t); !bytes.Equal(got, want) {
		t.Errorf("api/v1alpha1/tent.schema.json is out of date, run make generate (-committed +generated):\n%s",
			cmp.Diff(string(want), string(got)))
	}
}

// node is the part of a JSON Schema that the tests read.
type node struct {
	Schema               string           `json:"$schema"`
	ID                   string           `json:"$id"`
	Ref                  string           `json:"$ref"`
	Defs                 map[string]*node `json:"$defs"`
	OneOf                []*node          `json:"oneOf"`
	Type                 string           `json:"type"`
	Const                string           `json:"const"`
	Enum                 []string         `json:"enum"`
	Description          string           `json:"description"`
	Required             []string         `json:"required"`
	Properties           map[string]*node `json:"properties"`
	AdditionalProperties any              `json:"additionalProperties"`
	Items                *node            `json:"items"`
}

func TestSchemaShape(t *testing.T) {
	var s node
	if err := json.Unmarshal(generate(t), &s); err != nil {
		t.Fatalf("parse the schema: %v", err)
	}
	def := func(name string) *node {
		t.Helper()
		d := s.Defs[name]
		if d == nil {
			t.Fatalf("no $defs.%s", name)
		}
		return d
	}
	prop := func(defName, name string) *node {
		t.Helper()
		p := def(defName).Properties[name]
		if p == nil {
			t.Fatalf("no $defs.%s.properties.%s", defName, name)
		}
		return p
	}

	checks := []struct {
		name      string
		want, got any
	}{
		{"$schema", "https://json-schema.org/draft/2020-12/schema", s.Schema},
		{"$id", schemaURL, s.ID},
		{"oneOf", []*node{{Ref: "#/$defs/Cluster"}, {Ref: "#/$defs/NodeGroup"}}, s.OneOf},
		{"Cluster required", []string{"apiVersion", "kind", "metadata", "spec"}, def("Cluster").Required},
		{"NodeGroup required", []string{"apiVersion", "kind", "metadata", "spec"}, def("NodeGroup").Required},
		{"ClusterSpec required", []string{"cloud"}, def("ClusterSpec").Required},
		{"Cluster kind", "Cluster", prop("Cluster", "kind").Const},
		{"NodeGroup kind", "NodeGroup", prop("NodeGroup", "kind").Const},
		{"Cluster apiVersion", "tent/v1alpha1", prop("Cluster", "apiVersion").Const},
		{"NodeGroup apiVersion", "tent/v1alpha1", prop("NodeGroup", "apiVersion").Const},
		{"ClusterSpec additionalProperties", false, def("ClusterSpec").AdditionalProperties},
		{"NodeGroupSpec additionalProperties", false, def("NodeGroupSpec").AdditionalProperties},
		{
			"size description",
			"Size is the number of nodes. A server or combined group has 1, 3 or 5; a client group has 0 or more.",
			prop("NodeGroupSpec", "size").Description,
		},
		{"Cluster metadata description", "Metadata names the cluster.", prop("Cluster", "metadata").Description},
		{
			"NodeGroup metadata description",
			"Metadata names the node group and its cluster.",
			prop("NodeGroup", "metadata").Description,
		},
	}
	for _, c := range checks {
		if diff := cmp.Diff(c.want, c.got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", c.name, diff)
		}
	}

	props := []struct {
		name      string
		want, got *node
	}{
		{"role", &node{Type: "string", Enum: []string{"server", "client", "combined"}}, prop("NodeGroupSpec", "role")},
		{"provider", &node{Type: "string", Enum: []string{"vultr", "hetzner"}}, prop("Cloud", "provider")},
		{
			"clientIntroduction",
			&node{Type: "string", Enum: []string{"strict", "warn", "none"}},
			prop("ClusterNomad", "clientIntroduction"),
		},
		{"access.api", &node{Type: "array", Items: &node{Type: "string"}}, prop("Access", "api")},
		{"rollingUpdate", &node{Ref: "#/$defs/RollingUpdate"}, prop("NodeGroupSpec", "rollingUpdate")},
		{"rollingUpdate.maxSurge", &node{Type: "integer"}, prop("RollingUpdate", "maxSurge")},
		{"rollingUpdate.maxUnavailable", &node{Type: "integer"}, prop("RollingUpdate", "maxUnavailable")},
		{"rollingUpdate.drainTimeout", &node{Type: "string"}, prop("RollingUpdate", "drainTimeout")},
		{
			"nomad.meta",
			&node{Type: "object", AdditionalProperties: map[string]any{"type": "string"}},
			prop("NodeGroupNomad", "meta"),
		},
	}
	for _, p := range props {
		if diff := cmp.Diff(p.want, p.got, cmpopts.IgnoreFields(node{}, "Description")); diff != "" {
			t.Errorf("%s (-want +got):\n%s", p.name, diff)
		}
	}
}

func TestGenerateIndentsByTwoSpacesAndEndsWithANewline(t *testing.T) {
	got := generate(t)
	var compact, want bytes.Buffer
	if err := json.Compact(&compact, got); err != nil {
		t.Fatalf("compact the schema: %v", err)
	}
	if err := json.Indent(&want, compact.Bytes(), "", "  "); err != nil {
		t.Fatalf("indent the schema: %v", err)
	}
	want.WriteByte('\n')
	if diff := cmp.Diff(want.String(), string(got)); diff != "" {
		t.Errorf("Generate() output (-want +got):\n%s", diff)
	}
}

func TestSchemaAcceptsTheExamples(t *testing.T) {
	sch := compile(t)
	docs := map[string]any{}
	// The example and what tent writes for it.
	for _, name := range []string{"../spec/testdata/example.yaml", "../spec/testdata/example.encoded.yaml"} {
		maps.Copy(docs, yamlDocuments(t, name))
	}
	goldens, err := filepath.Glob(filepath.Join(apiDir, "testdata", "*.json"))
	if err != nil || len(goldens) == 0 {
		t.Fatalf("no JSON goldens in %s/testdata: %v", apiDir, err)
	}
	for _, name := range goldens {
		docs[name] = jsonValue(t, readFile(t, name))
	}
	for name, doc := range docs {
		if err := sch.Validate(doc); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSchemaRejectsInvalidDocuments(t *testing.T) {
	sch := compile(t)
	tests := []struct {
		name   string
		change func(spec map[string]any)
		want   string // in the error
	}{
		{"an unknown field", func(spec map[string]any) { spec["replicas"] = 3 }, "'replicas'"},
		{"an unknown role", func(spec map[string]any) { spec["role"] = "master" }, "'/spec/role'"},
		{
			"a string maxSurge",
			func(spec map[string]any) { spec["rollingUpdate"] = map[string]any{"maxSurge": "1"} },
			"'/spec/rollingUpdate/maxSurge'",
		},
		{
			"an unknown rollingUpdate field",
			func(spec map[string]any) { spec["rollingUpdate"] = map[string]any{"maxDrain": 1} },
			"'maxDrain'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := jsonValue(t, readFile(t, filepath.Join(apiDir, "testdata", "nodegroup-servers.json")))
			tt.change(doc.(map[string]any)["spec"].(map[string]any))
			err := sch.Validate(doc)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v, want an error about %s", err, tt.want)
			}
		})
	}
}

func TestGenerateRejectsADirectoryWithoutTheTypes(t *testing.T) {
	onlyCluster := t.TempDir()
	writeFile(t, filepath.Join(onlyCluster, "cluster.go"),
		"package other\n\n// Cluster is something else.\ntype Cluster struct{}\n")
	for _, dir := range []string{t.TempDir(), onlyCluster} {
		if _, err := Generate(dir); err == nil || !strings.Contains(err.Error(), "no doc comment") {
			t.Errorf("Generate(%q) = %v, want an error with %q", dir, err, "no doc comment")
		}
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := Generate(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Generate(%q) = %v, want %v", missing, err, fs.ErrNotExist)
	}
}

func TestGenerateNeedsADocCommentOnEveryField(t *testing.T) {
	const comment = "\t// NodeClass of the clients.\n"
	src := string(readFile(t, filepath.Join(apiDir, "types.go")))
	if !strings.Contains(src, comment) {
		t.Fatalf("types.go has no %q", comment)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "types.go"), strings.Replace(src, comment, "", 1))

	const want = "type NodeGroupNomad: field nodeClass has no doc comment"
	if _, err := Generate(dir); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Generate() = %v, want an error with %q", err, want)
	}
}

func TestReadComments(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "thing.go"), `package v1alpha1

// Thing has two sentences. This is the second.
type Thing struct {
	// Wrapped is a comment that the column limit
	// split over two lines.
	Wrapped string
	// Paragraphs is one paragraph.
	//
	// And another,
	//   indented.
	Paragraphs string
}
`)
	got, err := readComments(dir)
	if err != nil {
		t.Fatalf("readComments: %v", err)
	}
	const pkg = "github.com/ingvarch/tent/api/v1alpha1."
	want := map[string]string{
		pkg + "Thing":            "Thing has two sentences. This is the second.",
		pkg + "Thing.Wrapped":    "Wrapped is a comment that the column limit split over two lines.",
		pkg + "Thing.Paragraphs": "Paragraphs is one paragraph. And another, indented.",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("readComments (-want +got):\n%s", diff)
	}
}

func TestSetTypeMetaNeedsTheProperties(t *testing.T) {
	onlyName := jsonschema.NewProperties()
	onlyName.Set("name", &jsonschema.Schema{Type: "string"})
	tests := map[string]jsonschema.Definitions{
		"no definition":    {},
		"no properties":    {"Cluster": {}},
		"no kind property": {"Cluster": {Properties: onlyName}},
		"a nil definition": {"Cluster": nil},
	}
	for name, defs := range tests {
		t.Run(name, func(t *testing.T) {
			if err := setTypeMeta(defs, "Cluster"); err == nil {
				t.Error("setTypeMeta() = nil error, want one")
			}
		})
	}
}

func generate(t *testing.T) []byte {
	t.Helper()
	b, err := Generate(apiDir)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return b
}

// compile compiles the generated schema with a validator of its own, which also checks it against the 2020-12
// metaschema.
func compile(t *testing.T) *validator.Schema {
	t.Helper()
	c := validator.NewCompiler()
	if err := c.AddResource(schemaURL, jsonValue(t, generate(t))); err != nil {
		t.Fatalf("add the schema: %v", err)
	}
	sch, err := c.Compile(schemaURL)
	if err != nil {
		t.Fatalf("compile the schema: %v", err)
	}
	return sch
}

// yamlDocuments reads every document of a YAML file as a JSON value, keyed by file and document number.
func yamlDocuments(t *testing.T, name string) map[string]any {
	t.Helper()
	docs := map[string]any{}
	dec := yaml.NewDecoder(bytes.NewReader(readFile(t, name)))
	for i := 1; ; i++ {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s document %d: %v", name, i, err)
		}
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s document %d: %v", name, i, err)
		}
		docs[fmt.Sprintf("%s document %d", name, i)] = jsonValue(t, j)
	}
	if len(docs) == 0 {
		t.Fatalf("%s has no documents", name)
	}
	return docs
}

// jsonValue parses JSON the way the validator expects it, with numbers as json.Number.
func jsonValue(t *testing.T, j []byte) any {
	t.Helper()
	v, err := validator.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		t.Fatalf("parse JSON: %v", err)
	}
	return v
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
