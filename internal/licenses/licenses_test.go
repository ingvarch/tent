package licenses

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/licenseclassifier/v2/assets"
	"golang.org/x/mod/module"
	"golang.org/x/mod/zip"
)

var update = flag.Bool("update", false, "rewrite testdata/notices.golden")

// testdata returns the text of a file in testdata.
func testdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeFiles writes files, named by slash-separated paths, below dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// useProxy publishes module versions, each with its files, in a GOPROXY directory and points the go command at it,
// with an empty module cache, so the test sees what real dependencies look like.
func useProxy(t *testing.T, mods map[module.Version]map[string]string) {
	t.Helper()
	proxy := t.TempDir()
	for mod, files := range mods {
		src := t.TempDir()
		writeFiles(t, src, files)
		versions := filepath.Join(proxy, filepath.FromSlash(mod.Path), "@v")
		writeFiles(t, versions, map[string]string{
			mod.Version + ".info": `{"Version":"` + mod.Version + `"}`,
			mod.Version + ".mod":  files["go.mod"],
		})
		f, err := os.Create(filepath.Join(versions, mod.Version+".zip"))
		if err != nil {
			t.Fatal(err)
		}
		if err := zip.CreateFromDir(f, mod, src); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	url := filepath.ToSlash(proxy)
	if !strings.HasPrefix(url, "/") {
		url = "/" + url // a Windows drive
	}
	t.Setenv("GOPROXY", "file://"+url)
	t.Setenv("GOMODCACHE", t.TempDir())
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOWORK", "off")
	// The test module gets its go.sum, and the module cache stays removable.
	t.Setenv("GOFLAGS", "-mod=mod -modcacherw")
}

// useApp writes a program that requires the modules and imports the packages, makes its directory the working
// directory and returns it.
func useApp(t *testing.T, mods []module.Version, imports ...string) string {
	t.Helper()
	var requires, imported strings.Builder
	for _, m := range mods {
		fmt.Fprintf(&requires, "require %s %s\n", m.Path, m.Version)
	}
	for _, p := range imports {
		fmt.Fprintf(&imported, "import _ %q\n", p)
	}
	app := t.TempDir()
	writeFiles(t, app, map[string]string{
		"go.mod":  "module example.com/app\n\ngo 1.26\n\n" + requires.String(),
		"LICENSE": "The program's own licence is not a third-party notice.\n",
		"main.go": "package main\n\n" + imported.String() + "\nfunc main() {}\n",
	})
	t.Chdir(app)
	return app
}

func TestLinkedFindsTheLicencesOnEveryPlatform(t *testing.T) {
	mit, header := testdata(t, "mit.txt"), testdata(t, "apache-2.0-header.txt")
	a := module.Version{Path: "example.com/a", Version: "v1.0.0"}
	lib := module.Version{Path: "example.com/lib", Version: "v1.0.0"}
	useProxy(t, map[module.Version]map[string]string{
		a: {
			"go.mod": "module example.com/a\n\ngo 1.26\n\nrequire example.com/lib v1.0.0\n",
			// The only licence is at the root, above the only linked package.
			"LICENSE": goLicense,
			"a.go":    "package a\n",
			// go list lists lib before a.
			"sub/sub.go": "package sub\n\nimport _ \"example.com/lib\"\n",
		},
		lib: {
			"go.mod":     "module example.com/lib\n\ngo 1.26\n",
			"LICENSE":    mit,
			"NOTICE":     header,
			"README.md":  "Not a licence.\n",
			"license.go": "package lib\n",
			"lib.go":     "package lib\n",
			// Only Windows binaries link win.
			"lib_windows.go": "package lib\n\nimport _ \"example.com/lib/win\"\n",
			"win/win.go":     "package win\n",
			"win/LICENSE":    goLicense,
			"cgo/cgo.go":     "package cgo\n",
			"cgo/LICENSE":    mit,
		},
	})
	app := useApp(t, []module.Version{a, lib}, "example.com/a/sub", "example.com/lib")
	// The release builds without cgo, so cgo is never linked.
	writeFiles(t, app, map[string]string{
		"cgo.go": "//go:build cgo\n\npackage main\n\nimport _ \"example.com/lib/cgo\"\n",
	})

	bsd := []string{"BSD-3-Clause"}
	std := Module{
		Path: "std", Version: runtime.Version(), Source: "https://go.dev/dl/" + runtime.Version() + ".src.tar.gz",
		Files: []File{{"LICENSE", goLicense, bsd}}, Licenses: bsd,
	}
	aMod := Module{
		Path: a.Path, Version: a.Version, Source: "https://proxy.golang.org/example.com/a/@v/v1.0.0.zip",
		Files: []File{{"LICENSE", goLicense, bsd}}, Licenses: bsd,
	}
	libWith := func(files []File, licenses ...string) Module {
		return Module{
			Path: lib.Path, Version: lib.Version, Source: "https://proxy.golang.org/example.com/lib/@v/v1.0.0.zip",
			Files: files, Licenses: licenses,
		}
	}
	// The notice holds only an Apache-2.0 header, which counts.
	onLinux := []File{{"LICENSE", mit, []string{"MIT"}}, {"NOTICE", header, []string{"Apache-2.0"}}}
	cases := []struct {
		platforms []string
		want      []Module
	}{
		{[]string{"linux/amd64"}, []Module{std, aMod, libWith(onLinux, "Apache-2.0", "MIT")}},
		{[]string{"linux/amd64", "windows/arm64"}, []Module{std, aMod, libWith(
			append(onLinux, File{"win/LICENSE", goLicense, bsd}), "Apache-2.0", "BSD-3-Clause", "MIT",
		)}},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.platforms, ","), func(t *testing.T) {
			got, err := Linked(t.Context(), c.platforms, ".")
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(c.want, got); diff != "" {
				t.Errorf("Linked (-want +got):\n%s", diff)
			}
		})
	}
	// A cross build has no cgo anyway; a native one has it unless it is turned off.
	host := runtime.GOOS + "/" + runtime.GOARCH
	t.Run(host, func(t *testing.T) {
		got, err := Linked(t.Context(), []string{host}, ".")
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(got, func(m Module) bool { return m.Path == lib.Path })
		if i < 0 {
			t.Fatalf("Linked(%s) = %+v, without %s", host, got, lib.Path)
		}
		if slices.ContainsFunc(got[i].Files, func(f File) bool { return f.Path == "cgo/LICENSE" }) {
			t.Errorf("Linked(%s) builds with cgo; the release does not", host)
		}
	})
}

