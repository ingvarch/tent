package buildconfig_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/internal/assets"
)

// releaseConfig is the part of .goreleaser.yaml the tests look at.
type releaseConfig struct {
	Before struct {
		Hooks []any `json:"hooks"`
	} `json:"before"`
	Builds   []releaseBuild   `json:"builds"`
	Archives []releaseArchive `json:"archives"`
	Checksum struct {
		NameTemplate string      `json:"name_template"`
		ExtraFiles   []extraFile `json:"extra_files"`
	} `json:"checksum"`
	Signs []struct {
		Cmd       string   `json:"cmd"`
		Signature string   `json:"signature"`
		Args      []string `json:"args"`
		Artifacts string   `json:"artifacts"`
	} `json:"signs"`
	SBOMs []struct {
		Artifacts string   `json:"artifacts"`
		IDs       []string `json:"ids"`
	} `json:"sboms"`
	Notarize struct {
		MacOS []struct {
			Enabled string   `json:"enabled"`
			IDs     []string `json:"ids"`
			Sign    struct {
				Certificate string `json:"certificate"`
				Password    string `json:"password"`
			} `json:"sign"`
			Notarize struct {
				IssuerID string `json:"issuer_id"`
				KeyID    string `json:"key_id"`
				Key      string `json:"key"`
				Wait     bool   `json:"wait"`
			} `json:"notarize"`
		} `json:"macos"`
	} `json:"notarize"`
	NFPMs []struct {
		IDs      []string      `json:"ids"`
		Formats  []string      `json:"formats"`
		License  string        `json:"license"`
		Contents []packageFile `json:"contents"`
	} `json:"nfpms"`
	HomebrewCasks []struct {
		IDs        []string `json:"ids"`
		SkipUpload string   `json:"skip_upload"`
		Repository struct {
			Owner  string `json:"owner"`
			Name   string `json:"name"`
			Branch string `json:"branch"`
			Token  string `json:"token"`
		} `json:"repository"`
	} `json:"homebrew_casks"`
	Changelog struct {
		Use string `json:"use"`
	} `json:"changelog"`
	Release struct {
		Draft      bool        `json:"draft"`
		Prerelease string      `json:"prerelease"`
		ExtraFiles []extraFile `json:"extra_files"`
	} `json:"release"`
}

// extraFile is a file the release publishes beside what it builds.
type extraFile struct {
	Glob string `json:"glob"`
}

// entry is the id of a build or an archive, by which other parts of .goreleaser.yaml select it.
type entry struct {
	ID string `json:"id"`
}

func (e entry) entryID() string { return e.ID }

// releaseBuild is a build of .goreleaser.yaml: one binary for several platforms.
type releaseBuild struct {
	entry
	Main    string   `json:"main"`
	Binary  string   `json:"binary"`
	Env     []string `json:"env"`
	Flags   []string `json:"flags"`
	Targets []string `json:"targets"`
	Ldflags []string `json:"ldflags"`
}

// releaseArchive is an archive of .goreleaser.yaml: the builds it packs (IDs) and how.
type releaseArchive struct {
	entry
	IDs             []string `json:"ids"`
	NameTemplate    string   `json:"name_template"`
	Files           []any    `json:"files"`
	Formats         []string `json:"formats"`
	FormatOverrides []struct {
		Goos    string   `json:"goos"`
		Formats []string `json:"formats"`
	} `json:"format_overrides"`
}

// bare reports whether the archive publishes each binary as it is, with no files beside it.
func (a releaseArchive) bare() bool { return slices.Equal(a.Formats, []string{"binary"}) }

