package assets

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/channels"
)

func TestCNI(t *testing.T) {
	ch := &channels.Channel{Name: "test", CNI: channels.CNI{
		Version: "1.2.3",
		SHA256:  map[string]string{"amd64": strings.Repeat("a", 64), "arm64": strings.Repeat("b", 64)},
	}}
	for arch, sum := range map[string]string{"amd64": "a", "arm64": "b"} {
		got, err := CNI(ch, arch)
		if err != nil {
			t.Fatal(err)
		}
		want := Asset{
			Name:    "cni-plugins",
			Version: "1.2.3",
			URLs: []string{"https://github.com/containernetworking/plugins/releases/download/v1.2.3/cni-plugins-linux-" +
				arch + "-v1.2.3.tgz"},
			SHA256: strings.Repeat(sum, 64),
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("CNI(%s) (-want +got):\n%s", arch, diff)
		}
	}

	_, err := CNI(ch, "riscv64")
	wantErr(t, err, `channel test has no CNI plugins for riscv64`)
}

func TestCNIOfStable(t *testing.T) {
	ch, err := channels.Load("stable")
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		a, err := CNI(ch, arch)
		if err != nil {
			t.Fatal(err)
		}
		if a.Version != ch.CNI.Version || a.SHA256 != ch.CNI.SHA256[arch] || len(a.URLs) != 1 {
			t.Errorf("CNI(stable, %s) = %+v, want version %s and sha256 %s", arch, a, ch.CNI.Version,
				ch.CNI.SHA256[arch])
		}
	}
}
