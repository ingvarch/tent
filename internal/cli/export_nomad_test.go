package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/shellenv/shellenvtest"
	"github.com/ingvarch/tent/internal/statestore"
)

// exportFiles are the files that export nomad writes, in the order it writes them.
var exportFiles = []string{"ca.pem", "cli.pem", "cli-key.pem", "token"}

// exportEnv points the home directory and the cache directory at new temporary directories, so that no test reaches
// the real ones, and makes sh the login shell. It returns the home directory.
func exportEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // the home directory on Windows
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("SHELL", "/bin/sh")
	return home
}

// exportArgs returns the arguments that export the access of the test cluster in the store s to dir, followed by more.
func exportArgs(s state, dir string, more ...string) []string {
	return append([]string{"export", "nomad", "prod", "--state", s.url, "--dir", dir}, more...)
}

// runExport executes tent with args against the Vultr fake f and a new Nomad world, which it returns.
func runExport(t *testing.T, f *vultrfake.Fake, args ...string) (result, *nomadfake.Fake) {
	t.Helper()
	nomad, factory := nomadWorld("servers", "workers", 6)
	return runWithNomad(t, onVultr(f), factory, args...), nomad
}

// exported returns the files of dir by name, and fails the test when dir holds another file.
func exported(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			files[e.Name()] = "" // a directory in the place of a file
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(data)
	}
	return files
}

// wantExported fails the test unless dir holds exactly the four files of an export.
func wantExported(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := exported(t, dir)
	for _, name := range exportFiles {
		if files[name] == "" {
			t.Errorf("%s holds no %s or an empty one", dir, name)
		}
	}
	if len(files) != len(exportFiles) {
		t.Errorf("%s holds %d files, want %d", dir, len(files), len(exportFiles))
	}
	return files
}

// wantMode fails the test unless the file or directory at path has the permission bits mode. Windows has no such
// bits, so it checks nothing there.
func wantMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != mode {
		t.Errorf("%s has mode %o, want %o", path, got, mode)
	}
}

// tokenServer returns the address of the server that the fake's only CreateToken call reached.
func tokenServer(t *testing.T, nomad *nomadfake.Fake) string {
	t.Helper()
	var servers []string
	for _, c := range nomad.Calls() {
		if c.Name == "CreateToken" {
			servers = append(servers, c.Server)
		}
	}
	if len(servers) != 1 {
		t.Fatalf("Nomad got CreateToken calls to %v, want one", servers)
	}
	return servers[0]
}

// shLines are the lines that export nomad prints for sh, for the server at addr and the files in dir.
func shLines(addr, dir string) string {
	return fmt.Sprintf(`export NOMAD_ADDR='https://%[1]s'
export NOMAD_CACERT='%[2]s'
export NOMAD_CLIENT_CERT='%[3]s'
export NOMAD_CLIENT_KEY='%[4]s'
export NOMAD_TLS_SERVER_NAME='server.global.nomad'
export NOMAD_TOKEN="$(cat '%[5]s')"
`, addr, filepath.Join(dir, "ca.pem"), filepath.Join(dir, "cli.pem"), filepath.Join(dir, "cli-key.pem"),
		filepath.Join(dir, "token"))
}

