// Package licenses finds the licences of the modules a program links, checks them against the licences tent allows,
// and writes the third-party notices the release ships. It is not linked into tent.
package licenses

import (
	"bytes"
	"context"
	_ "embed" // goLicense
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	classifier "github.com/google/licenseclassifier/v2"
	"github.com/google/licenseclassifier/v2/assets"
	"golang.org/x/mod/module"
)

// Allowed are the licences, as the classifier names them (SPDX identifiers), that a module linked into tent may have.
var Allowed = []string{"Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "ISC", "MIT", "MPL-2.0"}

// Platforms are the systems the release builds tent for, as GOOS/GOARCH; each can link other modules.
var Platforms = []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"}

// goLicense is Go's LICENSE. Homebrew and Linux distributions move it out of GOROOT, so the package carries a copy.
//
//go:embed go.LICENSE
var goLicense string

// Module is the Go standard library or a module a program links.
type Module struct {
	Path     string // "std" for the standard library
	Version  string
	Source   string   // where to download its source code at this version
	Files    []File   // licence and notice files, sorted by path
	Licenses []string // the licences found in Files, sorted
}

// File is a licence or notice file of a module.
type File struct {
	Path     string // from the module root, with slashes
	Text     string
	Licenses []string // the licences found in Text, sorted
}

// listedModule and listedPackage are the parts of `go list -json` output the package reads.
type listedModule struct {
	Path    string
	Version string
	Dir     string
	Main    bool
	Replace *listedModule
}

type listedPackage struct {
	ImportPath string
	Dir        string
	Standard   bool
	Module     *listedModule
}

// licenceFile matches the names of licence and notice files, such as LICENSE, LICENSE.txt, COPYING or NOTICE.md.
var licenceFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice)([-._]|$)`)

// Linked returns the standard library and, sorted by path, every module the packages link on any of the platforms,
// with their licence and notice files and the licences found in them. The program's own module is left out.
func Linked(ctx context.Context, platforms []string, patterns ...string) ([]Module, error) {
	var pkgs []listedPackage
	for _, p := range platforms {
		listed, err := goList(ctx, p, patterns)
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, listed...)
	}
	mods, err := collect(pkgs)
	if err != nil {
		return nil, err
	}
	std, err := stdlib(ctx)
	if err != nil {
		return nil, err
	}
	mods = append([]Module{std}, mods...)

	c, err := assets.DefaultClassifier()
	if err != nil {
		return nil, fmt.Errorf("load the licence classifier: %w", err)
	}
	for i := range mods {
		var all []string
		for j := range mods[i].Files {
			f := &mods[i].Files[j]
			f.Licenses = licensesIn(c, f.Text)
			all = append(all, f.Licenses...)
		}
		slices.Sort(all)
		mods[i].Licenses = slices.Compact(all)
	}
	return mods, nil
}

// goCommand runs the go command with extra environment variables and returns its standard output.
func goCommand(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// goList lists the packages the patterns build from on a platform, without cgo, as the release builds them.
func goList(ctx context.Context, platform string, patterns []string) ([]listedPackage, error) {
	goos, goarch, ok := strings.Cut(platform, "/")
	if !ok {
		return nil, fmt.Errorf("platform %q is not GOOS/GOARCH", platform)
	}
	env := []string{"GOOS=" + goos, "GOARCH=" + goarch, "CGO_ENABLED=0"}
	args := append([]string{"list", "-deps", "-json=ImportPath,Dir,Standard,Module"}, patterns...)
	out, err := goCommand(ctx, env, args...)
	if err != nil {
		return nil, fmt.Errorf("list packages for %s: %w", platform, err)
	}
	var pkgs []listedPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			return pkgs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read go list for %s: %w", platform, err)
		}
		pkgs = append(pkgs, p)
	}
}

// collect groups the packages outside the standard library and the main module by module, sorted by path, and reads
// the licence and notice files between each package and its module root.
func collect(pkgs []listedPackage) ([]Module, error) {
	type found struct {
		mod   Module
		dir   string
		files map[string]bool
	}
	byPath := map[string]*found{}
	for _, p := range pkgs {
		if p.Standard {
			continue
		}
		m := p.Module
		switch {
		case m == nil:
			return nil, fmt.Errorf("package %s is in no module", p.ImportPath)
		case m.Main:
			continue
		case m.Replace != nil:
			return nil, fmt.Errorf("module %s %s is replaced by %s, and the notices cannot name its source",
				m.Path, m.Version, m.Replace.Path)
		case m.Dir == "":
			return nil, fmt.Errorf("module %s %s is not in the module cache: run go mod download", m.Path, m.Version)
		}
		f := byPath[m.Path]
		if f == nil {
			source, err := proxyURL(m.Path, m.Version)
			if err != nil {
				return nil, err
			}
			f = &found{Module{Path: m.Path, Version: m.Version, Source: source}, m.Dir, map[string]bool{}}
			byPath[m.Path] = f
		}
		names, err := licenceFiles(f.dir, p.Dir)
		if err != nil {
			return nil, fmt.Errorf("module %s: %w", m.Path, err)
		}
		for _, n := range names {
			f.files[n] = true
		}
	}

	mods := make([]Module, 0, len(byPath))
	for _, modPath := range slices.Sorted(maps.Keys(byPath)) {
		f := byPath[modPath]
		for _, name := range slices.Sorted(maps.Keys(f.files)) {
			text, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(name)))
			if err != nil {
				return nil, fmt.Errorf("module %s: %w", modPath, err)
			}
			f.mod.Files = append(f.mod.Files, File{Path: name, Text: string(text)})
		}
		mods = append(mods, f.mod)
	}
	return mods, nil
}

// licenceFiles returns the licence and notice files in pkgDir and in each directory above it up to modDir, as
// slash-separated paths from modDir.
func licenceFiles(modDir, pkgDir string) ([]string, error) {
	rel, err := filepath.Rel(modDir, pkgDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("package directory %s is outside the module directory %s", pkgDir, modDir)
	}
	var names []string
	for {
		entries, err := os.ReadDir(filepath.Join(modDir, rel))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() && licenceFile.MatchString(e.Name()) && filepath.Ext(e.Name()) != ".go" {
				names = append(names, path.Join(filepath.ToSlash(rel), e.Name()))
			}
		}
		if rel == "." {
			return names, nil
		}
		rel = filepath.Dir(rel)
	}
}

// proxyURL returns where the Go module proxy serves the source of a module version.
func proxyURL(modPath, version string) (string, error) {
	p, err := module.EscapePath(modPath)
	if err != nil {
		return "", err
	}
	v, err := module.EscapeVersion(version)
	if err != nil {
		return "", err
	}
	return "https://proxy.golang.org/" + p + "/@v/" + v + ".zip", nil
}

// stdlib returns the standard library of the go command that builds the program.
func stdlib(ctx context.Context) (Module, error) {
	out, err := goCommand(ctx, nil, "env", "GOVERSION")
	if err != nil {
		return Module{}, err
	}
	v := strings.TrimSpace(string(out))
	return Module{
		Path: "std", Version: v, Source: "https://go.dev/dl/" + v + ".src.tar.gz",
		Files: []File{{Path: "LICENSE", Text: goLicense}},
	}, nil
}

// licensesIn returns the licences the classifier finds in a text, sorted. A licence header, such as the Apache-2.0
// notice that go-yaml keeps in its LICENSE and NOTICE, counts too.
func licensesIn(c *classifier.Classifier, text string) []string {
	var names []string
	for _, m := range c.Match([]byte(text)).Matches {
		if m.MatchType == "License" || m.MatchType == "Header" {
			names = append(names, m.Name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// isNotice reports whether a file is a notice, which, unlike a licence file, need not name a licence.
func isNotice(f File) bool {
	return strings.HasPrefix(strings.ToLower(path.Base(f.Path)), "notice")
}

// Check returns an error, one line per problem, that names every module with a licence outside allowed, every
// licence file in which the classifier finds no licence (a proprietary or BUSL-1.1 text), and every module with no
// licence at all.
func Check(mods []Module, allowed []string) error {
	var errs []error
	for _, m := range mods {
		unknown := false
		for _, f := range m.Files {
			if len(f.Licenses) == 0 && !isNotice(f) {
				errs = append(errs, fmt.Errorf("%s %s: %s: no licence found", m.Path, m.Version, f.Path))
				unknown = true
			}
		}
		if !unknown && len(m.Licenses) == 0 {
			errs = append(errs, fmt.Errorf("%s %s: no licence found", m.Path, m.Version))
		}
		for _, l := range m.Licenses {
			if !slices.Contains(allowed, l) {
				errs = append(errs, fmt.Errorf("%s %s: %s is not an allowed licence", m.Path, m.Version, l))
			}
		}
	}
	return errors.Join(errs...)
}

const noticesHeader = `Third-party notices for tent

tent contains the Go standard library and the Go modules listed below. Each
entry gives the module path and version, the licences found in the module's
licence files, where to download its source code at that version, and the full
text of those files.
`

// Notices returns the third-party notices for the modules: for each, its path, version, licences, source and the
// text of its licence and notice files.
func Notices(mods []Module) []byte {
	var b strings.Builder
	b.WriteString(noticesHeader)
	for _, m := range mods {
		fmt.Fprintf(&b, "\n%s\n%s %s\nLicences: %s\nSource: %s\n",
			strings.Repeat("=", 80), m.Path, m.Version, strings.Join(m.Licenses, ", "), m.Source)
		for _, f := range m.Files {
			fmt.Fprintf(&b, "\n--- %s ---\n\n%s", f.Path, f.Text)
			if !strings.HasSuffix(f.Text, "\n") {
				b.WriteString("\n")
			}
		}
	}
	return []byte(b.String())
}
