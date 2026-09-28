package assets

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// A checksums.txt as GoReleaser writes it, with made-up sums. The last two lines name files whose names hold
// tent-node_linux_arm64.
const (
	sumAMD64  = "1111111111111111111111111111111111111111111111111111111111111111"
	sumARM64  = "2222222222222222222222222222222222222222222222222222222222222222"
	checksums = "3333333333333333333333333333333333333333333333333333333333333333  tent_0.3.0_linux_amd64.tar.gz\n" +
		sumAMD64 + "  tent-node_linux_amd64\n" +
		sumARM64 + "  tent-node_linux_arm64\n" +
		"4444444444444444444444444444444444444444444444444444444444444444  tent-node_linux_arm64.sbom.json\n" +
		"5555555555555555555555555555555555555555555555555555555555555555  x_tent-node_linux_arm64\n"
)

func TestTentNodeOfARelease(t *testing.T) {
	for _, version := range []string{"v0.3.0", "v0.3.0-rc.1"} {
		for arch, sum := range map[string]string{"amd64": sumAMD64, "arm64": sumARM64} {
			for _, slash := range []string{"", "/"} {
				srv := serve(t, map[string]string{"/" + version + "/checksums.txt": checksums})
				opts := Options{Client: srv.Client(), tentURL: srv.URL + slash}
				got, err := TentNode(t.Context(), opts, version, arch)
				if err != nil {
					t.Fatal(err)
				}
				want := Asset{
					Name:    "tent-node",
					Version: version,
					URLs:    []string{srv.URL + "/" + version + "/tent-node_linux_" + arch},
					SHA256:  sum,
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("TentNode(%s, %s) from %q (-want +got):\n%s", version, arch, opts.tentURL, diff)
				}
			}
		}
	}
}

func TestTentNodeDefaultURL(t *testing.T) {
	const release = "https://github.com/ingvarch/tent/releases/download/v0.3.0/"
	client := &http.Client{Transport: sites{release + "checksums.txt": checksums}}
	got, err := TentNode(t.Context(), Options{Client: client}, "v0.3.0", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	want := Asset{Name: "tent-node", Version: "v0.3.0", URLs: []string{release + "tent-node_linux_arm64"}, SHA256: sumARM64}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("TentNode (-want +got):\n%s", diff)
	}
}

func TestTentNodeChecksumsFail(t *testing.T) {
	const path = "/v0.3.0/checksums.txt"
	line2 := strings.SplitAfter(checksums, "\n")[1]
	for _, tc := range []struct {
		name, checksums string
		want            string
	}{
		{"no line for the arch", strings.Replace(checksums, sumARM64+"  tent-node_linux_arm64\n", "", 1),
			"has no line for tent-node_linux_arm64"},
		{"listed twice", checksums + sumARM64 + "  tent-node_linux_arm64\n", "lists tent-node_linux_arm64 twice"},
		{"one space", strings.Replace(checksums, sumARM64+"  ", sumARM64+" ", 1), "line 3: want a sha256"},
		{"no name", strings.Replace(checksums, "tent-node_linux_amd64", "", 1), "line 2: want a sha256"},
		{"a word", checksums + "garbage\n", "line 6: want a sha256"},
		{"blank line", strings.Replace(checksums, line2, "\n"+line2, 1), "line 2: want a sha256"},
		{"empty", "", "line 1: want a sha256"},
		{"short sha256", strings.Replace(checksums, sumARM64, sumARM64[1:], 1), "line 3: want a sha256"},
		{"upper-case sha256", strings.Replace(checksums, sumARM64, strings.Repeat("A", 64), 1),
			"line 3: want a sha256"},
		{"not hex", strings.Replace(checksums, sumARM64, strings.Repeat("g", 64), 1), "line 3: want a sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, map[string]string{path: tc.checksums})
			a, err := TentNode(t.Context(), Options{Client: srv.Client(), tentURL: srv.URL}, "v0.3.0", "arm64")
			wantErr(t, err, srv.URL+path, tc.want)
			if a.SHA256 != "" {
				t.Errorf("TentNode failed and returned %+v", a)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		srv := serve(t, nil)
		_, err := TentNode(t.Context(), Options{Client: srv.Client(), tentURL: srv.URL}, "v0.3.0", "arm64")
		wantErr(t, err, srv.URL+path, "404")
	})
}

// devVersions are versions of development builds: no release has their tent-node.
var devVersions = []string{"dev", "abc1234", "v0.3.0-SNAPSHOT-48dde55", "v0.3.0-4-gabc1234", "v0.3.0-dirty", ""}

func TestTentNodeOfADevelopmentBuild(t *testing.T) {
	// A development build must not read a release's checksums.txt.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("TentNode of a development build read %s", r.URL)
	}))
	t.Cleanup(srv.Close)
	const devURL = "https://dev.example.invalid/tent-node?sig=x"
	sum := strings.Repeat("c", 64)

	for _, version := range devVersions {
		opts := Options{Client: srv.Client(), tentURL: srv.URL, DevURL: devURL, DevSHA256: sum}
		got, err := TentNode(t.Context(), opts, version, "amd64")
		if err != nil {
			t.Fatalf("TentNode(%q): %v", version, err)
		}
		want := Asset{Name: "tent-node", Version: version, URLs: []string{devURL}, SHA256: sum}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("TentNode(%q) (-want +got):\n%s", version, diff)
		}
	}
}

func TestTentNodeOfADevelopmentBuildFails(t *testing.T) {
	sum := strings.Repeat("c", 64)
	for _, tc := range []struct {
		name string
		opts Options
		want []string
	}{
		{"no variables", Options{}, []string{"no release holds its tent-node", "TENT_NODE_URL", "TENT_NODE_SHA256"}},
		{"no sha256", Options{DevURL: "https://dev.example.invalid/tent-node"},
			[]string{"no release holds its tent-node", "TENT_NODE_URL", "TENT_NODE_SHA256"}},
		{"no URL", Options{DevSHA256: sum}, []string{"no release holds its tent-node", "TENT_NODE_URL", "TENT_NODE_SHA256"}},
		{"short sha256", Options{DevURL: "https://dev.example.invalid/tent-node", DevSHA256: sum[1:]},
			[]string{"TENT_NODE_SHA256", sum[1:], "64 lower-case hex digits"}},
		{"upper-case sha256", Options{DevURL: "https://dev.example.invalid/tent-node", DevSHA256: strings.ToUpper(sum)},
			[]string{"TENT_NODE_SHA256", "64 lower-case hex digits"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, version := range devVersions {
				_, err := TentNode(t.Context(), tc.opts, version, "amd64")
				wantErr(t, err, tc.want...)
			}
		})
	}
}

func TestTentNodeNamesTheDevelopmentBuild(t *testing.T) {
	for version, want := range map[string]string{
		"dev":               "tent dev is a development build, so no release holds its tent-node",
		"v0.3.0-4-gabc1234": "tent v0.3.0-4-gabc1234 is a development build, so no release holds its tent-node",
		"":                  "this tent has no version, so no release holds its tent-node",
	} {
		_, err := TentNode(t.Context(), Options{}, version, "amd64")
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("TentNode(%q) = %v, want an error that starts %q", version, err, want)
		}
	}
}