// byID returns the one entry of a .goreleaser.yaml list, such as builds or archives, that has the id.
func byID[T interface{ entryID() string }](t *testing.T, list string, entries []T, id string) T {
	t.Helper()
	var found []T
	for _, e := range entries {
		if e.entryID() == id {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf(".goreleaser.yaml has %d %s with the id %q, want one", len(found), list, id)
	}
	return found[0]
}

// platform returns the goos/goarch of a GoReleaser target, such as linux/arm64 for linux_arm64_v8.0.
func platform(t *testing.T, target string) string {
	t.Helper()
	parts := strings.SplitN(target, "_", 3)
	if len(parts) < 2 {
		t.Fatalf("target %q is not goos_goarch", target)
	}
	return parts[0] + "/" + parts[1]
}

// packageFile is a file the deb and rpm packages install.
type packageFile struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

// release parses .goreleaser.yaml.
func release(t *testing.T) releaseConfig {
	t.Helper()
	var cfg releaseConfig
	if err := yaml.Unmarshal(repoFile(t, ".goreleaser.yaml"), &cfg); err != nil {
		t.Fatalf("parsing .goreleaser.yaml: %v", err)
	}
	return cfg
}

func TestReleaseBuildsTentForEverySupportedPlatform(t *testing.T) {
	tent := byID(t, "builds", release(t).Builds, "tent")
	if tent.Main != "./cmd/tent" || tent.Binary != "tent" {
		t.Fatalf("build tent = %+v, want ./cmd/tent as tent", tent)
	}
	// ADR-0013: linux, darwin and windows on amd64 and arm64.
	var platforms []string
	for _, target := range tent.Targets {
		platforms = append(platforms, platform(t, target))
	}
	want := []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"}
	if diff := cmp.Diff(want, platforms, cmpopts.SortSlices(strings.Compare)); diff != "" {
		t.Errorf("platforms (-want +got):\n%s", diff)
	}
}

func TestReleaseBuildsTentNodeForLinuxNodes(t *testing.T) {
	builds := release(t).Builds
	node := byID(t, "builds", builds, "tent-node")
	if node.Main != "./cmd/tent-node" || node.Binary != "tent-node" {
		t.Fatalf("build tent-node = %+v, want ./cmd/tent-node as tent-node", node)
	}
	// Nodes run Linux on amd64 or arm64; the lowest level of each runs on any such machine.
	want := []string{"linux_amd64_v1", "linux_arm64_v8.0"}
	if diff := cmp.Diff(want, node.Targets, cmpopts.SortSlices(strings.Compare)); diff != "" {
		t.Errorf("tent-node targets (-want +got):\n%s", diff)
	}
	// tent-node refuses to run when its version differs from the one tent gave its asset, so both are stamped alike.
	tent := byID(t, "builds", builds, "tent")
	for _, c := range []struct {
		name       string
		tent, node []string
	}{
		{"env", tent.Env, node.Env},
		{"flags", tent.Flags, node.Flags},
		{"ldflags", tent.Ldflags, node.Ldflags},
	} {
		if diff := cmp.Diff(c.tent, c.node); diff != "" {
			t.Errorf("tent-node builds with other %s than tent (-tent +tent-node):\n%s", c.name, diff)
		}
	}
}

func TestReleaseBuildsStaticBinaries(t *testing.T) {
	// A static binary runs on any Linux image, whatever libc it has.
	for _, b := range release(t).Builds {
		if !slices.Contains(b.Env, "CGO_ENABLED=0") {
			t.Errorf("build %q env %q lacks CGO_ENABLED=0", b.ID, b.Env)
		}
		if !slices.Contains(b.Flags, "-trimpath") {
			t.Errorf("build %q flags %q lack -trimpath", b.ID, b.Flags)
		}
	}
}

func TestReleaseStampsTheVersion(t *testing.T) {
	builds := release(t).Builds
	if len(builds) == 0 {
		t.Fatal(".goreleaser.yaml has no builds")
	}
	// The release says which tag, commit and date it was built from; the tag keeps its v.
	const pkg = "-X github.com/ingvarch/tent/internal/buildinfo."
	for _, b := range builds {
		ldflags := strings.Join(b.Ldflags, " ")
		for _, want := range []string{pkg + "version=v{{ .Version }}", pkg + "commit=", pkg + "date="} {
			if !strings.Contains(ldflags, want) {
				t.Errorf("build %q ldflags %q lack %q", b.ID, ldflags, want)
			}
		}
	}
}

func TestReleaseNamesTheChecksums(t *testing.T) {
	// The CLI finds tent-node's sha256 in checksums.txt of its own release (architecture section 8, ADR-0013).
	if got := release(t).Checksum.NameTemplate; got != "checksums.txt" {
		t.Errorf("checksum name_template = %q, want checksums.txt", got)
	}
}

func TestReleaseSignsTheChecksumsWithCosign(t *testing.T) {
	// ADR-0013: keyless cosign signatures; the bundle carries certificate and signature.
	signs := release(t).Signs
	if len(signs) != 1 || signs[0].Cmd != "cosign" || signs[0].Artifacts != "checksum" ||
		signs[0].Signature != "${artifact}.sigstore.json" {
		t.Fatalf("signs = %+v, want cosign over the checksums into ${artifact}.sigstore.json", signs)
	}
	want := []string{"sign-blob", "--bundle=${signature}", "${artifact}", "--yes"}
	if diff := cmp.Diff(want, signs[0].Args); diff != "" {
		t.Errorf("cosign args (-want +got):\n%s", diff)
	}
}

func TestReleaseWritesAnSBOMPerArchiveAndPerTentNode(t *testing.T) {
	// tent-node ships as a bare binary, which no archive SBOM covers.
	var got []string
	for _, s := range release(t).SBOMs {
		got = append(got, s.Artifacts+" "+strings.Join(s.IDs, ","))
	}
	want := []string{"archive ", "binary tent-node"}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(strings.Compare)); diff != "" {
		t.Errorf("sboms as artifacts and ids (-want +got):\n%s", diff)
	}
}

