package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/s3url"
	"github.com/ingvarch/tent/internal/s3url/s3urltest"
	"github.com/ingvarch/tent/internal/secrettest"
)

// fakeBucket is the bucket that the fake S3 serves, path-style.
const fakeBucket = "tent-dev"

// testToken is the session token that some tests give the AWS SDK beside the made-up keys of s3urltest. The secret
// access key never shows in what the tool prints, nor does the session token on stderr.
const testToken = "tent-node-upload-session-token-9876543210"

// fakeS3 answers HEAD and PUT on the objects of one bucket, path-style.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte    // by key
	modified map[string]time.Time // when each object was uploaded
	requests []string             // the method and key of every request
	fail     map[string]int       // the status that requests with the method get instead of an answer
	url      string
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}, modified: map[string]time.Time{}, fail: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s3Error(w, http.StatusBadRequest, "IncompleteBody")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key, inBucket := strings.CutPrefix(r.URL.Path, "/"+fakeBucket+"/")
	f.requests = append(f.requests, r.Method+" "+key)
	if status := f.fail[r.Method]; status != 0 {
		s3Error(w, status, "Refused")
		return
	}
	switch {
	case !inBucket:
		s3Error(w, http.StatusNotFound, "NoSuchBucket")
	case r.Method == http.MethodHead:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("ETag", `"1"`)
		w.Header().Set("Last-Modified", f.modified[key].UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut:
		f.objects[key], f.modified[key] = body, time.Now()
		w.Header().Set("ETag", `"1"`)
		w.WriteHeader(http.StatusOK)
	default:
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
	}
}

// s3Error answers with an S3 error. Like AWS, it names the access key, which the tool must not print.
func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "<Error><Code>"+code+"</Code><Message>refused</Message><AWSAccessKeyId>"+s3urltest.KeyID+
		"</AWSAccessKeyId></Error>")
}

// log returns the method and key of every request so far.
func (f *fakeS3) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// object returns the object at key and whether it exists.
func (f *fakeS3) object(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	return data, ok
}

// put stores an object directly, as an upload at the time modified would have.
func (f *fakeS3) put(key string, data []byte, modified time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key], f.modified[key] = data, modified
}

// useFake points TENT_DEV_S3_URL at the prefix dev in the fake's bucket, with the test's credentials, and makes sh
// the user's shell.
func useFake(t *testing.T, f *fakeS3) {
	t.Helper()
	s3urltest.IsolateAWS(t)
	t.Setenv(urlEnv, "s3://"+fakeBucket+"/dev?endpoint="+f.url+"&region=auto&pathStyle=true")
	t.Setenv("SHELL", "/bin/sh")
}

// elfFile returns a linux ELF executable for the machine: its header and then content.
func elfFile(machine elf.Machine, content string) []byte {
	return elfOf(elf.ELFCLASS64, elf.ET_EXEC, machine, content)
}

