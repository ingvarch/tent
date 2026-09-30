package nodeup_test

import (
	"archive/tar"
	"io/fs"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

// Where the cni phase puts the plugins.
const (
	cniDir    = "/opt/cni"
	cniBinDir = cniDir + "/bin"
)

// cniPath is the path of the CNI plugins' archive on GitHub, and on the tests' servers.
const cniPath = "/containernetworking/plugins/releases/download/v1.9.1/cni-plugins-linux-amd64-v1.9.1.tgz"

// plugins are the plugins of the tests' archive.
var plugins = []string{"bridge", "host-local", "loopback", "portmap", "firewall"}

// pluginFile returns an entry of an archive: a stand-in for the plugin name, with the mode.
func pluginFile(name string, mode int64) nodeuptest.TarFile {
	return nodeuptest.TarFile{Header: tar.Header{Name: "./" + name, Mode: mode}, Content: []byte(name + " plugin\n")}
}

// topDir is the entry of an archive's top directory.
var topDir = nodeuptest.TarFile{Header: tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}}

// license is the entry of the licence in an archive of the CNI plugins.
var license = nodeuptest.TarFile{
	Header: tar.Header{Name: "./LICENSE", Mode: 0o644}, Content: []byte("Apache License\n"),
}

// cniArchive returns the tests' archive of the CNI plugins, as a release lays it out: the top directory, the plugins
// and the licence.
func cniArchive(t *testing.T) []byte {
	t.Helper()
	files := []nodeuptest.TarFile{topDir}
	for _, p := range plugins {
		files = append(files, pluginFile(p, 0o755))
	}
	return nodeuptest.Tgz(t, append(files, license)...)
}

// cniPlugins returns the cni-plugins asset of the tests' archive, from GitHub.
func cniPlugins(t *testing.T) nodeconfig.Asset {
	t.Helper()
	return nodeconfig.Asset{
		Name: nodeconfig.CNIPluginsAsset, Version: "1.9.1", URLs: []string{"https://github.com" + cniPath},
		SHA256: sum(cniArchive(t)),
	}
}

// serveCNI serves archive at cniPath, gives h a client that trusts the server, and makes nc's cni-plugins asset that
// archive from the server.
func serveCNI(t *testing.T, h *nodeup.Host, nc *nodeconfig.NodeConfig, archive []byte) *nodeuptest.Server {
	t.Helper()
	srv := serve(t, h, map[string]http.HandlerFunc{cniPath: file(archive)})
	i := slices.IndexFunc(nc.Assets, func(a nodeconfig.Asset) bool { return a.Name == nodeconfig.CNIPluginsAsset })
	if i < 0 {
		t.Fatal("the config has no cni-plugins asset")
	}
	nc.Assets[i].URLs, nc.Assets[i].SHA256 = []string{srv.URL + cniPath}, sum(archive)
	rehash(t, nc)
	return srv
}

// under returns the entries under dir in fsys, by path, dir included.
func under(fsys *nodeuptest.FS, dir string) map[string]nodeuptest.Entry {
	var paths []string
	for _, p := range fsys.Paths() {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			paths = append(paths, p)
		}
	}
	return entries(fsys, paths...)
}

// installed returns the entries of /opt/cni once the plugins and the licence of the tests' archive are in place.
func installed() map[string]nodeuptest.Entry {
	want := map[string]nodeuptest.Entry{
		cniDir:                 {Dir: true, Mode: 0o755, Owner: nodeconfig.Owner},
		cniBinDir:              {Dir: true, Mode: 0o755, Owner: nodeconfig.Owner},
		cniBinDir + "/LICENSE": {Data: license.Content, Mode: 0o644, Owner: nodeconfig.Owner},
	}
	for _, p := range plugins {
		want[cniBinDir+"/"+p] = nodeuptest.Entry{Data: []byte(p + " plugin\n"), Mode: 0o755, Owner: nodeconfig.Owner}
	}
	return want
}