func TestReleaseZipsForWindows(t *testing.T) {
	tent := byID(t, "archives", release(t).Archives, "tent")
	if !cmp.Equal(tent.Formats, []string{"tar.gz"}) ||
		len(tent.FormatOverrides) != 1 || tent.FormatOverrides[0].Goos != "windows" ||
		!cmp.Equal(tent.FormatOverrides[0].Formats, []string{"zip"}) {
		t.Errorf("archive tent = %+v, want tar.gz with a zip for windows", tent)
	}
}

func TestReleasePublishesTentNodeAsTheFileTentReads(t *testing.T) {
	cfg := release(t)
	a := byID(t, "archives", cfg.Archives, "tent-node")
	// Nodes download the bare binary and check it against its line in checksums.txt (ADR-0026).
	if !cmp.Equal(a.IDs, []string{"tent-node"}) || !a.bare() || len(a.FormatOverrides) != 0 {
		t.Errorf("archive tent-node = %+v, want the build tent-node as a bare binary", a)
	}
	// GoReleaser names the file by the template; only the fields below exist here, so a template that needs more fails.
	tmpl, err := template.New("name_template").Option("missingkey=error").Parse(a.NameTemplate)
	if err != nil {
		t.Fatalf("archive tent-node name_template %q: %v", a.NameTemplate, err)
	}
	node := byID(t, "builds", cfg.Builds, "tent-node")
	for _, target := range node.Targets {
		goos, arch, _ := strings.Cut(platform(t, target), "/")
		fields := map[string]string{
			"ProjectName": "tent", "Version": "0.3.0", "Binary": node.Binary, "Os": goos, "Arch": arch,
		}
		var name strings.Builder
		if err := tmpl.Execute(&name, fields); err != nil {
			t.Fatalf("archive tent-node name_template %q for %s: %v", a.NameTemplate, target, err)
		}
		if want := assets.TentNodeFile(arch); goos != "linux" || name.String() != want {
			t.Errorf("the release names tent-node for %s %q; tent looks for %q", target, name.String(), want)
		}
	}
}

func TestTentsArchivesPackagesAndCaskHoldOnlyTent(t *testing.T) {
	// Without ids GoReleaser takes every build, and tent-node would land on operators' machines.
	cfg := release(t)
	want := []string{"tent"}
	for _, a := range cfg.Archives {
		if a.ID != "tent-node" && !cmp.Equal(a.IDs, want) {
			t.Errorf("archive %q packs the builds %q, want %q", a.ID, a.IDs, want)
		}
	}
	for i, n := range cfg.NFPMs {
		if !cmp.Equal(n.IDs, want) {
			t.Errorf("nfpms %d packs the builds %q, want %q", i, n.IDs, want)
		}
	}
	// A cask selects archives: tent's archive has the id tent.
	for i, c := range cfg.HomebrewCasks {
		if !cmp.Equal(c.IDs, want) {
			t.Errorf("homebrew_casks %d takes the archives %q, want %q", i, c.IDs, want)
		}
	}
}

func TestReleasePacksDebAndRpmWithTheLicense(t *testing.T) {
	nfpms := release(t).NFPMs
	if len(nfpms) != 1 || !cmp.Equal(nfpms[0].Formats, []string{"deb", "rpm"}) || nfpms[0].License != "Apache-2.0" {
		t.Fatalf("nfpms = %+v, want deb and rpm under Apache-2.0", nfpms)
	}
	// Apache-2.0 asks for the licence in every copy, and a package is a copy; the licences of what tent links ask for
	// their notices too.
	for _, file := range []string{"LICENSE", noticesFile} {
		if !slices.Contains(nfpms[0].Contents, packageFile{Src: file, Dst: "/usr/share/doc/tent/" + file}) {
			t.Errorf("the packages do not ship %s as /usr/share/doc/tent/%s: %+v", file, file, nfpms[0].Contents)
		}
	}
}