// elfOf returns a linux ELF file of the class and type for the machine: its header and then content.
func elfOf(class elf.Class, typ elf.Type, machine elf.Machine, content string) []byte {
	size, sizeAt := 64, 52 // the header's size, and where the header holds it
	if class == elf.ELFCLASS32 {
		size, sizeAt = 52, 40
	}
	h := make([]byte, size)
	copy(h, elf.ELFMAG)
	h[elf.EI_CLASS] = byte(class)
	h[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	h[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(h[16:], uint16(typ))
	binary.LittleEndian.PutUint16(h[18:], uint16(machine))
	binary.LittleEndian.PutUint32(h[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(h[sizeAt:], uint16(size))
	return append(h, content...)
}

// writeBinary writes a tent-node for the machine into a new directory, and returns its path, content and sha256.
func writeBinary(t *testing.T, machine elf.Machine) (path string, data []byte, sum string) {
	t.Helper()
	data = elfFile(machine, "tent-node "+t.Name())
	path = writeFile(t, data)
	s := sha256.Sum256(data)
	return path, data, hex.EncodeToString(s[:])
}

// writeFile writes data as tent-node into a new directory and returns its path.
func writeFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tent-node")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// runTool runs the tool with args and returns its exit code and output.
func runTool(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(t.Context(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// mustRun runs the tool with args, fails the test unless it succeeds, and returns what it printed on stdout.
func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := runTool(t, args...)
	if code != 0 {
		t.Fatalf("tent-node-upload %s: exit %d, stderr:\n%s", strings.Join(args, " "), code, errOut)
	}
	return out
}

// exported matches a line that the tool prints for sh.
var exported = regexp.MustCompile(`^export (TENT_NODE_URL|TENT_NODE_SHA256)='([^'\\]*)'$`)

// printed returns the values of TENT_NODE_URL and TENT_NODE_SHA256 in what the tool printed for sh, and fails the
// test unless it printed exactly those two lines.
func printed(t *testing.T, stdout string) (nodeURL, sum string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	values := map[string]string{}
	for _, line := range lines {
		m := exported.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("the tool printed %d lines, want two export lines, and not %q", len(lines), line)
		}
		values[m[1]] = m[2]
	}
	if len(lines) != 2 || len(values) != 2 {
		t.Fatalf("the tool printed %d lines, want export TENT_NODE_URL and TENT_NODE_SHA256", len(lines))
	}
	return values["TENT_NODE_URL"], values["TENT_NODE_SHA256"]
}

// parsedURL parses the presigned URL and fails the test when it does not parse.
func parsedURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("the printed URL does not parse: %v", err)
	}
	return u
}

func TestObjectKey(t *testing.T) {
	sum := strings.Repeat("0a", 32)
	for _, tc := range []struct{ prefix, arch, want string }{
		{"dev", "amd64", "dev/tent-node/" + sum + "/tent-node_linux_amd64"},
		{"dev", "arm64", "dev/tent-node/" + sum + "/tent-node_linux_arm64"},
		{"ci/dev", "amd64", "ci/dev/tent-node/" + sum + "/tent-node_linux_amd64"},
	} {
		if got := objectKey(tc.prefix, sum, tc.arch); got != tc.want {
			t.Errorf("objectKey(%q, sum, %q) = %q, want %q", tc.prefix, tc.arch, got, tc.want)
		}
	}
	// The file is the one in tent's releases.
	if got, want := objectKey("dev", sum, "amd64"), "dev/tent-node/"+sum+"/"+assets.TentNodeFile("amd64"); got != want {
		t.Errorf("objectKey = %q, want the release's file name, %q", got, want)
	}
}

func TestParseBucketURL(t *testing.T) {
	// internal/s3url reads the URL; the tool requires a prefix and checks its segments.
	for _, tc := range []struct {
		raw  string
		want s3url.URL
	}{
		{"s3://tent-ci/dev?endpoint=https://acc.r2.cloudflarestorage.com&region=auto",
			s3url.URL{Bucket: "tent-ci", Prefix: "dev", Endpoint: "https://acc.r2.cloudflarestorage.com", Region: "auto"}},
		{"s3://tent-ci/dev/?endpoint=https://acc.r2.cloudflarestorage.com/&region=auto",
			s3url.URL{Bucket: "tent-ci", Prefix: "dev", Endpoint: "https://acc.r2.cloudflarestorage.com", Region: "auto"}},
		{"s3://b/a/b_c/d-e.f?endpoint=http://127.0.0.1:9000&region=us-east-1&pathStyle=true",
			s3url.URL{Bucket: "b", Prefix: "a/b_c/d-e.f", Endpoint: "http://127.0.0.1:9000", Region: "us-east-1",
				PathStyle: true}},
		{"s3://b/dev", s3url.URL{Bucket: "b", Prefix: "dev"}},
	} {
		got, err := parseBucketURL(tc.raw)
		if err != nil {
			t.Errorf("parseBucketURL(%q): %v", tc.raw, err)
			continue
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("parseBucketURL(%q) (-want +got):\n%s", tc.raw, diff)
		}
	}
}

func TestParseBucketURLFails(t *testing.T) {
	// A value that must never show in an error: the URL may hold one by mistake.
	const hidden = "hidden-value"
	for _, tc := range []struct{ name, raw, want string }{
		{"unset", "", "TENT_DEV_S3_URL is not set"},
		{"no prefix", "s3://b", "name a prefix"},
		{"a slash for a prefix", "s3://b/?region=auto", "name a prefix"},
		{"an empty segment", "s3://b/dev//x", "invalid prefix"},
		{"a dot segment", "s3://b/dev/../x", "invalid prefix"},
		{"a leading dot", "s3://b/.dev", "invalid prefix"},
		{"a space", "s3://b/de%20v", "invalid prefix"},
		// What internal/s3url refuses, named by the variable.
		{"user and password", "s3://user:" + hidden + "@b/dev", "remove the user and password"},
		{"another scheme", "https://" + hidden + "/dev", "not an s3 URL"},
		{"an unknown parameter", "s3://b/dev?token=" + hidden, "unknown query parameter"},
		{"an endpoint with a user", "s3://b/dev?endpoint=https://u:" + hidden + "%40h", "remove the user and password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseBucketURL(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseBucketURL(%q) = %v, want an error that says %q", tc.raw, err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), urlEnv) {
				t.Errorf("the error %q does not name %s", err, urlEnv)
			}
			if strings.Contains(err.Error(), hidden) {
				t.Errorf("the error %q shows a value of the URL", err)
			}
		})
	}
}

