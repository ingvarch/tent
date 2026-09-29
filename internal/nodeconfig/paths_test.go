package nodeconfig_test

import (
	"path"
	"testing"

	"github.com/hashicorp/hcl"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
)

// TestAgentFilePaths reads 00-tent.hcl back with HCL1 and checks that it names the files where the exported paths
// say: the TLS files in the tls block, and the intro token in the client's state directory, which Nomad keeps in the
// data directory.
func TestAgentFilePaths(t *testing.T) {
	for _, role := range v1alpha1.Roles() {
		var got struct {
			DataDir string `hcl:"data_dir"`
			TLS     *struct {
				CAFile   string `hcl:"ca_file"`
				CertFile string `hcl:"cert_file"`
				KeyFile  string `hcl:"key_file"`
			} `hcl:"tls"`
		}
		if err := hcl.Decode(&got, string(render(t, role)[0].Content)); err != nil {
			t.Fatalf("HCL1 cannot read 00-tent.hcl of %s: %v", role, err)
		}
		if got.TLS == nil || got.TLS.CAFile != nodeconfig.CAFile || got.TLS.CertFile != nodeconfig.CertFile ||
			got.TLS.KeyFile != nodeconfig.KeyFile {
			t.Errorf("00-tent.hcl of %s: tls = %+v, want the files %s, %s and %s", role, got.TLS,
				nodeconfig.CAFile, nodeconfig.CertFile, nodeconfig.KeyFile)
		}
		if intro := path.Join(got.DataDir, "client", "intro_token.jwt"); intro != nodeconfig.IntroTokenFile {
			t.Errorf("00-tent.hcl of %s: the intro token is at %s, not at IntroTokenFile %s", role, intro,
				nodeconfig.IntroTokenFile)
		}
	}
}