func TestReleasePublishesTheCaskToTheTap(t *testing.T) {
	casks := release(t).HomebrewCasks
	if len(casks) != 1 {
		t.Fatalf("homebrew_casks = %+v, want one", casks)
	}
	repo := casks[0].Repository
	if repo.Owner != "ingvarch" || repo.Name != "homebrew-tap" || repo.Branch != "main" ||
		repo.Token != "{{ .Env.HOMEBREW_TAP_GITHUB_TOKEN }}" {
		t.Errorf("cask repository = %+v, want ingvarch/homebrew-tap@main with the tap token", repo)
	}
	// A release candidate stays out of brew upgrade.
	if casks[0].SkipUpload != "auto" {
		t.Errorf("skip_upload = %q, want auto", casks[0].SkipUpload)
	}
}

func TestReleaseNotarizesForMacOS(t *testing.T) {
	macos := release(t).Notarize.MacOS
	if len(macos) != 1 {
		t.Fatalf("notarize.macos = %+v, want one", macos)
	}
	m := macos[0]
	// Only tent runs on macOS; tent-node is built for Linux alone.
	if !cmp.Equal(m.IDs, []string{"tent"}) {
		t.Errorf("notarize.macos ids = %q, want [tent]", m.IDs)
	}
	// A snapshot has no Apple keys; a release without them fails instead of shipping a binary macOS refuses.
	if m.Enabled != "{{ not .IsSnapshot }}" || !m.Notarize.Wait ||
		m.Sign.Certificate != "{{ .Env.MACOS_SIGN_P12 }}" || m.Sign.Password != "{{ .Env.MACOS_SIGN_PASSWORD }}" ||
		m.Notarize.IssuerID != "{{ .Env.MACOS_NOTARY_ISSUER_ID }}" || m.Notarize.KeyID != "{{ .Env.MACOS_NOTARY_KEY_ID }}" ||
		m.Notarize.Key != "{{ .Env.MACOS_NOTARY_KEY }}" {
		t.Errorf("notarize.macos = %+v", m)
	}
}

func TestReleaseNotesAndPublishing(t *testing.T) {
	cfg := release(t)
	// GitHub lists the merged pull requests and their authors.
	if cfg.Changelog.Use != "github-native" {
		t.Errorf("changelog use = %q, want github-native", cfg.Changelog.Use)
	}
	// A draft would serve brew a 404.
	if cfg.Release.Draft {
		t.Error("release draft = true, want false: publish at once")
	}
	// A -rc tag is not marked Latest.
	if cfg.Release.Prerelease != "auto" {
		t.Errorf("release prerelease = %q, want auto", cfg.Release.Prerelease)
	}
}

func TestReleaseWorkflow(t *testing.T) {
	if !strings.Contains(string(repoFile(t, ".github/workflows/release.yml")), `tags: ["v*"]`) {
		t.Error("release.yml does not run on v* tags")
	}
	// Keyless signing needs the job's OIDC token.
	if p := loadWorkflow(t, "release.yml").Permissions; p["contents"] != "write" || p["id-token"] != "write" {
		t.Errorf("permissions = %v, want contents and id-token write", p)
	}
	j := workflowJob(t, "release.yml", "release")
	// Keyless signing writes the repository and workflow to the public Rekor log, and brew cannot download private
	// release assets.
	if len(j.Steps) == 0 || j.Steps[0].If != "github.event.repository.private" || !strings.Contains(j.Steps[0].Run, "exit 1") {
		t.Errorf("%s does not refuse a private repository in its first step", j.where())
	}
	if stepUsing(t, j, "actions/checkout").with("fetch-depth") != "0" {
		t.Errorf("%s checks out a shallow clone; the notes need the tags", j.where())
	}
	stepUsing(t, j, "sigstore/cosign-installer")
	stepUsing(t, j, "anchore/sbom-action/download-syft")
	gr := stepUsing(t, j, "goreleaser/goreleaser-action")
	if gr.with("args") != "release --clean" {
		t.Errorf("%s: goreleaser args %q, want release --clean", j.where(), gr.with("args"))
	}
	for _, secret := range []string{"HOMEBREW_TAP_GITHUB_TOKEN", "MACOS_SIGN_P12", "MACOS_SIGN_PASSWORD",
		"MACOS_NOTARY_ISSUER_ID", "MACOS_NOTARY_KEY_ID", "MACOS_NOTARY_KEY"} {
		if gr.Env[secret] != "${{ secrets."+secret+" }}" {
			t.Errorf("%s does not pass %s to goreleaser", j.where(), secret)
		}
	}
}