func TestUploadsOnceThenSkips(t *testing.T) {
	f := newFakeS3(t)
	useFake(t, f)
	bin, data, sum := writeBinary(t, elf.EM_X86_64)
	key := "dev/tent-node/" + sum + "/tent-node_linux_amd64"

	first, firstSum := printed(t, mustRun(t, "-binary", bin))
	if got, ok := f.object(key); !ok || !bytes.Equal(got, data) {
		t.Fatalf("after the first run, the bucket holds %d bytes at %s (exists %t), want the binary's %d",
			len(got), key, ok, len(data))
	}
	second, secondSum := printed(t, mustRun(t, "-binary", bin))

	if diff := cmp.Diff([]string{"HEAD " + key, "PUT " + key, "HEAD " + key}, f.log()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	for i, u := range []string{first, second} {
		if got, want := parsedURL(t, u).Path, "/"+fakeBucket+"/"+key; got != want {
			t.Errorf("run %d: the URL's path is %q, want %q", i+1, got, want)
		}
	}
	if firstSum != sum || secondSum != sum {
		t.Errorf("TENT_NODE_SHA256 = %q then %q, want the binary's %q", firstSum, secondSum, sum)
	}
}

func TestSkipsAnObjectThatOutlivesTheURL(t *testing.T) {
	// The lifecycle rule deletes an object 8 days after its upload.
	for _, tc := range []struct {
		name string
		age  time.Duration
		args []string
	}{
		{"an hour old, a week's URL", time.Hour, nil},
		{"23 hours old, a week's URL", 23 * time.Hour, nil},
		{"6 days old, an hour's URL", 6 * 24 * time.Hour, []string{"-expires", "1h"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(t)
			useFake(t, f)
			bin, _, sum := writeBinary(t, elf.EM_X86_64)
			key := "dev/tent-node/" + sum + "/tent-node_linux_amd64"
			f.put(key, []byte("an earlier upload"), time.Now().Add(-tc.age))

			code, out, errOut := runTool(t, append([]string{"-binary", bin}, tc.args...)...)
			if code != 0 {
				t.Fatalf("exit %d, stderr:\n%s", code, errOut)
			}
			if diff := cmp.Diff([]string{"HEAD " + key}, f.log()); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
			if got, _ := f.object(key); string(got) != "an earlier upload" {
				t.Error("the tool replaced the object that was there")
			}
			if !strings.Contains(errOut, "not uploaded again") {
				t.Errorf("stderr does not say the upload was skipped:\n%s", errOut)
			}
			printed(t, out)
		})
	}
}

func TestUploadsAgainAnObjectThatTheURLWouldOutlive(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		args []string
	}{
		{"25 hours old, a week's URL", 25 * time.Hour, nil},
		{"7 days old, a day's URL", 7 * 24 * time.Hour, []string{"-expires", "24h"}},
		{"older than the rule", 9 * 24 * time.Hour, []string{"-expires", "1h"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(t)
			useFake(t, f)
			bin, data, sum := writeBinary(t, elf.EM_X86_64)
			key := "dev/tent-node/" + sum + "/tent-node_linux_amd64"
			f.put(key, data, time.Now().Add(-tc.age))

			code, out, errOut := runTool(t, append([]string{"-binary", bin}, tc.args...)...)
			if code != 0 {
				t.Fatalf("exit %d, stderr:\n%s", code, errOut)
			}
			if diff := cmp.Diff([]string{"HEAD " + key, "PUT " + key}, f.log()); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
			if !strings.Contains(errOut, "uploading it again") {
				t.Errorf("stderr does not say why the object is uploaded again:\n%s", errOut)
			}
			printed(t, out)
		})
	}
}

