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
	dir := releaseDir(opts.nomadURL, nomadReleases, version)
	sumsURL := dir + "nomad_" + version + "_SHA256SUMS"
	sigURL := sumsURL + ".sig"
	sums, err := opts.get(ctx, sumsURL)
	if err != nil {
		return Asset{}, err
	}
	sig, err := opts.get(ctx, sigURL)
	if err != nil {
		return Asset{}, err
	}
	err = verify(cmp.Or(opts.nomadKey, hashicorpKey), sums, sig, opts.now)
	if errors.Is(err, pgperrors.ErrKeyExpired) && opts.nomadKey == "" {
		err = fmt.Errorf("HashiCorp's release key embedded in this tent expired on %s: a newer tent, with the "+
			"renewed key, is needed: %w", hashicorpKeyExpiry, err)
	}
	if err != nil {
		return Asset{}, fmt.Errorf("verify %s with %s: %w", sumsURL, sigURL, err)
	}
	file := "nomad_" + version + "_linux_" + arch + ".zip"
	sum, err := sumOf(sumsURL, sums, file)
	if err != nil {
		return Asset{}, err
	}
	return Asset{Name: "nomad", Version: version, URLs: []string{dir + file}, SHA256: sum}, nil
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
