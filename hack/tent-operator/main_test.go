package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/hack/internal/shellenv/shellenvtest"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

const clusterName = "prod"

// fixture is a state store with a cluster in it, as tent update leaves one.
type fixture struct {
	url    string // the store's URL
	root   string // the store's directory
	ca     *pki.CA
	token  pki.Secret
	region string
}

// fileURL returns the file:// URL of an absolute directory: file:///srv/state, or file:///C:/state on Windows.
func fileURL(dir string) string {
	p := filepath.ToSlash(dir)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

// newFixture writes the cluster's completed spec, CA and bootstrap secret into a new store.
func newFixture(t *testing.T, region string) *fixture {
	t.Helper()
	f := &fixture{root: t.TempDir(), region: region, token: pki.NewBootstrapSecret()}
	f.url = fileURL(f.root)
	var err error
	if f.ca, err = pki.NewCA(clusterName, time.Now()); err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	c := &v1alpha1.Cluster{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindCluster},
		Metadata: v1alpha1.ClusterMeta{Name: clusterName},
		Spec: v1alpha1.ClusterSpec{
			Cloud: v1alpha1.Cloud{Provider: v1alpha1.ProviderVultr, Region: "ams"},
			Nomad: v1alpha1.ClusterNomad{Region: region},
		},
	}
	completed, err := spec.Encode(spec.Objects{Cluster: c})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	f.put(t, "cluster.completed.yaml", completed)
	f.put(t, "pki/private/ca.key", f.ca.Key().Bytes())
	f.put(t, "pki/ca-bundle.pem", f.ca.Bundle())
	f.put(t, "secrets/acl-bootstrap-token", f.token.Bytes())
	return f
}

// put writes an object of the cluster into the store.
func (f *fixture) put(t *testing.T, name string, data []byte) {
	t.Helper()
	s, err := statestore.Open(t.Context(), f.url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Put(t.Context(), clusterName+"/"+name, data, statestore.PutOptions{}); err != nil {
		t.Fatalf("Put %s: %v", name, err)
	}
}

// remove deletes an object of the cluster from the store.
func (f *fixture) remove(t *testing.T, name string) {
	t.Helper()
	s, err := statestore.Open(t.Context(), f.url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Delete(t.Context(), clusterName+"/"+name); err != nil {
		t.Fatalf("Delete %s: %v", name, err)
	}
}

// secrets are the values that no output of the tool may show.
func (f *fixture) secrets() map[string][]byte {
	return map[string][]byte{"the CA key": f.ca.Key().Bytes(), "the bootstrap secret": f.token.Bytes()}
}

// result is one run of the tool.
type result struct {
	code           int
	stdout, stderr string
	dir            string
}

// runWith runs the tool with the flags, on top of the defaults of the fixture, in a new directory.
func runWith(t *testing.T, f *fixture, env map[string]string, args ...string) result {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "operator")
	all := append([]string{"-state", f.url, "-name", clusterName, "-dir", dir, "-addr", "https://203.0.113.5:4646",
		"-shell", "sh"}, args...)
	return operateRaw(t, dir, env, all...)
}

// operateRaw runs the tool with exactly the flags.
func operateRaw(t *testing.T, dir string, env map[string]string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), args, &stdout, &stderr, func(k string) string { return env[k] })
	return result{code, stdout.String(), stderr.String(), dir}
}

func (r result) read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

func mustSucceed(t *testing.T, r result) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", r.code, r.stderr)
	}
}

func parseCert(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return c
}