func TestPresignedURLLivesAtMostAWeek(t *testing.T) {
	f := newFakeS3(t)
	useFake(t, f)
	bin, _, _ := writeBinary(t, elf.EM_X86_64)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "604800"},
		{[]string{"-expires", "168h"}, "604800"},
		{[]string{"-expires", "1h"}, "3600"},
		{[]string{"-expires", "1s"}, "1"},
	} {
		nodeURL, _ := printed(t, mustRun(t, append([]string{"-binary", bin}, tc.args...)...))
		q := parsedURL(t, nodeURL).Query()
		if got := q.Get("X-Amz-Expires"); got != tc.want {
			t.Errorf("with %q, X-Amz-Expires = %q, want %q", tc.args, got, tc.want)
		}
		// curl on the node sends a plain GET: the signature covers the host alone, and the URL asks for no checksum.
		if got := q.Get("X-Amz-SignedHeaders"); got != "host" {
			t.Errorf("with %q, X-Amz-SignedHeaders = %q, want host", tc.args, got)
		}
		want := []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-Signature",
			"X-Amz-SignedHeaders", "x-id"}
		if diff := cmp.Diff(want, slices.Sorted(maps.Keys(q))); diff != "" {
			t.Errorf("with %q, the URL's query parameters (-want +got):\n%s", tc.args, diff)
		}
		if n, err := strconv.Atoi(q.Get("X-Amz-Expires")); err != nil || n > 604800 {
			t.Errorf("with %q, X-Amz-Expires = %q, want at most 604800", tc.args, q.Get("X-Amz-Expires"))
		}
	}
}

func TestRefusesAnExpiryOutsideAWeek(t *testing.T) {
	f := newFakeS3(t)
	useFake(t, f)
	bin, _, _ := writeBinary(t, elf.EM_X86_64)
	for _, expires := range []string{"169h", "168h0m1s", "0s", "-1h", "999ms", "a week"} {
		code, out, errOut := runTool(t, "-binary", bin, "-expires", expires)
		if code != exitUsage || out != "" || !strings.Contains(errOut, "-expires") {
			t.Errorf("-expires %s: exit %d, stdout %q, stderr %q; want exit %d and an error about -expires", expires,
				code, out, errOut, exitUsage)
		}
	}
	if got := f.log(); len(got) != 0 {
		t.Errorf("the tool sent %q, want no request", got)
	}
}

