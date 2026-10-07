package e2e

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// envOf returns a getenv function over a fixed set of variables.
func envOf(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// requiredEnv holds the four variables the suite cannot run without.
func requiredEnv() map[string]string {
	return map[string]string{
		"VULTR_API_KEY":    "key-value-for-tests",
		"TENT_NODE_URL":    "https://node.example/tent-node",
		"TENT_NODE_SHA256": "0123abcd",
		"E2E_TENT":         "/repo/bin/tent",
	}
}

func TestReadSettingsDefaults(t *testing.T) {
	got, err := readSettings(envOf(requiredEnv()))
	if err != nil {
		t.Fatalf("readSettings: %v", err)
	}
	want := settings{
		Key:     "key-value-for-tests",
		NodeURL: "https://node.example/tent-node",
		NodeSHA: "0123abcd",
		Tent:    "/repo/bin/tent",
		Region:  "ams",
		Plan:    "vc2-1c-1gb",
		Images:  []string{"ubuntu-24.04", "ubuntu-26.04"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("settings = %+v, want %+v", got, want)
	}
}

func TestReadSettingsOverrides(t *testing.T) {
	vars := requiredEnv()
	vars["E2E_REGION"] = "fra"
	vars["E2E_PLAN"] = "vc2-2c-4gb"
	vars["E2E_IMAGES"] = "ubuntu-22.04"
	vars["E2E_KEEP"] = "1"
	vars["RUNNER_ADDR"] = "203.0.113.7"
	got, err := readSettings(envOf(vars))
	if err != nil {
		t.Fatalf("readSettings: %v", err)
	}
	want := settings{
		Key:     "key-value-for-tests",
		NodeURL: "https://node.example/tent-node",
		NodeSHA: "0123abcd",
		Tent:    "/repo/bin/tent",
		Region:  "fra",
		Plan:    "vc2-2c-4gb",
		Images:  []string{"ubuntu-22.04"},
		Keep:    true,
		Runner:  "203.0.113.7",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("settings = %+v, want %+v", got, want)
	}
}

func TestReadSettingsNamesOnlyTheMissingVariablesInOrder(t *testing.T) {
	tests := []struct {
		name    string
		drop    []string
		wantErr string
	}{
		{"all four", []string{"VULTR_API_KEY", "TENT_NODE_URL", "TENT_NODE_SHA256", "E2E_TENT"},
			"set VULTR_API_KEY, TENT_NODE_URL, TENT_NODE_SHA256 and E2E_TENT" + makeE2EHint},
		{"the key and the digest", []string{"VULTR_API_KEY", "TENT_NODE_SHA256"},
			"set VULTR_API_KEY and TENT_NODE_SHA256" + makeE2EHint},
		{"the url and the digest", []string{"TENT_NODE_URL", "TENT_NODE_SHA256"},
			"set TENT_NODE_URL and TENT_NODE_SHA256" + makeE2EHint},
		{"only the url", []string{"TENT_NODE_URL"}, "set TENT_NODE_URL" + makeE2EHint},
		{"only the tent", []string{"E2E_TENT"}, "set E2E_TENT" + makeE2EHint},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := requiredEnv()
			for _, name := range tt.drop {
				delete(vars, name)
			}
			_, err := readSettings(envOf(vars))
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestReadSettingsImages(t *testing.T) {
	tests := []struct {
		name    string
		images  string
		want    []string
		wantErr string
	}{
		{"spaces and empty entries", " ubuntu-24.04 , ,ubuntu-26.04,", []string{"ubuntu-24.04", "ubuntu-26.04"}, ""},
		{"no minor version", "ubuntu-24", nil, `"ubuntu-24"`},
		{"another distribution", "ubuntu-24.04,debian-12", nil, `"debian-12"`},
		{"junk before", "xubuntu-24.04", nil, `"xubuntu-24.04"`},
		{"junk after", "ubuntu-24.04x", nil, `"ubuntu-24.04x"`},
		{"three digits", "ubuntu-240.04", nil, `"ubuntu-240.04"`},
		{"only separators", " , ", nil, "E2E_IMAGES names no image"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := requiredEnv()
			vars["E2E_IMAGES"] = tt.images
			got, err := readSettings(envOf(vars))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one holding %s", err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), "E2E_IMAGES") {
					t.Errorf("error %q does not name E2E_IMAGES", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("readSettings: %v", err)
			}
			if !reflect.DeepEqual(got.Images, tt.want) {
				t.Errorf("images = %q, want %q", got.Images, tt.want)
			}
		})
	}
}

func TestReadSettingsKeep(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  bool
	}{{"", false}, {"1", true}, {"no", true}} {
		vars := requiredEnv()
		vars["E2E_KEEP"] = tt.value
		got, err := readSettings(envOf(vars))
		if err != nil {
			t.Fatalf("E2E_KEEP=%q: %v", tt.value, err)
		}
		if got.Keep != tt.want {
			t.Errorf("E2E_KEEP=%q: Keep = %v, want %v", tt.value, got.Keep, tt.want)
		}
	}
}

func TestSettingsNeverPrintTheKey(t *testing.T) {
	vars := requiredEnv()
	got, err := readSettings(envOf(vars))
	if err != nil {
		t.Fatalf("readSettings: %v", err)
	}
	for _, verb := range []string{"%v", "%+v", "%s"} {
		if text := fmt.Sprintf(verb, got); strings.Contains(text, vars["VULTR_API_KEY"]) {
			t.Errorf("%s prints the key: %s", verb, text)
		}
	}
	vars["E2E_IMAGES"] = "bad"
	if _, err := readSettings(envOf(vars)); err == nil || strings.Contains(err.Error(), vars["VULTR_API_KEY"]) {
		t.Errorf("error %v is missing or holds the key", err)
	}
}

func TestSettingsStringNamesRegionPlanImagesAndKeep(t *testing.T) {
	s := settings{Key: "k", Region: "ams", Plan: "vc2-1c-1gb", Images: []string{"ubuntu-24.04", "ubuntu-26.04"}}
	want := "region ams, plan vc2-1c-1gb, images ubuntu-24.04,ubuntu-26.04, keep false"
	if got := s.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestParseImagesRefusesAnImageNamedTwice(t *testing.T) {
	_, err := parseImages("ubuntu-24.04, ubuntu-26.04,ubuntu-24.04")
	if err == nil || err.Error() != `E2E_IMAGES: "ubuntu-24.04" is named twice` {
		t.Errorf("parseImages error = %v, want the image named twice", err)
	}
}