func TestWritesTheOperatorFiles(t *testing.T) {
	f := newFixture(t, "eu")
	r := runWith(t, f, nil, "-ttl", "2h")
	mustSucceed(t, r)

	if got := r.read(t, "ca.pem"); !bytes.Equal(got, f.ca.Bundle()) {
		t.Error("ca.pem is not the CA bundle of the store")
	}
	if got := r.read(t, "token"); !bytes.Equal(got, f.token.Bytes()) {
		t.Error("token is not the bootstrap secret of the store")
	}
	cert := parseCert(t, r.read(t, "cli.pem"))
	roots := x509.NewCertPool()
	roots.AddCert(f.ca.Certificate())
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("the operator certificate does not chain to the CA of the store for client authentication: %v", err)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "cli.eu.nomad" {
		t.Errorf("certificate names %v, want [cli.eu.nomad]", cert.DNSNames)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("extended key usage %v, want client authentication only", cert.ExtKeyUsage)
	}
	// The certificate starts a few minutes before now, so it lasts the TTL plus that backdate.
	if lasts := cert.NotAfter.Sub(cert.NotBefore); lasts < 2*time.Hour || lasts > 2*time.Hour+10*time.Minute {
		t.Errorf("the certificate lasts %s, want the TTL of 2h and a short backdate", lasts)
	}
	block, _ := pem.Decode(r.read(t, "cli-key.pem"))
	if block == nil {
		t.Fatal("cli-key.pem has no PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("ParsePKCS8PrivateKey: %v", err)
	}
	if pub, ok := key.(*ecdsa.PrivateKey); !ok || !pub.PublicKey.Equal(cert.PublicKey) {
		t.Error("cli-key.pem is not the key of cli.pem")
	}
}

func TestDefaultTTLIsOneHour(t *testing.T) {
	f := newFixture(t, "global")
	r := runWith(t, f, nil)
	mustSucceed(t, r)
	cert := parseCert(t, r.read(t, "cli.pem"))
	if lasts := cert.NotAfter.Sub(cert.NotBefore); lasts < time.Hour || lasts > time.Hour+10*time.Minute {
		t.Errorf("the certificate lasts %s, want 1h and a short backdate", lasts)
	}
}

func TestFilesAreOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix modes")
	}
	f := newFixture(t, "global")
	r := runWith(t, f, nil)
	mustSucceed(t, r)
	if info, err := os.Stat(r.dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("directory: %v, %v; want mode 0700", info, err)
	}
	for _, name := range []string{"ca.pem", "cli.pem", "cli-key.pem", "token"} {
		if info, err := os.Stat(filepath.Join(r.dir, name)); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v; want mode 0600", name, info, err)
		}
	}
}

func TestLinesSetTheNomadVariables(t *testing.T) {
	names := []string{"NOMAD_ADDR", "NOMAD_CACERT", "NOMAD_CLIENT_CERT", "NOMAD_CLIENT_KEY", "NOMAD_TLS_SERVER_NAME"}
	for _, shell := range shellenvtest.Shells {
		t.Run(shell, func(t *testing.T) {
			f := newFixture(t, "eu")
			// A directory name that the shell must quote.
			dir := filepath.Join(t.TempDir(), "it's a dir")
			r := operateRaw(t, dir, nil, "-state", f.url, "-name", clusterName, "-dir", dir,
				"-addr", "https://203.0.113.5:4646", "-shell", shell)
			mustSucceed(t, r)
			if strings.Count(r.stdout, "\n") != len(names) {
				t.Errorf("stdout has %d lines, want %d:\n%s", strings.Count(r.stdout, "\n"), len(names), r.stdout)
			}
			got := strings.Split(strings.TrimSuffix(shellenvtest.Run(t, shell, r.stdout+shellenvtest.Print(shell, names...)),
				"\n"), "\n")
			want := []string{"https://203.0.113.5:4646", filepath.Join(dir, "ca.pem"), filepath.Join(dir, "cli.pem"),
				filepath.Join(dir, "cli-key.pem"), "server.eu.nomad"}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("the shell reads %q, want %q", got, want)
			}
		})
	}
}

func TestStderrSaysHowToSetTheToken(t *testing.T) {
	for _, shell := range shellenvtest.Shells {
		t.Run(shell, func(t *testing.T) {
			f := newFixture(t, "global")
			dir := filepath.Join(t.TempDir(), "it's a dir")
			r := operateRaw(t, dir, nil, "-state", f.url, "-name", clusterName, "-dir", dir,
				"-addr", "https://203.0.113.5:4646", "-shell", shell)
			mustSucceed(t, r)
			_, line, ok := strings.Cut(r.stderr, "NOMAD_TOKEN: ")
			if !ok {
				t.Fatalf("stderr does not say how to set NOMAD_TOKEN:\n%s", r.stderr)
			}
			line, _, _ = strings.Cut(line, "\n")
			wantStart := map[string]string{"fish": "set -gx NOMAD_TOKEN (cat ", "sh": "export NOMAD_TOKEN=\"$(cat "}[shell]
			if !strings.HasPrefix(line, wantStart) {
				t.Fatalf("the line starts %.30q, want %q", line, wantStart)
			}
			got := shellenvtest.Run(t, shell, line+"\n"+shellenvtest.Print(shell, "NOMAD_TOKEN"))
			if got != string(f.token.Bytes())+"\n" {
				t.Error("the line on stderr does not set NOMAD_TOKEN to the bootstrap secret")
			}
		})
	}
}

