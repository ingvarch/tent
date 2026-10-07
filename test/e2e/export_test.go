package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

// exportInfo is what tent export nomad -o json prints.
type exportInfo struct {
	Cluster       string    `json:"cluster"`
	Dir           string    `json:"dir"`
	Address       string    `json:"address"`
	CACert        string    `json:"caCert"`
	ClientCert    string    `json:"clientCert"`
	ClientKey     string    `json:"clientKey"`
	TLSServerName string    `json:"tlsServerName"`
	TokenFile     string    `json:"tokenFile"`
	TokenAccessor string    `json:"tokenAccessor"`
	Expires       time.Time `json:"expires"`
}

// parseExport reads the JSON of tent export nomad. It fails when a value the Nomad client needs is empty.
func parseExport(data []byte) (exportInfo, error) {
	var x exportInfo
	if err := json.Unmarshal(data, &x); err != nil {
		return exportInfo{}, fmt.Errorf("read the export: %w", err)
	}
	for _, v := range []struct{ key, value string }{
		{"address", x.Address}, {"caCert", x.CACert}, {"clientCert", x.ClientCert},
		{"clientKey", x.ClientKey}, {"tlsServerName", x.TLSServerName}, {"tokenFile", x.TokenFile},
	} {
		if v.value == "" {
			return exportInfo{}, fmt.Errorf("the export has no %q", v.key)
		}
	}
	return x, nil
}

// machinesOf returns the instances of a cluster by their tags, only those of one role when role is not empty.
func machinesOf(instances []vultrapi.Instance, cluster, role string) []vultrapi.Instance {
	var out []vultrapi.Instance
	for _, in := range instances {
		if !slices.Contains(in.Tags, "tent/cluster="+cluster) {
			continue
		}
		if role != "" && !slices.Contains(in.Tags, "tent/role="+role) {
			continue
		}
		out = append(out, in)
	}
	return out
}