// fishLines are the lines that export nomad prints for fish, for the server at addr and the files in dir. A backslash
// of a path, which a Windows path has, is written twice: inside single quotes fish reads two as one.
func fishLines(addr, dir string) string {
	file := func(name string) string { return strings.ReplaceAll(filepath.Join(dir, name), `\`, `\\`) }
	return fmt.Sprintf(`set -gx NOMAD_ADDR 'https://%[1]s'
set -gx NOMAD_CACERT '%[2]s'
set -gx NOMAD_CLIENT_CERT '%[3]s'
set -gx NOMAD_CLIENT_KEY '%[4]s'
set -gx NOMAD_TLS_SERVER_NAME 'server.global.nomad'
set -gx NOMAD_TOKEN (cat '%[5]s')
`, addr, file("ca.pem"), file("cli.pem"), file("cli-key.pem"), file("token"))
}

// wroteNotice matches what export nomad tells on stderr for the test cluster, which a bubble starts at
// 2000-01-01 00:00 UTC and builds within minutes: the directory, the end a day later and the token's accessor.
var wroteNotice = regexp.MustCompile(
	`^wrote the Nomad access of cluster prod to (.+); it works until 2000-01-02 00:\d\d:\d\d UTC ` +
		`\(token accessor ([0-9a-f-]{36})\)\n$`)

// TestExportNomadPrintsTheLinesForSh writes the four files and prints the six lines for sh on stdout, and the notice
// with the end of the access and the token's accessor on stderr.
func TestExportNomadPrintsTheLinesForSh(t *testing.T) {
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)

		got, nomad := runExport(t, f, exportArgs(s, dir)...)

		if got.code != 0 || got.out != shLines(tokenServer(t, nomad), dir) {
			t.Errorf("exit code %d, stdout\n%s\nwant\n%s", got.code, got.out, shLines(tokenServer(t, nomad), dir))
		}
		m := wroteNotice.FindStringSubmatch(got.errOut)
		if m == nil {
			t.Fatalf("stderr = %q, want the notice that matches %s", got.errOut, wroteNotice)
		}
		if m[1] != dir {
			t.Errorf("the notice names the directory %q, want %q", m[1], dir)
		}
		if issued := nomad.Issued(); len(issued) != 1 || issued[0].Accessor != m[2] {
			t.Errorf("the notice names the accessor %s, Nomad issued %+v", m[2], issued)
		}
		wantExported(t, dir)
	})
}

// TestExportNomadPrintsTheLinesForFish prints the lines for fish when --shell says so.
func TestExportNomadPrintsTheLinesForFish(t *testing.T) {
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)

		got, nomad := runExport(t, f, exportArgs(s, dir, "--shell", "fish")...)

		if got.code != 0 || got.out != fishLines(tokenServer(t, nomad), dir) {
			t.Errorf("exit code %d, stdout\n%s\nwant\n%s", got.code, got.out, fishLines(tokenServer(t, nomad), dir))
		}
	})
}

// TestExportNomadShellFromTheLogin takes the shell from $SHELL when --shell is not given, and lets the flag win.
func TestExportNomadShellFromTheLogin(t *testing.T) {
	for _, tc := range []struct {
		name, login string
		args        []string
		fish        bool
	}{
		{"fish login", "/usr/bin/fish", nil, true},
		{"zsh login", "/bin/zsh", nil, false},
		{"fish login, sh flag", "/usr/bin/fish", []string{"--shell", "sh"}, false},
		{"zsh login, fish flag", "/bin/zsh", []string{"--shell", "fish"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exportEnv(t)
			t.Setenv("SHELL", tc.login)
			dir := filepath.Join(t.TempDir(), "access")
			synctest.Test(t, func(t *testing.T) {
				s, f := builtCluster(t)

				got, nomad := runExport(t, f, exportArgs(s, dir, tc.args...)...)

				want := shLines(tokenServer(t, nomad), dir)
				if tc.fish {
					want = fishLines(tokenServer(t, nomad), dir)
				}
				if got.code != 0 || got.out != want {
					t.Errorf("exit code %d, stdout\n%s\nwant\n%s", got.code, got.out, want)
				}
			})
		})
	}
}

// TestExportNomadRefusesAnUnknownShell fails before it makes the directory or asks Nomad for a token.
func TestExportNomadRefusesAnUnknownShell(t *testing.T) {
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)

		got, nomad := runExport(t, f, exportArgs(s, dir, "--shell", "bash")...)

		wantError(t, got, "Error: invalid --shell \"bash\": want sh or fish\n")
		wantNothingExported(t, dir, nomad)
	})
}

// wantNothingExported fails the test unless dir does not exist and Nomad got no call.
func wantNothingExported(t *testing.T, dir string, nomad *nomadfake.Fake) {
	t.Helper()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory was made: stat %s: %v", dir, err)
	}
	if calls := nomad.Calls(); len(calls) != 0 {
		t.Errorf("Nomad got the calls %v", calls)
	}
}

// TestExportNomadLinesSetTheVariablesInARealShell runs the printed lines in sh and in fish: they set the six
// variables, and NOMAD_TOKEN reads the token file.
func TestExportNomadLinesSetTheVariablesInARealShell(t *testing.T) {
	for _, shell := range shellenvtest.Shells {
		t.Run(shell, func(t *testing.T) {
			exportEnv(t)
			dir := filepath.Join(t.TempDir(), "it's a dir")
			synctest.Test(t, func(t *testing.T) {
				s, f := builtCluster(t)
				got, nomad := runExport(t, f, exportArgs(s, dir, "--shell", shell)...)
				if got.code != 0 {
					t.Fatalf("exit code %d\n%s", got.code, got.errOut)
				}
				names := []string{"NOMAD_ADDR", "NOMAD_CACERT", "NOMAD_CLIENT_CERT", "NOMAD_CLIENT_KEY",
					"NOMAD_TLS_SERVER_NAME", "NOMAD_TOKEN"}

				out := shellenvtest.Run(t, shell, got.out+shellenvtest.Print(shell, names...))

				want := []string{"https://" + tokenServer(t, nomad), filepath.Join(dir, "ca.pem"),
					filepath.Join(dir, "cli.pem"), filepath.Join(dir, "cli-key.pem"), "server.global.nomad",
					exported(t, dir)["token"]}
				if diff := cmp.Diff(want, strings.Split(strings.TrimSuffix(out, "\n"), "\n")); diff != "" {
					t.Errorf("the variables (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// TestExportNomadFiles checks what each file holds: the stored CA bundle, a client certificate of the cluster's CA
// that pairs with the key, and a token of one line that is not the bootstrap secret. Modes are 0700 and 0600.
func TestExportNomadFiles(t *testing.T) {
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		before := s.objects(t)

		got, nomad := runExport(t, f, exportArgs(s, dir)...)

		if got.code != 0 {
			t.Fatalf("exit code %d\n%s", got.code, got.errOut)
		}
		files := wantExported(t, dir)
		if diff := cmp.Diff(before["prod/pki/ca-bundle.pem"], files["ca.pem"]); diff != "" {
			t.Errorf("ca.pem (-want +got):\n%s", diff)
		}
		wantClientCertificate(t, files)
		token := files["token"]
		if strings.ContainsAny(token, "\r\n") || len(token) != 36 {
			t.Errorf("the token file holds %d bytes with a line end or not 36, want one UUID without a line end", len(token))
		}
		if token == before["prod/secrets/acl-bootstrap-token"] {
			t.Error("the token file holds the bootstrap secret")
		}
		if issued := nomad.Issued(); len(issued) != 1 || issued[0].TTL.Hours() != 24 {
			t.Errorf("Nomad issued %+v, want one token of 24h", issued)
		}
		wantMode(t, dir, 0o700)
		for _, name := range exportFiles {
			wantMode(t, filepath.Join(dir, name), 0o600)
		}
		s.want(t, before)
	})
}

// wantClientCertificate fails the test unless cli.pem is a certificate of the CA in ca.pem for cli.global.nomad, for
// client authentication only, that is a pair with the key in cli-key.pem.
func wantClientCertificate(t *testing.T, files map[string]string) {
	t.Helper()
	if _, err := tls.X509KeyPair([]byte(files["cli.pem"]), []byte(files["cli-key.pem"])); err != nil {
		t.Errorf("cli.pem and cli-key.pem are no pair: %v", err)
	}
	block, _ := pem.Decode([]byte(files["cli.pem"]))
	if block == nil {
		t.Fatal("cli.pem holds no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(files["ca.pem"])) {
		t.Fatal("ca.pem holds no certificate")
	}
	_, err = cert.Verify(x509.VerifyOptions{
		Roots: roots, CurrentTime: cert.NotBefore.Add(10 * time.Minute),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		t.Errorf("cli.pem does not verify against ca.pem: %v", err)
	}
	if cert.Subject.CommonName != "cli.global.nomad" || !slices.Equal(cert.ExtKeyUsage, []x509.ExtKeyUsage{
		x509.ExtKeyUsageClientAuth,
	}) {
		t.Errorf("cli.pem is for %q with the usages %v, want cli.global.nomad for client authentication only",
			cert.Subject.CommonName, cert.ExtKeyUsage)
	}
}

// TestExportNomadKeepsTheModeOfAnExistingDirectory leaves a directory that exists as it is, and writes 0600 files.
func TestExportNomadKeepsTheModeOfAnExistingDirectory(t *testing.T) {
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // whatever the umask says
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)

		got, _ := runExport(t, f, exportArgs(s, dir)...)

		if got.code != 0 {
			t.Fatalf("exit code %d\n%s", got.code, got.errOut)
		}
		wantMode(t, dir, 0o755)
		for _, name := range exportFiles {
			wantMode(t, filepath.Join(dir, name), 0o600)
		}
	})
}

// TestExportNomadReplacesTheFilesOfAnEarlierRun gives the second run's files in the place of the first run's, with no
// temporary file left.
func TestExportNomadReplacesTheFilesOfAnEarlierRun(t *testing.T) {
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		if got, _ := runExport(t, f, exportArgs(s, dir)...); got.code != 0 {
			t.Fatalf("first run: exit code %d\n%s", got.code, got.errOut)
		}
		first := wantExported(t, dir)

		got, _ := runExport(t, f, exportArgs(s, dir)...)

		if got.code != 0 {
			t.Fatalf("second run: exit code %d\n%s", got.code, got.errOut)
		}
		second := wantExported(t, dir)
		if second["ca.pem"] != first["ca.pem"] {
			t.Error("the second run changed the CA bundle")
		}
		for _, name := range []string{"cli.pem", "cli-key.pem", "token"} {
			if second[name] == first[name] {
				t.Errorf("the second run left the %s of the first", name)
			}
		}
		wantMode(t, filepath.Join(dir, "token"), 0o600)
	})
}

// TestExportNomadReplacesAFileWithAWiderMode gives each file the mode 0600 when a file with a wider mode is in its
// place, and renames the new file over the old one: another name of the old token file keeps the old content.
func TestExportNomadReplacesAFileWithAWiderMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no permission bits")
	}
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range exportFiles {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("old "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil { // whatever the umask says
			t.Fatal(err)
		}
	}
	kept := filepath.Join(t.TempDir(), "token")
	if err := os.Link(filepath.Join(dir, "token"), kept); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)

		got, _ := runExport(t, f, exportArgs(s, dir)...)

		if got.code != 0 {
			t.Fatalf("exit code %d\n%s", got.code, got.errOut)
		}
		files := wantExported(t, dir)
		for _, name := range exportFiles {
			wantMode(t, filepath.Join(dir, name), 0o600)
			if files[name] == "old "+name {
				t.Errorf("%s was not replaced", name)
			}
		}
		if old, err := os.ReadFile(kept); err != nil || string(old) != "old token" {
			t.Errorf("the old token file holds %d bytes (%v), want its old content: the new file is renamed over it",
				len(old), err)
		}
	})
}

// TestExportNomadFailedRenameLeavesTheFilesBeforeIt stops at the first file it cannot rename into place, names its
// final path, replaces no file after it, leaves the files before it replaced and no temporary file.
func TestExportNomadFailedRenameLeavesTheFilesBeforeIt(t *testing.T) {
	for i, blocked := range exportFiles {
		t.Run(blocked, func(t *testing.T) {
			exportEnv(t)
			dir := filepath.Join(t.TempDir(), "access")
			if err := os.MkdirAll(filepath.Join(dir, blocked, "in the way"), 0o700); err != nil { // a directory in its place
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				s, f := builtCluster(t)

				got, _ := runExport(t, f, exportArgs(s, dir)...)

				wantPrefix := "Error: write " + filepath.Join(dir, blocked) + ": "
				if got.code != 1 || got.out != "" || !strings.HasPrefix(got.errOut, wantPrefix) {
					t.Errorf("exit code %d, stdout %q, stderr %q, want 1, nothing and the error of the write", got.code,
						got.out, got.errOut)
				}
				want := append(slices.Clone(exportFiles[:i]), blocked)
				var names []string
				for name := range exported(t, dir) {
					names = append(names, name)
				}
				slices.Sort(names)
				slices.Sort(want)
				if !slices.Equal(names, want) {
					t.Errorf("%s holds %v, want the files before %s and the directory in its place: %v", dir, names, blocked, want)
				}
			})
		})
	}
}

// TestWriteFilesKeepsTheEarlierRunWhenALaterFileCannotBeWritten fails on the third file, whose temporary file cannot
// be created, and leaves the four files of the earlier run as they were, with no temporary file.
func TestWriteFilesKeepsTheEarlierRunWhenALaterFileCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	var files []exportFile
	old := map[string]string{}
	for _, name := range exportFiles {
		path := filepath.Join(dir, name)
		old[name] = "old " + name
		if err := os.WriteFile(path, []byte(old[name]), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, exportFile{path, []byte("new " + name)})
	}
	files[2].path = filepath.Join(dir, "no such directory", exportKeyFile)

	err := writeFiles(files)

	if want := "write " + files[2].path + ": "; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error = %v, want it to start with %q", err, want)
	}
	if diff := cmp.Diff(old, exported(t, dir)); diff != "" {
		t.Errorf("%s (-earlier run +now):\n%s", dir, diff)
	}
}

// TestExportNomadMakesTheDirectoryBeforeAskingNomad fails when the directory cannot be made, and Nomad got no call.
func TestExportNomadMakesTheDirectoryBeforeAskingNomad(t *testing.T) {
	exportEnv(t)
	file := writeFile(t, "a file", "")
	dir := filepath.Join(file, "access") // below a file
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)

		got, nomad := runExport(t, f, exportArgs(s, dir)...)

		if got.code != 1 || got.out != "" || !strings.HasPrefix(got.errOut, "Error: create the directory "+dir+": ") {
			t.Errorf("exit code %d, stdout %q, stderr %q, want 1, nothing and the error of the directory", got.code,
				got.out, got.errOut)
		}
		if calls := nomad.Calls(); len(calls) != 0 || len(nomad.Issued()) != 0 {
			t.Errorf("Nomad got the calls %v and issued %v", calls, nomad.Issued())
		}
	})
}

// TestExportNomadWritesNothingWhenTheAccessFails passes on the error of the use case and leaves no directory that
// this run made, though the directory is made before the access is: the cluster was never built.
func TestExportNomadWritesNothingWhenTheAccessFails(t *testing.T) {
	exportEnv(t)
	parent := filepath.Join(t.TempDir(), "new parent")
	dir := filepath.Join(parent, "access")
	s := withCluster(t)

	got, _ := runExport(t, vultrfake.New(), exportArgs(s, dir)...)

	if got.code != 1 || got.out != "" || !strings.HasPrefix(got.errOut, "Error: cluster prod: the state store lacks ") {
		t.Errorf("exit code %d, stdout %q, stderr %q, want 1, nothing and the error of the access", got.code, got.out,
			got.errOut)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory that this run made is still there: stat %s: %v", dir, err)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Errorf("the parent of the directory is gone, but this run removes only the directory: %v", err)
	}
}

// TestExportNomadKeepsADirectoryThatExistedWhenTheAccessFails leaves an empty directory that existed, and one with a
// file, as they were.
func TestExportNomadKeepsADirectoryThatExistedWhenTheAccessFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		file bool
	}{{"empty", false}, {"with a file", true}} {
		t.Run(tc.name, func(t *testing.T) {
			exportEnv(t)
			dir := filepath.Join(t.TempDir(), "access")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.file {
				if err := os.WriteFile(filepath.Join(dir, "notes"), []byte("mine"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			s := withCluster(t)

			got, _ := runExport(t, vultrfake.New(), exportArgs(s, dir)...)

			if got.code != 1 {
				t.Errorf("exit code %d, want 1\n%s", got.code, got.errOut)
			}
			want := map[string]string{}
			if tc.file {
				want["notes"] = "mine"
			}
			if diff := cmp.Diff(want, exported(t, dir)); diff != "" {
				t.Errorf("%s (-held +now):\n%s", dir, diff)
			}
		})
	}
}

// TestExportNomadKeepsADirectoryItMadeOnceItHoldsAFile leaves the directory that this run made when the access fails
// and the directory is no longer empty: another run wrote its token there meanwhile.
func TestExportNomadKeepsADirectoryItMadeOnceItHoldsAFile(t *testing.T) {
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		noNomad := func(nomadops.Config) (nomadops.API, error) {
			if err := os.WriteFile(filepath.Join(dir, "token"), []byte("of another run"), 0o600); err != nil {
				t.Error(err)
			}
			return nil, errors.New("no Nomad client")
		}

		got := runWithNomad(t, onVultr(f), noNomad, exportArgs(s, dir)...)

		if got.code != 1 || got.out != "" || !strings.HasSuffix(got.errOut, "no Nomad client\n") {
			t.Errorf("exit code %d, stdout %q, stderr %q, want 1, nothing and the error of the client", got.code, got.out,
				got.errOut)
		}
		if diff := cmp.Diff(map[string]string{"token": "of another run"}, exported(t, dir)); diff != "" {
			t.Errorf("%s (-held +now):\n%s", dir, diff)
		}
	})
}

// TestExportNomadRefusesAClusterNameThatIsNotAClusterName follows the rule of the use case before it makes the
// default directory: PROD names the state of prod on a disk that ignores case.
func TestExportNomadRefusesAClusterNameThatIsNotAClusterName(t *testing.T) {
	exportEnv(t)
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	s := withCluster(t)

	got, nomad := runExport(t, vultrfake.New(), "export", "nomad", "PROD", "--state", s.url)

	if got.code != 1 || !strings.HasPrefix(got.errOut, `Error: invalid cluster name "PROD": `) {
		t.Errorf("exit code %d, stderr %q, want 1 and an invalid cluster name", got.code, got.errOut)
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Errorf("the cache directory holds %v (%v), want nothing", entries, err)
	}
	if calls := nomad.Calls(); len(calls) != 0 {
		t.Errorf("Nomad got the calls %v", calls)
	}
}

// TestExportNomadRefusesATTLNotAboveZero fails in the form of --wait and --shell before it makes the directory.
func TestExportNomadRefusesATTLNotAboveZero(t *testing.T) {
	for ttl, shown := range map[string]string{"0": "0s", "-1h": "-1h0m0s"} {
		t.Run(ttl, func(t *testing.T) {
			exportEnv(t)
			dir := filepath.Join(t.TempDir(), "access")
			synctest.Test(t, func(t *testing.T) {
				s, f := builtCluster(t)

				got, nomad := runExport(t, f, exportArgs(s, dir, "--ttl", ttl)...)

				wantError(t, got, "Error: invalid --ttl "+shown+": must be above zero\n")
				wantNothingExported(t, dir, nomad)
			})
		})
	}
}

// TestExportNomadTTLOutsideNomadsLimits says what Nomad says, and leaves no file and no directory.
func TestExportNomadTTLOutsideNomadsLimits(t *testing.T) {
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)

		got, _ := runExport(t, f, exportArgs(s, dir, "--ttl", "48h")...)

		_, nomadsText, _ := strings.Cut(got.errOut, "token 0 invalid")
		const want = ": 1 error occurred: * expiration time cannot be more than 24h0m0s in the future (was 48h0m0s)\n"
		if got.code != 1 || got.out != "" || nomadsText != want {
			t.Errorf("exit code %d, stdout %q, stderr %q, want 1, nothing and Nomad's text %q", got.code, got.out,
				got.errOut, want)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the directory that this run made is still there: stat %s: %v", dir, err)
		}
	})
}

// TestExportNomadDefaultDirectory writes to $XDG_CACHE_HOME/tent/<cluster> when it is absolute, else under the home
// directory in .cache/tent/<cluster>.
func TestExportNomadDefaultDirectory(t *testing.T) {
	xdg := t.TempDir()
	for _, tc := range []struct {
		name, xdg string
		want      func(home string) string
	}{
		{"XDG_CACHE_HOME", xdg, func(string) string { return filepath.Join(xdg, "tent", "prod") }},
		{"the home directory", "", func(home string) string { return filepath.Join(home, ".cache", "tent", "prod") }},
		{"a relative XDG_CACHE_HOME", "relative", func(home string) string {
			return filepath.Join(home, ".cache", "tent", "prod")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := exportEnv(t)
			t.Setenv("XDG_CACHE_HOME", tc.xdg)
			t.Chdir(t.TempDir()) // where a relative XDG_CACHE_HOME would point
			synctest.Test(t, func(t *testing.T) {
				s, f := builtCluster(t)

				got, _ := runExport(t, f, "export", "nomad", "prod", "--state", s.url, "-o", "json")

				if got.code != 0 {
					t.Fatalf("exit code %d\n%s", got.code, got.errOut)
				}
				var out struct{ Dir string }
				if err := json.Unmarshal([]byte(got.out), &out); err != nil {
					t.Fatal(err)
				}
				if want := tc.want(home); out.Dir != want {
					t.Errorf("the directory is %q, want %q", out.Dir, want)
				}
				wantExported(t, out.Dir)
			})
		})
	}
}

// TestExportNomadWithoutAHomeAsksForDir fails before it reaches anything when there is no place for the default
// directory.
func TestExportNomadWithoutAHomeAsksForDir(t *testing.T) {
	exportEnv(t)
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)

		got, nomad := runExport(t, f, "export", "nomad", "prod", "--state", s.url)

		if got.code != 1 || got.out != "" || !strings.HasSuffix(got.errOut, "; give --dir\n") ||
			!strings.HasPrefix(got.errOut, "Error: the default directory: ") {
			t.Errorf("exit code %d, stdout %q, stderr %q, want 1, nothing and a hint at --dir", got.code, got.out,
				got.errOut)
		}
		if calls := nomad.Calls(); len(calls) != 0 {
			t.Errorf("Nomad got the calls %v", calls)
		}
	})
}

// TestExportNomadMakesRelativeDirectoryAbsolute writes below the working directory, and names absolute paths.
func TestExportNomadMakesRelativeDirectoryAbsolute(t *testing.T) {
	exportEnv(t)
	work := t.TempDir()
	t.Chdir(work)
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)

		got, _ := runExport(t, f, exportArgs(s, filepath.Join("rel", "access"), "-o", "json")...)

		if got.code != 0 {
			t.Fatalf("exit code %d\n%s", got.code, got.errOut)
		}
		var out struct{ Dir, CACert string }
		if err := json.Unmarshal([]byte(got.out), &out); err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(out.Dir) || !strings.HasSuffix(out.Dir, filepath.Join("rel", "access")) ||
			out.CACert != filepath.Join(out.Dir, "ca.pem") {
			t.Errorf("the directory is %q and the CA file %q, want absolute paths that end in rel/access", out.Dir,
				out.CACert)
		}
		wantExported(t, filepath.Join(work, "rel", "access"))
	})
}

// TestExportNomadStructuredOutput prints one object in JSON and in YAML: the cluster, the directory, the address, the
// paths, the server name, the accessor and the end, and no shell line; the notice stays on stderr.
func TestExportNomadStructuredOutput(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			exportEnv(t)
			dir := filepath.Join(t.TempDir(), "access")
			synctest.Test(t, func(t *testing.T) {
				s, f := builtCluster(t)

				got, nomad := runExport(t, f, exportArgs(s, dir, "-o", format)...)

				if got.code != 0 {
					t.Fatalf("exit code %d\n%s", got.code, got.errOut)
				}
				var out map[string]string
				var err error
				if format == "json" {
					err = json.Unmarshal([]byte(got.out), &out)
				} else {
					err = yaml.Unmarshal([]byte(got.out), &out)
				}
				if err != nil {
					t.Fatalf("stdout is no %s object: %v\n%s", format, err, got.out)
				}
				m := wroteNotice.FindStringSubmatch(got.errOut)
				if m == nil {
					t.Fatalf("stderr = %q, want the notice", got.errOut)
				}
				want := map[string]string{
					"cluster": "prod", "dir": dir, "address": "https://" + tokenServer(t, nomad),
					"caCert": filepath.Join(dir, "ca.pem"), "clientCert": filepath.Join(dir, "cli.pem"),
					"clientKey": filepath.Join(dir, "cli-key.pem"), "tlsServerName": "server.global.nomad",
					"tokenFile": filepath.Join(dir, "token"), "tokenAccessor": m[2], "expires": out["expires"],
				}
				if diff := cmp.Diff(want, out); diff != "" {
					t.Errorf("the object (-want +got):\n%s", diff)
				}
				if !regexp.MustCompile(`^2000-01-02T00:\d\d:\d\dZ$`).MatchString(out["expires"]) {
					t.Errorf("expires = %q, want RFC 3339 in UTC, a day after the build", out["expires"])
				}
				if strings.Contains(got.out, "NOMAD_") {
					t.Errorf("stdout holds a shell line:\n%s", got.out)
				}
			})
		})
	}
}

// TestExportNomadShowsNoSecrets keeps the token's secret and the key out of both streams, also with -vv and in each
// format, and keeps the bootstrap secret and the CA key out of the files.
func TestExportNomadShowsNoSecrets(t *testing.T) {
	for _, format := range []string{"table", "json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			exportEnv(t)
			dir := filepath.Join(t.TempDir(), "access")
			synctest.Test(t, func(t *testing.T) {
				s, f := builtCluster(t)

				got, _ := runExport(t, f, exportArgs(s, dir, "-o", format, "-vv", "--log-format", "json")...)

				if got.code != 0 {
					t.Fatalf("exit code %d\n%s", got.code, got.errOut)
				}
				files := wantExported(t, dir)
				issued := map[string][]byte{"token": []byte(files["token"]), "key": []byte(files["cli-key.pem"])}
				secrettest.CheckHidden(t, map[string]string{"stdout": got.out, "stderr": got.errOut}, issued, "")
				stored := map[string][]byte{}
				for _, p := range secretPaths {
					if p != "prod/pki/ca-bundle.pem" { // public
						stored[p] = []byte(s.objects(t)[p])
					}
				}
				secrettest.CheckHidden(t, files, stored, "")
				secrettest.CheckHidden(t, map[string]string{"stdout": got.out, "stderr": got.errOut}, stored, "")
			})
		})
	}
}

// TestExportNomadChecksTheClusterName refuses a name that is no path segment before it makes a directory.
func TestExportNomadChecksTheClusterName(t *testing.T) {
	exportEnv(t)
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	s := withCluster(t)

	got, nomad := runExport(t, vultrfake.New(), "export", "nomad", "../x", "--state", s.url)

	if got.code != 1 || !strings.HasPrefix(got.errOut, `Error: invalid cluster name "../x": `) {
		t.Errorf("exit code %d, stderr %q, want 1 and an invalid cluster name", got.code, got.errOut)
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Errorf("the cache directory holds %v (%v), want nothing", entries, err)
	}
	if calls := nomad.Calls(); len(calls) != 0 {
		t.Errorf("Nomad got the calls %v", calls)
	}
}

// TestExportNomadNeedsAState fails before it makes the directory when no state store is set.
func TestExportNomadNeedsAState(t *testing.T) {
	exportEnv(t)
	t.Setenv(envState, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "access")

	got, _ := runExport(t, vultrfake.New(), "export", "nomad", "prod", "--dir", dir)

	if got.code != 1 || !strings.HasPrefix(got.errOut, "Error: no state store: ") {
		t.Errorf("exit code %d, stderr %q, want 1 and no state store", got.code, got.errOut)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory was made: stat %s: %v", dir, err)
	}
}

// TestExportAloneShowsItsHelp lists nomad.
func TestExportAloneShowsItsHelp(t *testing.T) {
	got := runIn(t, "", "export")

	if got.code != 0 || !strings.Contains(got.out, "nomad") || got.errOut != "" {
		t.Errorf("exit code %d, stdout %q, stderr %q, want the help that lists nomad", got.code, got.out, got.errOut)
	}
}

// TestExportNomadNamesTheTokenAfterTheOperator asks Nomad for a token named "tent export nomad <owner>@<host>".
func TestExportNomadNamesTheTokenAfterTheOperator(t *testing.T) {
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		owner, host := statestore.LocalHolder()

		_, nomad := runExport(t, f, exportArgs(s, dir)...)

		if issued := nomad.Issued(); len(issued) != 1 || issued[0].Name != "tent export nomad "+owner+"@"+host {
			t.Errorf("Nomad issued %+v, want one token named after %s@%s", issued, owner, host)
		}
	})
}

// TestExportNomadNamesTheServerAfterTheRegion sets NOMAD_TLS_SERVER_NAME to the name that the servers' certificate
// has in the cluster's Nomad region, and the operator certificate is for that region.
func TestExportNomadNamesTheServerAfterTheRegion(t *testing.T) {
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		s.put(t, clusterPath, replaced(t, clusterYAML, "vultr: {}\n", "vultr: {}\n  nomad:\n    region: eu\n"))

		got, _ := runExport(t, f, exportArgs(s, dir, "-o", "json")...)

		if got.code != 0 {
			t.Fatalf("exit code %d\n%s", got.code, got.errOut)
		}
		if !strings.Contains(got.out, `"tlsServerName": "server.eu.nomad"`) {
			t.Errorf("stdout = %s, want the server name server.eu.nomad", got.out)
		}
		block, _ := pem.Decode([]byte(exported(t, dir)["cli.pem"]))
		if cert, err := x509.ParseCertificate(block.Bytes); err != nil || cert.Subject.CommonName != "cli.eu.nomad" {
			t.Errorf("cli.pem is %v, %v, want a certificate for cli.eu.nomad", cert, err)
		}
	})
}

// earlyToken is a Nomad API whose tokens end a fraction of a second before their TTL says, as when Nomad's clock is
// behind tent's.
type earlyToken struct{ nomadops.API }

func (a earlyToken) CreateToken(ctx context.Context, req nomadops.TokenRequest) (nomadops.Token, error) {
	tok, err := a.API.CreateToken(ctx, req)
	tok.Expires = tok.Expires.Add(-1234 * time.Millisecond)
	return tok, err
}

// TestExportNomadPrintsTheEndInWholeSeconds cuts the fraction of a second off the end of the access, which is the
// token's when that ends before the certificate.
func TestExportNomadPrintsTheEndInWholeSeconds(t *testing.T) {
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		_, factory := nomadWorld("servers", "workers", 6)
		nomad := func(cfg nomadops.Config) (nomadops.API, error) {
			api, err := factory(cfg)
			return earlyToken{api}, err
		}

		got := runWithNomad(t, onVultr(f), nomad, exportArgs(s, dir, "-o", "json")...)

		var out struct{ Expires string }
		if err := json.Unmarshal([]byte(got.out), &out); err != nil || got.code != 0 {
			t.Fatalf("exit code %d, stdout %q (%v), stderr %q", got.code, got.out, err, got.errOut)
		}
		if strings.Contains(out.Expires, ".") {
			t.Errorf("expires = %q, want whole seconds", out.Expires)
		}
	})
}

// TestExportNomadNamesTheFileItCannotCreate names the final path of the first file when the directory takes no new
// file, and leaves nothing in it.
func TestExportNomadNamesTheFileItCannotCreate(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory that refuses a new file: no permission bits on Windows, and root ignores them")
	}
	exportEnv(t)
	dir := filepath.Join(t.TempDir(), "access")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)

		got, _ := runExport(t, f, exportArgs(s, dir)...)

		wantPrefix := "Error: write " + filepath.Join(dir, "ca.pem") + ": "
		if got.code != 1 || got.out != "" || !strings.HasPrefix(got.errOut, wantPrefix) {
			t.Errorf("exit code %d, stdout %q, stderr %q, want 1, nothing and the error of the write", got.code, got.out,
				got.errOut)
		}
		if files := exported(t, dir); len(files) != 0 {
			t.Errorf("%s holds %d files", dir, len(files))
		}
	})
}