func TestPrintedValuesAreWhatTentAndNodesAccept(t *testing.T) {
	f := newFakeS3(t)
	useFake(t, f)
	bin, _, sum := writeBinary(t, elf.EM_X86_64)
	nodeURL, gotSum := printed(t, mustRun(t, "-binary", bin))

	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(gotSum) || gotSum != sum {
		t.Errorf("TENT_NODE_SHA256 = %q, want the binary's sha256 %q in 64 lower-case hex digits", gotSum, sum)
	}
	// tent takes the variables for a development build.
	const version = "v0.1.0-3-gabc1234-dirty"
	a, err := assets.TentNode(t.Context(), assets.Options{DevURL: nodeURL, DevSHA256: gotSum}, version, "amd64")
	if err != nil {
		t.Fatalf("assets.TentNode with the printed values: %v", err)
	}
	// NodeConfig and the user data take the asset.
	nc := &nodeconfig.NodeConfig{
		APIVersion: v1alpha1.APIVersion, Kind: nodeconfig.Kind, Cluster: "dev", Provider: v1alpha1.ProviderVultr,
		NodeGroup: "clients", Name: "dev-clients-1", Role: v1alpha1.RoleClient, Region: "global",
		Assets:   []nodeconfig.Asset{nodeconfig.Asset(a)},
		Join:     nodeconfig.Join{Strategy: nodeconfig.JoinSeedAndRefresh, RefreshInterval: time.Minute},
		Firewall: nodeconfig.HostFirewall{BlockMetadata: netip.MustParseAddr("169.254.169.254")},
	}
	if err := nc.Validate(); err != nil {
		t.Errorf("a node config with the printed values: %v", err)
	}
	if _, err := nodeconfig.UserData(nc); err != nil {
		t.Errorf("the user data of a node config with the printed values: %v", err)
	}
}

// shells are the shells that the tool prints for, with how each prints two variables after running the lines.
var shells = map[string]string{
	"sh":   `printf '%s\n' "$TENT_NODE_URL" "$TENT_NODE_SHA256"`,
	"fish": `printf '%s\n' $TENT_NODE_URL $TENT_NODE_SHA256`,
}

// inShell runs script in the shell and returns its output. It skips the test when the shell is not installed.
func inShell(t *testing.T, shell, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the lines are for POSIX shells and fish")
	}
	path, err := exec.LookPath(shell)
	if err != nil {
		t.Skipf("%s is not installed", shell)
	}
	cmd := exec.Command(path, "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v, output:\n%s", shell, err, out)
	}
	return string(out)
}

func TestLinesSetTheVariablesInEachShell(t *testing.T) {
	f := newFakeS3(t)
	useFake(t, f)
	bin, _, sum := writeBinary(t, elf.EM_X86_64)
	for shell, show := range shells {
		t.Run(shell, func(t *testing.T) {
			out := mustRun(t, "-binary", bin, "-shell", shell)
			got := strings.Split(strings.TrimSuffix(inShell(t, shell, out+show), "\n"), "\n")
			if len(got) != 2 || got[1] != sum {
				t.Fatalf("%s sets %d values, TENT_NODE_SHA256 %q; want the URL and %q", shell, len(got), got, sum)
			}
			if parsedURL(t, got[0]).Query().Get("X-Amz-Signature") == "" {
				t.Error("TENT_NODE_URL is not a presigned URL")
			}
		})
	}
}

func TestExportLineQuotes(t *testing.T) {
	values := []string{"plain", "it's", `back\slash`, `trailing\`, `$HOME and $(id) and ` + "`id`", "a b\tc", `"double"`,
		`\'`, "semi;colon&amp"}
	for shell, show := range shells {
		t.Run(shell, func(t *testing.T) {
			for _, v := range values {
				script := exportLine(shell, "TENT_NODE_URL", v) + "\n" + exportLine(shell, "TENT_NODE_SHA256", "x") +
					"\n" + show
				if got := inShell(t, shell, script); got != v+"\nx\n" {
					t.Errorf("%s reads %q back as %q", shell, v, strings.TrimSuffix(got, "\nx\n"))
				}
			}
		})
	}
}

func TestShellFollowsTheLoginShell(t *testing.T) {
	f := newFakeS3(t)
	useFake(t, f)
	bin, _, _ := writeBinary(t, elf.EM_X86_64)
	for _, tc := range []struct{ login, args, want string }{
		{"/opt/homebrew/bin/fish", "", "set -gx TENT_NODE_URL '"},
		{"/usr/bin/fish", "", "set -gx TENT_NODE_URL '"},
		{"/bin/bash", "", "export TENT_NODE_URL='"},
		{"/bin/zsh", "", "export TENT_NODE_URL='"},
		{"", "", "export TENT_NODE_URL='"},
		{"/opt/homebrew/bin/fish", "sh", "export TENT_NODE_URL='"},
		{"/bin/bash", "fish", "set -gx TENT_NODE_URL '"},
	} {
		t.Setenv("SHELL", tc.login)
		args := []string{"-binary", bin}
		if tc.args != "" {
			args = append(args, "-shell", tc.args)
		}
		out := mustRun(t, args...)
		if !strings.HasPrefix(out, tc.want) {
			t.Errorf("SHELL=%q, -shell %q: the tool printed %.30q…, want it to start with %q", tc.login, tc.args, out,
				tc.want)
		}
	}
}

func TestNoCredentialPrinted(t *testing.T) {
	bin, _, sum := writeBinary(t, elf.EM_X86_64)
	for _, tc := range []struct {
		name string
		fail map[string]int
		keep bool // the object exists before the run
	}{
		{"upload", nil, false},
		{"skip", nil, true},
		{"HEAD refused", map[string]int{http.MethodHead: http.StatusForbidden}, false},
		{"PUT refused", map[string]int{http.MethodPut: http.StatusForbidden}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(t)
			useFake(t, f)
			for method, status := range tc.fail {
				f.fail[method] = status
			}
			if tc.keep {
				f.put("dev/tent-node/"+sum+"/tent-node_linux_amd64", []byte("x"), time.Now())
			}
			_, out, errOut := runTool(t, "-binary", bin)
			if secrettest.Shows(out+errOut, []byte(s3urltest.Secret)) {
				t.Error("the tool printed the secret access key")
			}
			// The presigned URL carries the key's id; nothing else does.
			if strings.Contains(errOut, s3urltest.KeyID) {
				t.Errorf("stderr shows the access key id:\n%s", errOut)
			}
		})
	}
}