func TestCNI(t *testing.T) {
	h, fsys, r, e := ubuntu(t)
	nc := combined(t)
	archive := cniArchive(t)
	srv := serveCNI(t, h, nc, archive)
	res, err := runPhase(t, "cni", h, nc, e)
	if err != nil || res != (nodeup.Result{Status: nodeup.Done}) {
		t.Fatalf("cni: %+v, %v; want done", res, err)
	}
	if diff := cmp.Diff([]string{cniPath}, srv.Requests()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(installed(), under(fsys, cniDir)); diff != "" {
		t.Errorf("/opt/cni (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(cached(archive), entries(fsys, tentDir, assetDir, cachePath)); diff != "" {
		t.Errorf("the cache (-want +got):\n%s", diff)
	}
	want := []string{tentDir, assetDir, cachePath, cniDir, cniBinDir}
	for _, p := range append(slices.Clone(plugins), "LICENSE") {
		want = append(want, cniBinDir+"/"+p)
	}
	if diff := cmp.Diff(want, fsys.Changes()); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	if len(r.Commands()) != 0 {
		t.Errorf("cni ran %q, want nothing", r.Commands())
	}

	// The second run takes the cached archive and finds every file in place.
	res, err = runPhase(t, "cni", h, nc, e)
	if err != nil || res != (nodeup.Result{Status: nodeup.Unchanged}) {
		t.Fatalf("the second cni: %+v, %v; want unchanged", res, err)
	}
	if got := srv.Requests(); len(got) != 1 {
		t.Errorf("the second cni asked for %q, want nothing", got[1:])
	}
	if got := fsys.Changes()[len(want):]; len(got) != 0 {
		t.Errorf("the second cni changed %q, want nothing", got)
	}
}

func TestCNIPutsBackAnAlteredPlugin(t *testing.T) {
	h, fsys, _, e := ubuntu(t)
	nc := combined(t)
	srv := serveCNI(t, h, nc, cniArchive(t))
	if _, err := runPhase(t, "cni", h, nc, e); err != nil {
		t.Fatal(err)
	}
	fsys.AddFile(t, cniBinDir+"/bridge", []byte("altered\n"), 0o777, "nobody:nogroup")
	changes := len(fsys.Changes())
	res, err := runPhase(t, "cni", h, nc, e)
	if err != nil || res != (nodeup.Result{Status: nodeup.Done}) {
		t.Fatalf("cni: %+v, %v; want done", res, err)
	}
	if diff := cmp.Diff([]string{cniBinDir + "/bridge"}, fsys.Changes()[changes:]); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(installed(), under(fsys, cniDir)); diff != "" {
		t.Errorf("/opt/cni (-want +got):\n%s", diff)
	}
	if got := srv.Requests(); len(got) != 1 {
		t.Errorf("requests %q, want one", got)
	}
}

func TestCNIDownloadsALostArchive(t *testing.T) {
	h, fsys, _, e := ubuntu(t)
	nc := combined(t)
	srv := serveCNI(t, h, nc, cniArchive(t))
	if _, err := runPhase(t, "cni", h, nc, e); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Remove(cachePath); err != nil {
		t.Fatal(err)
	}
	changes := len(fsys.Changes())
	// The plugins are in place, and only the cache changes.
	res, err := runPhase(t, "cni", h, nc, e)
	if err != nil || res != (nodeup.Result{Status: nodeup.Done}) {
		t.Fatalf("cni: %+v, %v; want done", res, err)
	}
	if diff := cmp.Diff([]string{cachePath}, fsys.Changes()[changes:]); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	if got := srv.Requests(); len(got) != 2 {
		t.Errorf("requests %q, want two", got)
	}
}

func TestCNIKeepsOtherFiles(t *testing.T) {
	h, fsys, _, e := ubuntu(t)
	nc := combined(t)
	serveCNI(t, h, nc, cniArchive(t))
	other := nodeuptest.Entry{Data: []byte("an operator's plugin\n"), Mode: 0o700, Owner: nodeconfig.Owner}
	fsys.AddDir(t, cniDir, 0o755, nodeconfig.Owner)
	fsys.AddDir(t, cniBinDir, 0o755, nodeconfig.Owner)
	fsys.AddFile(t, cniBinDir+"/macvlan", other.Data, other.Mode, other.Owner)
	if _, err := runPhase(t, "cni", h, nc, e); err != nil {
		t.Fatal(err)
	}
	want := installed()
	want[cniBinDir+"/macvlan"] = other
	if diff := cmp.Diff(want, under(fsys, cniDir)); diff != "" {
		t.Errorf("/opt/cni (-want +got):\n%s", diff)
	}
}

func TestCNIModes(t *testing.T) {
	h, fsys, _, e := ubuntu(t)
	nc := combined(t)
	serveCNI(t, h, nc, nodeuptest.Tgz(t,
		pluginFile("bridge", 0o777), pluginFile("portmap", 0o4755), pluginFile("dhcp", 0o600),
		// A name without ./ is at the top level too.
		nodeuptest.TarFile{Header: tar.Header{Name: "vlan", Mode: 0o750}, Content: []byte("vlan plugin\n")},
	))
	if _, err := runPhase(t, "cni", h, nc, e); err != nil {
		t.Fatal(err)
	}
	modes := map[string]fs.FileMode{}
	for p, entry := range under(fsys, cniBinDir) {
		modes[p] = entry.Mode
	}
	want := map[string]fs.FileMode{
		cniBinDir: 0o755, cniBinDir + "/bridge": 0o755, cniBinDir + "/portmap": 0o755, cniBinDir + "/dhcp": 0o600,
		cniBinDir + "/vlan": 0o750,
	}
	if diff := cmp.Diff(want, modes); diff != "" {
		t.Errorf("modes (-want +got):\n%s", diff)
	}
}

func TestCNIRefusesAnArchive(t *testing.T) {
	archive := cniArchive(t)
	for _, c := range []struct {
		name    string
		archive []byte
		want    string
	}{
		{"a nested file", tgz(t, pluginFile("sub/bridge", 0o755)),
			`"./sub/bridge" is not a plain file name at the top level`},
		{"a file above", tgz(t, nodeuptest.TarFile{Header: tar.Header{Name: "../bridge", Mode: 0o755}}),
			`"../bridge" is not a plain file name at the top level`},
		{"an absolute path", tgz(t, nodeuptest.TarFile{Header: tar.Header{Name: "/usr/bin/bridge", Mode: 0o755}}),
			`"/usr/bin/bridge" is not a plain file name at the top level`},
		{"a hidden file", tgz(t, pluginFile(".bridge", 0o755)), `"./.bridge" is not a plain file name at the top level`},
		{"a nested directory", tgz(t, nodeuptest.TarFile{Header: tar.Header{Name: "./sub/", Typeflag: tar.TypeDir,
			Mode: 0o755}}), `"./sub/" is a directory, not a file at the top level`},
		{"a symbolic link", tgz(t, nodeuptest.TarFile{Header: tar.Header{Name: "./cni", Typeflag: tar.TypeSymlink,
			Linkname: "/etc/cni"}}), `"./cni" is a symbolic link, not a file at the top level`},
		{"a hard link", tgz(t, nodeuptest.TarFile{Header: tar.Header{Name: "./ptp", Typeflag: tar.TypeLink,
			Linkname: "./bridge"}}), `"./ptp" is a hard link, not a file at the top level`},
		{"a character device", tgz(t, nodeuptest.TarFile{Header: tar.Header{Name: "./null", Typeflag: tar.TypeChar,
			Devmajor: 1, Devminor: 3}}), `"./null" is a character device, not a file at the top level`},
		{"a block device", tgz(t, nodeuptest.TarFile{Header: tar.Header{Name: "./sda", Typeflag: tar.TypeBlock,
			Devmajor: 8}}), `"./sda" is a block device, not a file at the top level`},
		{"a named pipe", tgz(t, nodeuptest.TarFile{Header: tar.Header{Name: "./fifo", Typeflag: tar.TypeFifo}}),
			`"./fifo" is a named pipe, not a file at the top level`},
		{"another type", tgz(t, nodeuptest.TarFile{Header: tar.Header{Name: "./ptp", Typeflag: tar.TypeCont,
			Mode: 0o755}, Content: []byte("ptp plugin\n")}),
			`"./ptp" is an entry of type '7', not a file at the top level`},
		{"a file above 256 MiB", tgz(t, nodeuptest.TarFile{Header: tar.Header{Name: "./huge", Mode: 0o755,
			Size: 256<<20 + 1}, Content: []byte("the start")}), `"./huge" has 268435457 bytes, more than 268435456`},
		{"a file twice", tgz(t, nodeuptest.TarFile{Header: tar.Header{Name: "bridge", Mode: 0o755},
			Content: []byte("bridge plugin\n")}), `"bridge": the archive holds bridge twice`},
		{"no gzip", []byte("not an archive\n"), "gzip: invalid header"},
		{"a cut archive", archive[:len(archive)/2], "unexpected EOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, _, e := ubuntu(t)
			nc := combined(t)
			serveCNI(t, h, nc, c.archive)
			_, err := runPhase(t, "cni", h, nc, e)
			if want := "unpack cni-plugins 1.9.1: " + c.want; errText(err) != want {
				t.Errorf("cni: %q, want %q", errText(err), want)
			}
			if got := under(fsys, cniDir); len(got) != 0 {
				t.Errorf("cni wrote %q, want nothing", slices.Sorted(maps.Keys(got)))
			}
		})
	}
}

// tgz returns an archive of the top directory, the bridge plugin, file and the licence, in that order.
func tgz(t *testing.T, file nodeuptest.TarFile) []byte {
	t.Helper()
	return nodeuptest.Tgz(t, topDir, pluginFile("bridge", 0o755), file, license)
}

func TestCNISkipsServers(t *testing.T) {
	h, fsys, r, e := ubuntu(t)
	nc := server(t)
	srv := serveCNI(t, h, nc, cniArchive(t))
	res, err := runPhase(t, "cni", h, nc, e)
	if want := (nodeup.Result{Status: nodeup.Skipped, Reason: "servers run no workloads"}); err != nil || res != want {
		t.Errorf("cni: %+v, %v; want %+v", res, err, want)
	}
	if len(srv.Requests()) != 0 || len(r.Commands()) != 0 || len(fsys.Changes()) != 0 {
		t.Errorf("cni asked for %q, ran %q and changed %q, want nothing", srv.Requests(), r.Commands(), fsys.Changes())
	}
}

func TestCNIFails(t *testing.T) {
	t.Run("no asset", func(t *testing.T) {
		h, fsys, _, e := ubuntu(t)
		nc := combined(t)
		nc.Assets = slices.DeleteFunc(nc.Assets, func(a nodeconfig.Asset) bool {
			return a.Name == nodeconfig.CNIPluginsAsset
		})
		_, err := runPhase(t, "cni", h, rehash(t, nc), e)
		if want := "NodeConfig has no cni-plugins asset"; errText(err) != want {
			t.Errorf("cni: %q, want %q", errText(err), want)
		}
		if len(fsys.Changes()) != 0 {
			t.Errorf("cni changed %q, want nothing", fsys.Changes())
		}
	})
	t.Run("no download", func(t *testing.T) {
		h, fsys, _, e := ubuntu(t)
		nc := combined(t)
		srv := serveCNI(t, h, nc, cniArchive(t))
		nc.Assets[1].URLs = []string{srv.URL + "/missing.tgz"}
		_, err := runPhase(t, "cni", h, nc, e)
		want := "fetch cni-plugins 1.9.1: GET " + srv.URL + "/missing.tgz: answered 404 Not Found"
		if errText(err) != want {
			t.Errorf("cni: %q, want %q", errText(err), want)
		}
		if got := under(fsys, cniDir); len(got) != 0 {
			t.Errorf("cni wrote %q, want nothing", slices.Sorted(maps.Keys(got)))
		}
	})
}
