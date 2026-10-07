package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

const exportJSON = `{
  "cluster": "e2e-ab12cd-2404",
  "dir": "/r/export",
  "address": "https://203.0.113.9:4646",
  "caCert": "/r/export/ca.pem",
  "clientCert": "/r/export/client.pem",
  "clientKey": "/r/export/client.key",
  "tlsServerName": "server.global.nomad",
  "tokenFile": "/r/export/token",
  "tokenAccessor": "acc-1",
  "expires": "2026-10-08T10:00:00Z"
}`

func TestParseExportReadsEveryKey(t *testing.T) {
	got, err := parseExport([]byte(exportJSON))
	if err != nil {
		t.Fatalf("parseExport: %v", err)
	}
	want := exportInfo{
		Cluster:       "e2e-ab12cd-2404",
		Dir:           "/r/export",
		Address:       "https://203.0.113.9:4646",
		CACert:        "/r/export/ca.pem",
		ClientCert:    "/r/export/client.pem",
		ClientKey:     "/r/export/client.key",
		TLSServerName: "server.global.nomad",
		TokenFile:     "/r/export/token",
		TokenAccessor: "acc-1",
		Expires:       time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC),
	}
	if !got.Expires.Equal(want.Expires) {
		t.Errorf("expires = %v, want %v", got.Expires, want.Expires)
	}
	got.Expires = want.Expires
	if got != want {
		t.Errorf("exportInfo = %+v, want %+v", got, want)
	}
}

func TestParseExportRequiresEveryKeyTheClientNeeds(t *testing.T) {
	for _, key := range []string{"address", "caCert", "clientCert", "clientKey", "tlsServerName", "tokenFile"} {
		t.Run(key, func(t *testing.T) {
			var fields map[string]any
			if err := json.Unmarshal([]byte(exportJSON), &fields); err != nil {
				t.Fatalf("decode the sample: %v", err)
			}
			fields[key] = ""
			text, err := json.Marshal(fields)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			_, err = parseExport(text)
			if err == nil || !strings.Contains(err.Error(), `"`+key+`"`) {
				t.Errorf("error = %v, want one naming %q", err, key)
			}
		})
	}
}

func TestParseExportRejectsTextThatIsNotJSON(t *testing.T) {
	if _, err := parseExport([]byte("no clusters")); err == nil {
		t.Error("parseExport accepted text that is not JSON")
	}
}

func TestMachinesOfPicksByClusterAndRole(t *testing.T) {
	instances := []vultrapi.Instance{
		{ID: "1", Tags: []string{"tent/cluster=a", "tent/role=server"}},
		{ID: "2", Tags: []string{"tent/role=client", "tent/cluster=a"}},
		{ID: "3", Tags: []string{"tent/cluster=b", "tent/role=server"}},
		{ID: "4", Tags: []string{"tent/cluster=ab", "tent/role=server"}},
		{ID: "5", Tags: []string{"tent/role=server"}},
		{ID: "6"},
	}
	ids := func(list []vultrapi.Instance) string {
		var out []string
		for _, in := range list {
			out = append(out, in.ID)
		}
		return strings.Join(out, ",")
	}
	for _, tt := range []struct{ cluster, role, want string }{
		{"a", "", "1,2"},
		{"a", "server", "1"},
		{"a", "client", "2"},
		{"a", "combined", ""},
		{"b", "server", "3"},
		{"c", "", ""},
	} {
		if got := ids(machinesOf(instances, tt.cluster, tt.role)); got != tt.want {
			t.Errorf("machinesOf(%q, %q) = %q, want %q", tt.cluster, tt.role, got, tt.want)
		}
	}
}