func TestShellFollowsTheLoginShell(t *testing.T) {
	f := newFixture(t, "global")
	const fish, sh = "set -gx NOMAD_ADDR ", "export NOMAD_ADDR="
	for _, tc := range []struct{ login, flag, want string }{
		{"/usr/bin/fish", "", fish},
		{"/bin/bash", "", sh},
		// The flag wins over the login shell.
		{"/usr/bin/fish", "sh", sh},
		{"/bin/bash", "fish", fish},
	} {
		dir := filepath.Join(t.TempDir(), "operator")
		args := []string{"-state", f.url, "-name", clusterName, "-dir", dir, "-addr", "https://203.0.113.5:4646"}
		if tc.flag != "" {
			args = append(args, "-shell", tc.flag)
		}
		r := operateRaw(t, dir, map[string]string{"SHELL": tc.login}, args...)
		mustSucceed(t, r)
		if !strings.HasPrefix(r.stdout, tc.want) {
			t.Errorf("SHELL=%q, -shell %q: stdout starts %.30q, want %q", tc.login, tc.flag, r.stdout, tc.want)
		}
	}
}

func TestEnvironmentGivesTheStateAndTheCluster(t *testing.T) {
	f := newFixture(t, "global")
	dir := filepath.Join(t.TempDir(), "operator")
	r := operateRaw(t, dir, map[string]string{"TENT_STATE": f.url, "TENT_CLUSTER": clusterName},
		"-dir", dir, "-addr", "https://203.0.113.5:4646", "-shell", "sh")
	mustSucceed(t, r)

	// The flags win over the environment. The other store has a cluster of the same name with another CA.
	other := newFixture(t, "global")
	dir = filepath.Join(t.TempDir(), "operator")
	r = operateRaw(t, dir, map[string]string{"TENT_STATE": other.url, "TENT_CLUSTER": "nosuch"},
		"-state", f.url, "-name", clusterName, "-dir", dir, "-addr", "https://203.0.113.5:4646", "-shell", "sh")
	mustSucceed(t, r)
	if got := r.read(t, "ca.pem"); !bytes.Equal(got, f.ca.Bundle()) {
		t.Error("-state did not win over TENT_STATE: ca.pem is not the CA of the store that the flag names")
	}
}

func TestNothingShowsASecret(t *testing.T) {
	f := newFixture(t, "global")
	r := runWith(t, f, nil)
	mustSucceed(t, r)
	secrets := f.secrets()
	secrets["the operator's key"] = r.read(t, "cli-key.pem")
	secrettest.CheckHidden(t, map[string]string{"stdout": r.stdout, "stderr": r.stderr}, secrets, "")
}

func TestFailuresName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(t *testing.T, f *fixture)
		want  string
	}{
		{"no CA key", func(t *testing.T, f *fixture) { f.remove(t, "pki/private/ca.key") }, "pki/private/ca.key"},
		{"no CA bundle", func(t *testing.T, f *fixture) { f.remove(t, "pki/ca-bundle.pem") }, "pki/ca-bundle.pem"},
		{"no bootstrap secret", func(t *testing.T, f *fixture) { f.remove(t, "secrets/acl-bootstrap-token") },
			"secrets/acl-bootstrap-token"},
		{"no completed spec", func(t *testing.T, f *fixture) { f.remove(t, "cluster.completed.yaml") },
			"cluster.completed.yaml"},
		{"a CA key that is not a key", func(t *testing.T, f *fixture) {
			f.put(t, "pki/private/ca.key", []byte("not-a-key-SECRETTEXT"))
		}, "CA"},
		{"a bootstrap secret that is not a UUID", func(t *testing.T, f *fixture) {
			f.put(t, "secrets/acl-bootstrap-token", []byte("not-a-uuid-SECRETTEXT"))
		}, "bootstrap"},
		{"a completed spec without a Nomad region", func(t *testing.T, f *fixture) {
			f.put(t, "cluster.completed.yaml", []byte("# nothing\n"))
		}, "cluster.completed.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "global")
			tc.spoil(t, f)
			r := runWith(t, f, nil)
			if r.code != exitError {
				t.Errorf("exit code %d, want %d", r.code, exitError)
			}
			if !strings.Contains(r.stderr, tc.want) {
				t.Errorf("stderr does not name %q:\n%s", tc.want, r.stderr)
			}
			if r.stdout != "" {
				t.Errorf("stdout is not empty after a failure:\n%s", r.stdout)
			}
			if _, err := os.Stat(r.dir); err == nil {
				t.Error("the directory exists after a failure")
			}
			secrets := f.secrets()
			secrets["the text of a broken secret"] = []byte("SECRETTEXT")
			secrettest.CheckHidden(t, map[string]string{"stderr": r.stderr}, secrets, "")
		})
	}
}

