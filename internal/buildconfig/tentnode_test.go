package buildconfig_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/licenses"
)

// tentNodeMain is the main package of tent-node, the agent that runs on every node.
const tentNodeMain = "github.com/ingvarch/tent/cmd/tent-node"

// tentNodeDenied are the packages that tent-node must never link, with their subpackages, and why.
var tentNodeDenied = []struct{ pkg, why string }{
	{"github.com/ingvarch/tent/internal/cloud", "no code that can create or delete cloud resources runs on a node"},
	{"github.com/ingvarch/tent/internal/assets", "tent-node gets its assets in NodeConfig and carries no PGP code"},
	{"github.com/ingvarch/tent/internal/channels", "tent-node reads versions from NodeConfig"},
	{"github.com/ingvarch/tent/internal/statestore", "a node never reaches the state store"},
	{"github.com/ingvarch/tent/internal/app", "the use cases of the CLI never run on a node"},
	{"github.com/ingvarch/tent/internal/nomadops", "the operator's Nomad client never runs on a node"},
	{"github.com/vultr/govultr", "cloud SDKs never run on a node"},
	{"github.com/hetznercloud/hcloud-go", "cloud SDKs never run on a node"},
	{"github.com/aws", "cloud SDKs never run on a node"},
	{"github.com/ProtonMail/go-crypto", "tent-node carries no PGP code"},
	{"github.com/hashicorp/nomad", "tent-node runs the Nomad binary and imports no Nomad module"},
	{"github.com/spf13/cobra", "tent-node parses its flags with the standard library"},
}

// tentNodeModules are the only modules besides the standard library whose packages tent-node may link.
var tentNodeModules = []string{"github.com/ingvarch/tent", "golang.org/x/mod"}

// within reports whether pkg is prefix or one of its subpackages.
func within(pkg, prefix string) bool { return pkg == prefix || strings.HasPrefix(pkg, prefix+"/") }

// deniedWhy returns why tent-node must not link pkg, or "" when the deny list does not name it.
func deniedWhy(pkg string) string {
	for _, d := range tentNodeDenied {
		if within(pkg, d.pkg) {
			return d.why
		}
	}
	return ""
}

func TestTentNodeLinksOnlyNodeCode(t *testing.T) {
	// depguard checks the imports of each file; this checks everything the binary links, however it gets there.
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run("linux/"+arch, func(t *testing.T) {
			pkgs, err := licenses.List(t.Context(), "linux/"+arch, tentNodeMain)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(pkgs, func(p licenses.ListedPackage) bool { return p.ImportPath == tentNodeMain }) {
				t.Fatalf("go list does not list %s", tentNodeMain)
			}
			for _, p := range pkgs {
				if p.Standard {
					continue
				}
				var module string
				if p.Module != nil {
					module = p.Module.Path
				}
				// A denied package is reported once, with the reason; its module is not reported again.
				if why := deniedWhy(p.ImportPath); why != "" {
					t.Errorf("tent-node links %s: %s", p.ImportPath, why)
				} else if !slices.Contains(tentNodeModules, module) {
					t.Errorf("tent-node links %s from the module %q; it may link only the standard library and %s",
						p.ImportPath, module, strings.Join(tentNodeModules, " and "))
				}
			}
		})
	}
}