func TestCheckFailsOnAnUnknownLicenceBesideAKnownOne(t *testing.T) {
	// A known licence in a subpackage must not hide a proprietary licence at the root.
	prop := module.Version{Path: "example.com/prop", Version: "v1.0.0"}
	useProxy(t, map[module.Version]map[string]string{prop: {
		"go.mod":      "module example.com/prop\n\ngo 1.26\n",
		"LICENSE":     "Copyright 2026 Example Corp. All rights reserved. Use needs a written agreement.\n",
		"sub/LICENSE": testdata(t, "mit.txt"),
		"sub/sub.go":  "package sub\n",
	}})
	useApp(t, []module.Version{prop}, "example.com/prop/sub")
	mods, err := Linked(t.Context(), []string{"linux/amd64"}, ".")
	if err != nil {
		t.Fatal(err)
	}
	const want = "example.com/prop v1.0.0: LICENSE: no licence found"
	if err := Check(mods, Allowed); err == nil || err.Error() != want {
		t.Errorf("Check: err %v, want %q", err, want)
	}
}

func TestGoListNamesThePlatformWhenItFails(t *testing.T) {
	_, err := goList(t.Context(), "windows/arm64", []string{"./does-not-exist"})
	if err == nil || !strings.HasPrefix(err.Error(), "list packages for windows/arm64: ") {
		t.Errorf("goList: err %v, want one that names windows/arm64", err)
	}
}

func TestCollectRefusesModulesItCannotName(t *testing.T) {
	downloaded := &listedModule{Path: "example.com/x", Version: "v1.0.0", Dir: "/x"}
	notDownloaded, replaced := *downloaded, *downloaded
	notDownloaded.Dir = ""
	replaced.Replace = &listedModule{Path: "../fork", Dir: "/fork"}
	cases := []struct {
		name   string
		module *listedModule
		says   string
	}{
		{"no module", nil, "package example.com/x is in no module"},
		{
			"not downloaded", &notDownloaded,
			"module example.com/x v1.0.0 is not in the module cache: run go mod download",
		},
		// The notices would name the source of the module that was replaced.
		{
			"replaced", &replaced,
			"module example.com/x v1.0.0 is replaced by ../fork, and the notices cannot name its source",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := collect([]listedPackage{{ImportPath: "example.com/x", Dir: "/x", Module: c.module}})
			if err == nil || err.Error() != c.says {
				t.Errorf("collect: err %v, want %q", err, c.says)
			}
		})
	}
}