func TestExistingDirectoryIsLeftAlone(t *testing.T) {
	f := newFixture(t, "global")
	dir := t.TempDir()
	keep := filepath.Join(dir, "token")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := operateRaw(t, dir, nil, "-state", f.url, "-name", clusterName, "-dir", dir, "-addr", "https://203.0.113.5:4646",
		"-shell", "sh")
	if r.code != exitError || !strings.Contains(r.stderr, dir) {
		t.Errorf("exit code %d, stderr:\n%s; want %d and the directory named", r.code, r.stderr, exitError)
	}
	if got, _ := os.ReadFile(keep); string(got) != "mine" {
		t.Error("the tool changed a file of an existing directory")
	}
}

func TestBadStoreURLShowsNoPassword(t *testing.T) {
	f := newFixture(t, "global")
	r := runWith(t, f, nil, "-state", "s3://key:PASSWORD@bucket/x")
	if r.code != exitError || strings.Contains(r.stderr, "PASSWORD") {
		t.Errorf("exit code %d, stderr:\n%s; want %d and no password", r.code, r.stderr, exitError)
	}
}

func TestUsageErrors(t *testing.T) {
	f := newFixture(t, "global")
	dir := filepath.Join(t.TempDir(), "operator")
	good := []string{"-state", f.url, "-name", clusterName, "-dir", dir, "-addr", "https://203.0.113.5:4646", "-shell", "sh"}
	without := func(flag string) []string {
		var out []string
		for i := 0; i < len(good); i += 2 {
			if good[i] != flag {
				out = append(out, good[i], good[i+1])
			}
		}
		return out
	}
	for name, args := range map[string][]string{
		"no state":      without("-state"),
		"no name":       without("-name"),
		"no dir":        without("-dir"),
		"no addr":       without("-addr"),
		"http address":  append(without("-addr"), "-addr", "http://203.0.113.5:4646"),
		"bare address":  append(without("-addr"), "-addr", "203.0.113.5"),
		"zero TTL":      append(slicesClone(good), "-ttl", "0"),
		"negative TTL":  append(slicesClone(good), "-ttl", "-1h"),
		"unknown shell": append(without("-shell"), "-shell", "bash"),
		"an argument":   append(slicesClone(good), "extra"),
		"unknown flag":  append(slicesClone(good), "-nope"),
	} {
		t.Run(name, func(t *testing.T) {
			r := operateRaw(t, dir, nil, args...)
			if r.code != exitUsage {
				t.Errorf("exit code %d, want %d; stderr:\n%s", r.code, exitUsage, r.stderr)
			}
			if r.stdout != "" {
				t.Errorf("stdout is not empty:\n%s", r.stdout)
			}
			if _, err := os.Stat(dir); err == nil {
				t.Error("the directory exists after a usage error")
			}
		})
	}
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

func TestHelpWritesUsageToStderr(t *testing.T) {
	r := operateRaw(t, "", nil, "-h")
	if r.code != 0 || r.stdout != "" || !strings.Contains(r.stderr, "Usage: tent-operator") {
		t.Errorf("exit code %d, stdout %q, stderr:\n%s", r.code, r.stdout, r.stderr)
	}
}

func TestHelpShowsNoEnvironmentValues(t *testing.T) {
	r := operateRaw(t, "", map[string]string{"TENT_STATE": "s3://key:PASSWORD@bucket/x", "TENT_CLUSTER": "prod"}, "-h")
	if strings.Contains(r.stderr, "PASSWORD") || strings.Contains(r.stderr, `(default "`) {
		t.Errorf("the usage shows an environment value:\n%s", r.stderr)
	}
}

func TestFailedWriteRemovesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "operator")
	files := map[string][]byte{"token": []byte("a"), "missing/cli-key.pem": []byte("b"), "ca.pem": nil}
	if err := writeFiles(dir, files); err == nil {
		t.Fatal("writeFiles succeeded with a name that cannot be created")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the directory is still there after the failed write")
	}
}

func TestRelativeDirectoryIsPrintedAbsolute(t *testing.T) {
	f := newFixture(t, "eu")
	t.Chdir(t.TempDir())
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(wd, "operator")
	r := operateRaw(t, dir, nil, "-state", f.url, "-name", clusterName, "-dir", "operator",
		"-addr", "https://203.0.113.5:4646", "-shell", "sh")
	mustSucceed(t, r)
	for _, name := range []string{"ca.pem", "cli.pem", "cli-key.pem"} {
		if want := filepath.Join(dir, name); !strings.Contains(r.stdout, want) {
			t.Errorf("stdout does not name %s:\n%s", want, r.stdout)
		}
	}
	if !strings.Contains(r.stderr, dir) || !strings.Contains(r.stderr, filepath.Join(dir, "token")) {
		t.Errorf("stderr does not name %s:\n%s", dir, r.stderr)
	}
}