func TestServerRefusals(t *testing.T) {
	bin, _, sum := writeBinary(t, elf.EM_X86_64)
	key := "dev/tent-node/" + sum + "/tent-node_linux_amd64"
	for _, tc := range []struct {
		method string
		want   []string // the requests
		says   string
	}{
		{http.MethodHead, []string{"HEAD " + key}, "look for s3://tent-dev/" + key},
		{http.MethodPut, []string{"HEAD " + key, "PUT " + key}, "upload to s3://tent-dev/" + key},
	} {
		t.Run(tc.method, func(t *testing.T) {
			f := newFakeS3(t)
			useFake(t, f)
			f.fail[tc.method] = http.StatusForbidden
			code, out, errOut := runTool(t, "-binary", bin)
			if code != exitError || out != "" || !strings.Contains(errOut, tc.says) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d, nothing on stdout and an error that says %q",
					code, out, errOut, exitError, tc.says)
			}
			if diff := cmp.Diff(tc.want, f.log()); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBinaryMustBeLinuxForTheArch(t *testing.T) {
	f := newFakeS3(t)
	useFake(t, f)
	dir := t.TempDir()
	script := filepath.Join(dir, "script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho tent-node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	arm, _, _ := writeBinary(t, elf.EM_AARCH64)
	amd, _, _ := writeBinary(t, elf.EM_X86_64)
	// Files for the right machine that a node cannot run.
	bits32 := writeFile(t, elfOf(elf.ELFCLASS32, elf.ET_EXEC, elf.EM_X86_64, "32-bit"))
	object := writeFile(t, elfOf(elf.ELFCLASS64, elf.ET_REL, elf.EM_X86_64, "an object file"))
	core := writeFile(t, elfOf(elf.ELFCLASS64, elf.ET_CORE, elf.EM_AARCH64, "a core dump"))
	// Go writes the OS ABI ELFOSABI_FREEBSD into a FreeBSD build.
	freebsd := elfFile(elf.EM_X86_64, "a FreeBSD build")
	freebsd[elf.EI_OSABI] = byte(elf.ELFOSABI_FREEBSD)
	bsd := writeFile(t, freebsd)
	for _, tc := range []struct {
		name string
		args []string
		says string
	}{
		{"missing", []string{"-binary", filepath.Join(dir, "missing")}, "missing"},
		{"not an ELF", []string{"-binary", script}, "is not a linux executable"},
		{"arm64 for amd64", []string{"-binary", arm}, "is for arm64, not amd64"},
		{"amd64 for arm64", []string{"-binary", amd, "-arch", "arm64"}, "is for amd64, not arm64"},
		{"32-bit", []string{"-binary", bits32}, bits32 + " is not a 64-bit executable: its class is ELFCLASS32"},
		{"relocatable", []string{"-binary", object}, object + " is not an executable: its type is ET_REL"},
		{"core dump", []string{"-binary", core, "-arch", "arm64"}, core + " is not an executable: its type is ET_CORE"},
		{"FreeBSD", []string{"-binary", bsd}, bsd + " is not a linux executable: its OS ABI is ELFOSABI_FREEBSD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runTool(t, tc.args...)
			if code != exitError || out != "" || !strings.Contains(errOut, tc.says) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and an error that says %q", code, out, errOut,
					exitError, tc.says)
			}
		})
	}
	if got := f.log(); len(got) != 0 {
		t.Errorf("the tool sent %q, want no request", got)
	}
}