func TestReleasePinsActionsByCommit(t *testing.T) {
	// The release job holds write tokens and every release secret, so a moved tag must not change its code.
	pinned := regexp.MustCompile(`^[\w.-]+/[\w.-]+(/[\w./-]+)?@[0-9a-f]{40}$`)
	for _, j := range loadWorkflow(t, "release.yml").Jobs {
		for _, s := range j.Steps {
			if s.Uses != "" && !pinned.MatchString(s.Uses) {
				t.Errorf("%s uses %s, not a full commit", j.where(), s.Uses)
			}
		}
	}
	// The comment names the release, which the retired-actions check reads.
	tagged := regexp.MustCompile(`@v\d+\.\d+\.\d+$`)
	for _, used := range loadWorkflow(t, "release.yml").actionVersions() {
		if !tagged.MatchString(used) {
			t.Errorf("release.yml uses %s without a # vX.Y.Z comment", used)
		}
	}
}

func TestCITriesTheReleaseLikeMakeDist(t *testing.T) {
	// A broken release config shows up in the pull request, and make dist builds the same thing.
	j := workflowJob(t, "ci.yml", "snapshot")
	stepUsing(t, j, "anchore/sbom-action/download-syft")
	args := stepUsing(t, j, "goreleaser/goreleaser-action").with("args")
	dist := recipe(t, "dist")
	if len(dist) != 1 || dist[0] != "goreleaser "+args || !strings.Contains(args, "--snapshot") {
		t.Errorf("%s runs goreleaser %q, make dist runs %q", j.where(), args, dist)
	}
}

// runChecksumsCheck runs the script of a CI step in a directory whose dist/checksums.txt holds checksums, or in one
// without the file when checksums is "", and returns its error.
func runChecksumsCheck(t *testing.T, script, checksums string) error {
	t.Helper()
	dir := t.TempDir()
	if checksums != "" {
		if err := os.Mkdir(filepath.Join(dir, "dist"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "dist", "checksums.txt"), []byte(checksums), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-e", "-c", script)
	cmd.Dir = dir
	return cmd.Run()
}

func TestCISnapshotChecksThatTheReleaseListsTentNode(t *testing.T) {
	// tent finds tent-node's sha256 in checksums.txt by these names; a pull request that drops them fails.
	j := workflowJob(t, "ci.yml", "snapshot")
	gr := slices.IndexFunc(j.Steps, func(s step) bool { return s.usesAction("goreleaser/goreleaser-action") })
	if gr < 0 {
		t.Fatalf("%s: no step uses goreleaser/goreleaser-action", j.where())
	}
	after := j.Steps[gr+1:]
	i := slices.IndexFunc(after, func(s step) bool { return strings.Contains(s.Run, "dist/checksums.txt") })
	if i < 0 {
		t.Fatalf("%s: no step after goreleaser reads dist/checksums.txt", j.where())
	}
	if runtime.GOOS == "windows" {
		t.Skip("the step runs under a POSIX shell")
	}
	line := func(name string) string { return strings.Repeat("1", 64) + "  " + name + "\n" }
	amd64, arm64 := assets.TentNodeFile("amd64"), assets.TentNodeFile("arm64")
	cases := []struct {
		name, checksums string
		pass            bool
	}{
		{"both", line("tent_0.3.0_linux_amd64.tar.gz") + line(amd64) + line(arm64), true},
		{"no arm64", line(amd64), false},
		{"no amd64", line(arm64), false},
		{"only their SBOMs", line(amd64+".sbom.json") + line(arm64+".sbom.json"), false},
		{"no checksums.txt", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := runChecksumsCheck(t, after[i].Run, c.checksums); (err == nil) != c.pass {
				t.Errorf("%s: the step on %q: err %v, want pass %v", j.where(), c.checksums, err, c.pass)
			}
		})
	}
}

func TestGoreleaserIsPinned(t *testing.T) {
	if !regexp.MustCompile(`(?m)^goreleaser \d+\.\d+\.\d+$`).Match(repoFile(t, ".tool-versions")) {
		t.Error(".tool-versions pins no goreleaser version")
	}
	for _, name := range workflowNames(t) {
		for _, j := range loadWorkflow(t, name).Jobs {
			for _, s := range j.Steps {
				if s.usesAction("goreleaser/goreleaser-action") && s.with("version-file") != ".tool-versions" {
					t.Errorf("%s runs a goreleaser not pinned in .tool-versions", j.where())
				}
			}
		}
	}
}
