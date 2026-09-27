package cli

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/internal/buildinfo"
)

func TestVersionTable(t *testing.T) {
	code, out, errOut := run(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty", errOut)
	}
	want := "tent " + buildinfo.Get().String() + "\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestVersionJSON(t *testing.T) {
	code, out, errOut := run(t, "version", "-o", "json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty", errOut)
	}
	var got buildinfo.Info
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if diff := cmp.Diff(buildinfo.Get(), got); diff != "" {
		t.Errorf("version mismatch (-want +got):\n%s", diff)
	}
}

func TestVersionTakesTheOutputFromTheConfigFile(t *testing.T) {
	writeConfig(t, "output: json\n")
	code, out, errOut := run(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, errOut)
	}
	var got buildinfo.Info
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
}

func TestVersionYAML(t *testing.T) {
	code, out, errOut := run(t, "version", "-o", "yaml")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty", errOut)
	}
	var got buildinfo.Info
	if err := yaml.UnmarshalStrict([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not YAML with the expected fields: %v\n%s", err, out)
	}
	if diff := cmp.Diff(buildinfo.Get(), got); diff != "" {
		t.Errorf("version mismatch (-want +got):\n%s", diff)
	}
}