func TestAcceptsEveryKindOfLinuxExecutable(t *testing.T) {
	// A binary built with -buildmode=pie has the ELF type ET_DYN.
	pie := elfOf(elf.ELFCLASS64, elf.ET_DYN, elf.EM_X86_64, "a position-independent executable")
	// Go leaves the OS ABI of a linux build at ELFOSABI_NONE; GNU ld writes ELFOSABI_LINUX for GNU extensions.
	gnu := elfFile(elf.EM_X86_64, "a GNU/Linux executable")
	gnu[elf.EI_OSABI] = byte(elf.ELFOSABI_LINUX)
	for name, data := range map[string][]byte{"position-independent": pie, "ELFOSABI_LINUX": gnu} {
		t.Run(name, func(t *testing.T) {
			f := newFakeS3(t)
			useFake(t, f)
			printed(t, mustRun(t, "-binary", writeFile(t, data)))
		})
	}
}

func TestDefaultBinaryIsWhatMakeBuildWrites(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		arch    string
		machine elf.Machine
	}{
		{nil, "amd64", elf.EM_X86_64},
		{[]string{"-arch", "arm64"}, "arm64", elf.EM_AARCH64},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			f := newFakeS3(t)
			useFake(t, f)
			dir := t.TempDir()
			t.Chdir(dir)
			data := elfFile(tc.machine, "built by make")
			if err := os.MkdirAll("bin", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join("bin", assets.TentNodeFile(tc.arch)), data, 0o755); err != nil {
				t.Fatal(err)
			}
			s := sha256.Sum256(data)
			key := "dev/tent-node/" + hex.EncodeToString(s[:]) + "/" + assets.TentNodeFile(tc.arch)
			mustRun(t, tc.args...)
			if got, ok := f.object(key); !ok || !bytes.Equal(got, data) {
				t.Errorf("the bucket holds %d bytes at %s (exists %t), want bin/%s", len(got), key, ok,
					assets.TentNodeFile(tc.arch))
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	f := newFakeS3(t)
	useFake(t, f)
	bin, _, _ := writeBinary(t, elf.EM_X86_64)
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"-binary", bin, "-arch", "386"}, "-arch"},
		{[]string{"-binary", bin, "-shell", "csh"}, "-shell"},
		{[]string{"-binary", bin, "extra"}, "no arguments"},
		{[]string{"-no-such-flag"}, "no-such-flag"},
	} {
		code, out, errOut := runTool(t, tc.args...)
		if code != exitUsage || out != "" || !strings.Contains(errOut, tc.says) {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want exit %d and an error that says %q", tc.args, code, out,
				errOut, exitUsage, tc.says)
		}
	}
	if code, _, errOut := runTool(t, "-h"); code != 0 || !strings.Contains(errOut, urlEnv) {
		t.Errorf("-h: exit %d, stderr %q; want exit 0 and a usage that names %s", code, errOut, urlEnv)
	}
	if got := f.log(); len(got) != 0 {
		t.Errorf("the tool sent %q, want no request", got)
	}
}

