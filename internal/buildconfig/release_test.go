package buildconfig_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"sigs.k8s.io/yaml"
)

// releaseConfig is the part of .goreleaser.yaml the tests look at.
type releaseConfig struct {
	Before struct {
		Hooks []any `json:"hooks"`
	} `json:"before"`
	Builds []struct {
		Main    string   `json:"main"`
		Binary  string   `json:"binary"`
		Env     []string `json:"env"`
		Targets []string `json:"targets"`
		Ldflags []string `json:"ldflags"`
	} `json:"builds"`
	Archives []struct {
		Files           []any    `json:"files"`
		Formats         []string `json:"formats"`
		FormatOverrides []struct {
			Goos    string   `json:"goos"`
			Formats []string `json:"formats"`
		} `json:"format_overrides"`
	} `json:"archives"`
	Checksum struct {
		NameTemplate string `json:"name_template"`
	} `json:"checksum"`
	Signs []struct {
		Cmd       string   `json:"cmd"`
		Signature string   `json:"signature"`
		Args      []string `json:"args"`
		Artifacts string   `json:"artifacts"`
	} `json:"signs"`
	SBOMs []struct {
		Artifacts string `json:"artifacts"`
	} `json:"sboms"`
	Notarize struct {
		MacOS []struct {
			Enabled string `json:"enabled"`
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
		Formats  []string      `json:"formats"`
		License  string        `json:"license"`
		Contents []packageFile `json:"contents"`
	} `json:"nfpms"`
	HomebrewCasks []struct {
		SkipUpload string `json:"skip_upload"`
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
		Draft      bool   `json:"draft"`
		Prerelease string `json:"prerelease"`
	} `json:"release"`
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
	cfg := release(t)
	if len(cfg.Builds) != 1 || cfg.Builds[0].Main != "./cmd/tent" || cfg.Builds[0].Binary != "tent" {
		t.Fatalf("builds = %+v, want one build of ./cmd/tent", cfg.Builds)
	}
	// ADR-0013: linux, darwin and windows on amd64 and arm64.
	var platforms []string
	for _, target := range cfg.Builds[0].Targets {
		parts := strings.SplitN(target, "_", 3)
		if len(parts) < 2 {
			t.Fatalf("target %q is not goos_goarch", target)
		}
		platforms = append(platforms, parts[0]+"/"+parts[1])
	}
	want := []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"}
	if diff := cmp.Diff(want, platforms, cmpopts.SortSlices(strings.Compare)); diff != "" {
		t.Errorf("platforms (-want +got):\n%s", diff)
	}
	// A static binary runs on any Linux image, whatever libc it has.
	if !slices.Contains(cfg.Builds[0].Env, "CGO_ENABLED=0") {
		t.Errorf("build env %q lacks CGO_ENABLED=0", cfg.Builds[0].Env)
	}
}

func TestReleaseStampsTheVersion(t *testing.T) {
	builds := release(t).Builds
	if len(builds) != 1 {
		t.Fatalf("builds = %+v, want one", builds)
	}
	ldflags := strings.Join(builds[0].Ldflags, " ")
	// The release says which tag, commit and date it was built from; the tag keeps its v.
	const pkg = "-X github.com/ingvarch/tent/internal/buildinfo."
	for _, want := range []string{pkg + "version=v{{ .Version }}", pkg + "commit=", pkg + "date="} {
		if !strings.Contains(ldflags, want) {
			t.Errorf("ldflags %q lack %q", ldflags, want)
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

func TestReleaseWritesAnSBOMPerArchive(t *testing.T) {
	sboms := release(t).SBOMs
	if len(sboms) != 1 || sboms[0].Artifacts != "archive" {
		t.Errorf("sboms = %+v, want one per archive", sboms)
	}
}

func TestReleaseZipsForWindows(t *testing.T) {
	archives := release(t).Archives
	if len(archives) != 1 || !cmp.Equal(archives[0].Formats, []string{"tar.gz"}) ||
		len(archives[0].FormatOverrides) != 1 || archives[0].FormatOverrides[0].Goos != "windows" ||
		!cmp.Equal(archives[0].FormatOverrides[0].Formats, []string{"zip"}) {
		t.Errorf("archives = %+v, want tar.gz with a zip for windows", archives)
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