func TestCheckNamesEveryUnknownOrDisallowedLicence(t *testing.T) {
	allowed := []string{"Apache-2.0", "MIT"}
	ok := Module{Path: "example.com/ok", Version: "v1.0.0", Licenses: []string{"Apache-2.0", "MIT"}, Files: []File{
		{Path: "LICENSE", Licenses: []string{"MIT"}},
		{Path: "NOTICE", Licenses: []string{"Apache-2.0"}},
		// A notice need not name a licence.
		{Path: "sub/NOTICE.txt"},
	}}
	if err := Check([]Module{ok}, allowed); err != nil {
		t.Errorf("Check(allowed licences) = %v", err)
	}
	mods := []Module{
		ok,
		{
			Path: "example.com/gpl", Version: "v1.2.0", Licenses: []string{"GPL-3.0", "MIT"},
			Files: []File{{Path: "COPYING", Licenses: []string{"GPL-3.0", "MIT"}}},
		},
		// A licence the classifier does not know, such as BUSL-1.1, fails beside a known one too.
		{
			Path: "example.com/busl", Version: "v2.0.0", Licenses: []string{"MIT"},
			Files: []File{{Path: "LICENSE"}, {Path: "sub/LICENSE", Licenses: []string{"MIT"}}},
		},
		{Path: "example.com/notice", Version: "v0.2.0", Files: []File{{Path: "NOTICE"}}},
		{Path: "example.com/none", Version: "v0.1.0"},
	}
	want := strings.Join([]string{
		"example.com/gpl v1.2.0: GPL-3.0 is not an allowed licence",
		"example.com/busl v2.0.0: LICENSE: no licence found",
		"example.com/notice v0.2.0: no licence found",
		"example.com/none v0.1.0: no licence found",
	}, "\n")
	if err := Check(mods, allowed); err == nil || err.Error() != want {
		t.Errorf("Check: err %v, want:\n%s", err, want)
	}
}

func TestAllowedLicencesAreKnownToTheClassifier(t *testing.T) {
	// A name the classifier does not use never matches, so the list would allow less than it says.
	for _, name := range Allowed {
		if _, err := assets.ReadLicenseFile("License/" + name); errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the classifier does not know %s", name)
		}
	}
}

func TestNotices(t *testing.T) {
	const golden = "notices.golden"
	mods := []Module{
		{
			Path: "std", Version: "go1.26.0", Source: "https://go.dev/dl/go1.26.0.src.tar.gz",
			Files:    []File{{Path: "LICENSE", Text: "Copyright 2009 The Go Authors.\n"}},
			Licenses: []string{"BSD-3-Clause"},
		},
		{
			Path: "example.com/lib", Version: "v1.0.0",
			Source: "https://proxy.golang.org/example.com/lib/@v/v1.0.0.zip",
			Files: []File{
				{Path: "LICENSE", Text: "The MIT licence.\n"},
				{Path: "sub/NOTICE", Text: "A notice without a final newline."},
			},
			Licenses: []string{"Apache-2.0", "MIT"},
		},
	}
	got := Notices(mods)
	if *update {
		if err := os.WriteFile(filepath.Join("testdata", golden), got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if diff := cmp.Diff(testdata(t, golden), string(got)); diff != "" {
		t.Errorf("Notices differ from %s (-file +got):\n%s", golden, diff)
	}
}

func TestGoLicenseIsTheOneGoShips(t *testing.T) {
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(strings.TrimSpace(string(out)), "LICENSE")
	shipped, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Homebrew and Linux distributions move it; CI installs Go from go.dev, which keeps it.
		if os.Getenv("CI") != "" {
			t.Fatalf("%s does not exist", path)
		}
		t.Skipf("%s does not exist: this Go distribution moved it", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(string(shipped), goLicense); diff != "" {
		t.Errorf("go.LICENSE differs from %s; copy it (-shipped +copy):\n%s", path, diff)
	}
}
