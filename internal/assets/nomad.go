package assets

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	_ "embed" // hashicorpKey
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/ingvarch/tent/internal/nodeconfig"
)

// hashicorpKey is HashiCorp's release signing key, armored, from https://www.hashicorp.com/.well-known/pgp-key.txt.
//
//go:embed hashicorp.asc
var hashicorpKey string

// hashicorpKeyExpiry is the date, in UTC, on which the embedded key and its signing subkey expire.
const hashicorpKeyExpiry = "2030-03-01"

// signatureHashes are the hashes a signature of Nomad's SHA256SUMS may use.
var signatureHashes = []crypto.Hash{crypto.SHA256, crypto.SHA384, crypto.SHA512}

// Nomad returns the Nomad release for linux on arch (amd64 or arm64) with the sha256 that nomad_<version>_SHA256SUMS
// lists for it. The file must verify with its detached signature and HashiCorp's release key, which tent embeds.
func Nomad(ctx context.Context, opts Options, version, arch string) (Asset, error) {
	return nomadFor(ctx, opts, version, "linux", arch)
}

// nomadFor is Nomad for the operating system goos: nodes run linux, tests the system they run on.
func nomadFor(ctx context.Context, opts Options, version, goos, arch string) (Asset, error) {
	dir, sumsURL, sums, err := verifiedNomadSums(ctx, opts, version)
	if err != nil {
		return Asset{}, err
	}
	file := nomadZip(version, goos, arch)
	sum, err := sumOf(sumsURL, sums, file)
	if err != nil {
		return Asset{}, err
	}
	return Asset{Name: nodeconfig.NomadAsset, Version: version, URLs: []string{dir + file}, SHA256: sum}, nil
}

// CheckNomad checks that Nomad version is a release that nodes of every arch in archs can download. It reads
// nomad_<version>_SHA256SUMS and its detached signature, one request each, verifies them as Nomad does, and fails
// unless the file lists the linux zip of each arch. Called without an arch, it checks the signature alone. It
// downloads no zip.
func CheckNomad(ctx context.Context, opts Options, version string, archs ...string) error {
	_, sumsURL, sums, err := verifiedNomadSums(ctx, opts, version)
	if err != nil {
		return err
	}
	for _, arch := range archs {
		if _, err := sumOf(sumsURL, sums, nomadZip(version, "linux", arch)); err != nil {
			return err
		}
	}
	return nil
}

// nomadZip is the name of the Nomad release file for goos and arch.
func nomadZip(version, goos, arch string) string {
	return "nomad_" + version + "_" + goos + "_" + arch + ".zip"
}

// verifiedNomadSums reads nomad_<version>_SHA256SUMS and its signature, verifies them with HashiCorp's release key,
// and returns the release directory's URL, the file's URL and its content.
func verifiedNomadSums(ctx context.Context, opts Options, version string) (dir, sumsURL string, sums []byte,
	err error) {
	dir = releaseDir(opts.nomadURL, nomadReleases, version)
	sumsURL = dir + "nomad_" + version + "_SHA256SUMS"
	sigURL := sumsURL + ".sig"
	sums, err = opts.get(ctx, sumsURL)
	if err != nil {
		return "", "", nil, err
	}
	sig, err := opts.get(ctx, sigURL)
	if err != nil {
		return "", "", nil, err
	}
	err = verify(cmp.Or(opts.nomadKey, hashicorpKey), sums, sig, opts.Now)
	if errors.Is(err, pgperrors.ErrKeyExpired) && opts.nomadKey == "" {
		err = fmt.Errorf("HashiCorp's release key embedded in this tent expired on %s: a newer tent, with the "+
			"renewed key, is needed: %w", hashicorpKeyExpiry, err)
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("verify %s with %s: %w", sumsURL, sigURL, err)
	}
	return dir, sumsURL, sums, nil
}

// verify checks that sig is a detached signature of signed by the armored key, with one of signatureHashes, and that
// the key was neither expired nor revoked at now (time.Now when nil).
func verify(armoredKey string, signed, sig []byte, now func() time.Time) error {
	keys, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredKey))
	if err != nil {
		return fmt.Errorf("read the release key: %w", err)
	}
	_, err = openpgp.CheckDetachedSignatureAndHash(keys, bytes.NewReader(signed), bytes.NewReader(sig), signatureHashes,
		&packet.Config{Time: now})
	return err
}
