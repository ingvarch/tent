package assets

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/channels"
)

// nodeconfigGoldens is where internal/nodeconfig keeps the agent configuration it renders: as <role>_<file>.golden, or
// as <file>.golden when every role that has the file gets the same content.
const nodeconfigGoldens = "../nodeconfig/testdata"

// maxNomadZip bounds the download of a Nomad zip: those of Nomad 2.0.0 and 2.0.7 take 53 to 73 MB.
const maxNomadZip = 128 << 20

// TestNomadConfigValidateOnline runs nomad config validate, of the oldest Nomad the stable channel allows and of the
// one it recommends, on each role's agent configuration as the goldens of internal/nodeconfig hold it, on the joining
// form of a server and a combined node, and on one with a key Nomad does not know.
func TestNomadConfigValidateOnline(t *testing.T) {
	if os.Getenv("TENT_TEST_ONLINE") != "1" {
		t.Skip("downloads Nomad from releases.hashicorp.com; set TENT_TEST_ONLINE=1 to run it")
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if !slices.Contains([]string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"}, platform) {
		t.Skipf("runs Nomad on darwin and linux, amd64 and arm64, not on %s", platform)
	}
	files := nomadAgentGoldens(t)
	ch, err := channels.Load("stable")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	for _, version := range slices.Compact([]string{ch.Nomad.Minimum, ch.Nomad.Recommended}) {
		t.Run(version, func(t *testing.T) {
			nomad := downloadNomad(ctx, t, version)
			for _, role := range v1alpha1.Roles() {
				t.Run(string(role), func(t *testing.T) {
					forms := []bool{false}
					if hasJoiningForm(files[role]) {
						forms = append(forms, true)
					}
					for _, joining := range forms {
						t.Run(fmt.Sprintf("joining=%t", joining), func(t *testing.T) {
							dir := nomadAgentDir(t, nomadAgentForm(files[role], joining))
							out, err := nomadValidate(ctx, nomad, dir)
							if err != nil {
								t.Fatalf("nomad config validate: %v\n%s", err, out)
							}
							t.Logf("nomad config validate:\n%s", out)
						})
					}
				})
			}
			t.Run("unknown key", func(t *testing.T) {
				dir := nomadAgentDir(t, nomadAgentForm(files[v1alpha1.RoleServer], false))
				writeTestFile(t, filepath.Join(dir, "50-unknown.hcl"), "tent_unknown_key = true\n")
				out, err := nomadValidate(ctx, nomad, dir)
				if ee, ok := errors.AsType[*exec.ExitError](err); !ok || ee.ExitCode() < 1 {
					t.Fatalf("nomad config validate = %v, want an exit code above 0\n%s", err, out)
				}
				if !strings.Contains(out, "tent_unknown_key") {
					t.Fatalf("nomad config validate failed without naming tent_unknown_key:\n%s", out)
				}
				t.Logf("nomad config validate: %v\n%s", err, out)
			})
		})
	}
}

func TestNomadAgentFiles(t *testing.T) {
	all := []string{"d/server_00-tent.hcl.golden", "d/client_00-tent.hcl.golden", "d/combined_00-tent.hcl.golden",
		"d/11-instance.hcl.golden"}
	got, err := nomadAgentFiles(all)
	if err != nil {
		t.Fatal(err)
	}
	want := map[v1alpha1.Role]map[string]string{
		v1alpha1.RoleServer: {"00-tent.hcl": "d/server_00-tent.hcl.golden"},
		v1alpha1.RoleClient: {"00-tent.hcl": "d/client_00-tent.hcl.golden", "11-instance.hcl": "d/11-instance.hcl.golden"},
		v1alpha1.RoleCombined: {
			"00-tent.hcl":     "d/combined_00-tent.hcl.golden",
			"11-instance.hcl": "d/11-instance.hcl.golden",
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("nomadAgentFiles (-want +got):\n%s", diff)
	}

	for _, tc := range []struct {
		name    string
		goldens []string
		want    []string
	}{
		{"a golden of no role", append(slices.Clone(all), "d/12-other.hcl.golden"),
			[]string{"d/12-other.hcl.golden", "which roles get it"}},
		{"a role without goldens", all[1:], []string{"no goldens of the server role"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := nomadAgentFiles(tc.goldens)
			wantErr(t, err, tc.want...)
		})
	}

	t.Run("the goldens of nodeconfig", func(t *testing.T) {
		goldens := nomadAgentGoldens(t) // every golden has a place
		for _, role := range v1alpha1.Roles() {
			if got := hasJoiningForm(goldens[role]); got != role.RunsServer() {
				t.Errorf("the %s role has a joining form = %t, want %t", role, got, role.RunsServer())
			}
		}
	})
}

// TestNomadAgentForm checks that the joining form takes 10-node.joining.hcl in place of 10-node.hcl and keeps the other
// files, and that the other form leaves the joining file out.
func TestNomadAgentForm(t *testing.T) {
	files := map[string]string{
		"00-tent.hcl":             "d/server_00-tent.hcl.golden",
		"10-node.hcl":             "d/server_10-node.hcl.golden",
		"10-node.joining.hcl":     "d/server_10-node.joining.hcl.golden",
		"99-user-client.hcl":      "d/client_99.golden",
		"11-instance.joining.hcl": "d/other.golden",
	}
	if !hasJoiningForm(files) || hasJoiningForm(map[string]string{"10-node.hcl": "d/g"}) {
		t.Error("hasJoiningForm does not tell a role with a joining form from one without")
	}
	wantBootstrap := map[string]string{
		"00-tent.hcl":        "d/server_00-tent.hcl.golden",
		"10-node.hcl":        "d/server_10-node.hcl.golden",
		"99-user-client.hcl": "d/client_99.golden",
	}
	if diff := cmp.Diff(wantBootstrap, nomadAgentForm(files, false)); diff != "" {
		t.Errorf("nomadAgentForm(files, false) (-want +got):\n%s", diff)
	}
	wantJoining := map[string]string{
		"00-tent.hcl":        "d/server_00-tent.hcl.golden",
		"10-node.hcl":        "d/server_10-node.joining.hcl.golden",
		"11-instance.hcl":    "d/other.golden",
		"99-user-client.hcl": "d/client_99.golden",
	}
	if diff := cmp.Diff(wantJoining, nomadAgentForm(files, true)); diff != "" {
		t.Errorf("nomadAgentForm(files, true) (-want +got):\n%s", diff)
	}
}

// joiningInfix in the name of a file marks its joining form: the content of a server or combined node that joins
// servers that exist. 10-node.joining.hcl takes the place of 10-node.hcl in that form.
const joiningInfix = ".joining"

// hasJoiningForm reports whether files, the golden of each file name, hold a joining form.
func hasJoiningForm(files map[string]string) bool {
	for name := range files {
		if strings.Contains(name, joiningInfix) {
			return true
		}
	}
	return false
}

// nomadAgentForm returns the files of one form of a role's agent configuration. The joining form is the files without
// the infix, each replaced by its file with the infix where there is one; the other form leaves the latter out.
func nomadAgentForm(files map[string]string, joining bool) map[string]string {
	form := make(map[string]string, len(files))
	for name, golden := range files {
		if !strings.Contains(name, joiningInfix) {
			form[name] = golden
		}
	}
	if joining {
		for name, golden := range files {
			if strings.Contains(name, joiningInfix) {
				form[strings.Replace(name, joiningInfix, "", 1)] = golden
			}
		}
	}
	return form
}

// nomadAgentGoldens returns each role's agent configuration from the goldens of internal/nodeconfig, as
// nomadAgentFiles places them.
func nomadAgentGoldens(t *testing.T) map[v1alpha1.Role]map[string]string {
	t.Helper()
	goldens, err := filepath.Glob(filepath.Join(nodeconfigGoldens, "*.hcl.golden"))
	if err != nil {
		t.Fatal(err)
	}
	files, err := nomadAgentFiles(goldens)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// nomadAgentFiles places the goldens, by path, in each role's /etc/nomad.d: it returns the golden of each file name,
// by role. <role>_<file>.golden belongs to the role, and 11-instance.hcl.golden to the roles that run a client. A
// golden of no role, or a role without goldens, is an error.
func nomadAgentFiles(goldens []string) (map[v1alpha1.Role]map[string]string, error) {
	files := make(map[v1alpha1.Role]map[string]string)
	for _, role := range v1alpha1.Roles() {
		files[role] = make(map[string]string)
	}
	for _, golden := range goldens {
		name := strings.TrimSuffix(filepath.Base(golden), ".golden")
		placed := false
		for _, role := range v1alpha1.Roles() {
			file, ok := strings.CutPrefix(name, string(role)+"_")
			if !ok && name == "11-instance.hcl" && role.RunsClient() {
				file, ok = name, true
			}
			if ok {
				files[role][file] = golden
				placed = true
			}
		}
		if !placed {
			return nil, fmt.Errorf("%s names no role and is not 11-instance.hcl.golden: say which roles get it", golden)
		}
	}
	for _, role := range v1alpha1.Roles() {
		if len(files[role]) == 0 {
			return nil, fmt.Errorf("no goldens of the %s role", role)
		}
	}
	return files, nil
}

// downloadNomad downloads the Nomad version for the system the test runs on, checks the zip against the sha256 of
// HashiCorp's signed SHA256SUMS, and returns the path of the binary in it.
func downloadNomad(ctx context.Context, t *testing.T, version string) string {
	t.Helper()
	var opts Options
	a, err := nomadFor(ctx, opts, version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	data, err := opts.getUpTo(ctx, a.URLs[0], maxNomadZip)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != a.SHA256 {
		t.Fatalf("%s has the sha256 %x, want %s", a.URLs[0], sum, a.SHA256)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read %s: %v", a.URLs[0], err)
	}
	src, err := zr.Open("nomad")
	if err != nil {
		t.Fatalf("read %s: %v", a.URLs[0], err)
	}
	defer func() { _ = src.Close() }()
	bin := filepath.Join(t.TempDir(), "nomad")
	dst, err := os.OpenFile(bin, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("write %s: %v", bin, err)
	}
	t.Logf("nomad %s %s from %s", a.Version, runtime.GOOS+"/"+runtime.GOARCH, a.URLs[0])
	return bin
}

// nomadAgentDir returns a new directory that holds the agent configuration of files, the golden of each file name, as
// /etc/nomad.d does.
func nomadAgentDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, golden := range files {
		writeTestFile(t, filepath.Join(dir, name), readFile(t, golden))
	}
	return dir
}

func writeTestFile(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// nomadValidate runs nomad config validate on dir and returns what it printed.
func nomadValidate(ctx context.Context, nomad, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, nomad, "config", "validate", dir).CombinedOutput()
	return string(out), err
}