func TestEnvironmentErrors(t *testing.T) {
	f := newFakeS3(t)
	bin, _, _ := writeBinary(t, elf.EM_X86_64)
	for _, tc := range []struct{ name, url, says string }{
		{"unset", "", "TENT_DEV_S3_URL is not set"},
		{"no region", "s3://" + fakeBucket + "/dev?endpoint=" + f.url + "&pathStyle=true", "TENT_DEV_S3_URL: no region"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useFake(t, f)
			t.Setenv(urlEnv, tc.url)
			code, out, errOut := runTool(t, "-binary", bin)
			if code != exitError || out != "" || !strings.Contains(errOut, tc.says) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and an error that says %q", code, out, errOut,
					exitError, tc.says)
			}
		})
	}
	if got := f.log(); len(got) != 0 {
		t.Errorf("the tool sent %q, want no request", got)
	}
}

// fakeCredentials is a credentials provider that returns what it holds.
type fakeCredentials struct {
	creds aws.Credentials
	err   error
}

func (f fakeCredentials) Retrieve(context.Context) (aws.Credentials, error) { return f.creds, f.err }

func TestCredentialsWarning(t *testing.T) {
	until := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) // when the URL expires
	static := aws.Credentials{
		AccessKeyID: s3urltest.KeyID, SecretAccessKey: s3urltest.Secret, Source: "EnvConfigCredentials",
	}
	session := static
	session.SessionToken = testToken
	expiring := func(at time.Time) aws.Credentials {
		c := session
		c.CanExpire, c.Expires = true, at
		return c
	}
	for _, tc := range []struct {
		name  string
		creds aws.Credentials
		want  string // "" for no warning
	}{
		{"static keys, as an R2 token has", static, ""},
		{"a session token without an expiry", session, "session token"},
		{"expiring after the URL", expiring(until.Add(time.Minute)), ""},
		{"expiring with the URL", expiring(until), ""},
		{"expiring before the URL", expiring(until.Add(-time.Hour)), "expire at 2026-10-06T11:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := credentialsWarning(t.Context(), fakeCredentials{creds: tc.creds}, until)
			if err != nil {
				t.Fatal(err)
			}
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("warning %q, want one that says %q", got, tc.want)
			}
			secrets := map[string]string{"secret": s3urltest.Secret, "token": testToken, "key id": s3urltest.KeyID}
			for name, secret := range secrets {
				if secrettest.Shows(got, []byte(secret)) {
					t.Errorf("the warning shows the %s", name)
				}
			}
		})
	}
	_, err := credentialsWarning(t.Context(), fakeCredentials{err: errors.New("no provider answered")}, until)
	if err == nil || !strings.Contains(err.Error(), "get the AWS credentials: no provider answered") {
		t.Errorf("a failed retrieval = %v, want an error that says get the AWS credentials", err)
	}
}

func TestWarnsAboutASessionToken(t *testing.T) {
	f := newFakeS3(t)
	useFake(t, f)
	t.Setenv("AWS_SESSION_TOKEN", testToken)
	bin, _, _ := writeBinary(t, elf.EM_X86_64)
	code, out, errOut := runTool(t, "-binary", bin)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "warning: ") || !strings.Contains(errOut, "session token") {
		t.Errorf("stderr does not warn about the session token:\n%s", errOut)
	}
	if secrettest.Shows(errOut, []byte(testToken)) || secrettest.Shows(out+errOut, []byte(s3urltest.Secret)) {
		t.Error("the tool printed the session token on stderr or the secret anywhere")
	}
	// A presigned URL of temporary credentials carries their token: it works only while the session does.
	nodeURL, _ := printed(t, out)
	if parsedURL(t, nodeURL).Query().Get("X-Amz-Security-Token") != testToken {
		t.Error("the URL does not carry the session token")
	}
}

func TestNeedsCredentials(t *testing.T) {
	f := newFakeS3(t)
	useFake(t, f)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	bin, _, _ := writeBinary(t, elf.EM_X86_64)
	code, out, errOut := runTool(t, "-binary", bin)
	if code != exitError || out != "" || !strings.Contains(errOut, "get the AWS credentials") {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and an error that says get the AWS credentials", code, out,
			errOut, exitError)
	}
	if got := f.log(); len(got) != 0 {
		t.Errorf("the tool sent %q, want no request", got)
	}
}
