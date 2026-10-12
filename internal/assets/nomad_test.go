package assets

import (
	"bytes"
	"context"
	"crypto"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/assets/assetstest"
	"github.com/ingvarch/tent/internal/channels"
)

// hashicorpFingerprint is the primary fingerprint of HashiCorp's release key, as HashiCorp publishes it.
const hashicorpFingerprint = "C874 011F 0AB4 0511 0D02 1055 3436 5D94 72D7 468F"

// testSums is a SHA256SUMS of Nomad 2.0.7 with made-up sums. The last two lines name files whose names hold the
// arm64 zip's name.
const testSums = `1111111111111111111111111111111111111111111111111111111111111111  nomad_2.0.7_darwin_arm64.zip
2222222222222222222222222222222222222222222222222222222222222222  nomad_2.0.7_linux_amd64.zip
3333333333333333333333333333333333333333333333333333333333333333  nomad_2.0.7_linux_arm64.zip
4444444444444444444444444444444444444444444444444444444444444444  nomad_2.0.7_linux_arm64.zip.sig
5555555555555555555555555555555555555555555555555555555555555555  xnomad_2.0.7_linux_arm64.zip
`

// t0 is when the test keys are made. They sign an hour later, and the checks run two hours later unless a test says
// otherwise.
var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// at returns a clock that stands at t0 plus d.
func at(d time.Duration) func() time.Time {
	t := t0.Add(d)
	return func() time.Time { return t }
}

// day returns a clock that stands at noon UTC of the date, such as 2026-09-28.
func day(t *testing.T, date string) func() time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, date)
	if err != nil {
		t.Fatal(err)
	}
	d = d.Add(12 * time.Hour)
	return func() time.Time { return d }
}

// newKey makes an OpenPGP key at t0 that expires after lifetime, or never when lifetime is 0.
func newKey(t *testing.T, lifetime time.Duration) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("tent test", "", "test@example.invalid", &packet.Config{
		Algorithm:       packet.PubKeyAlgoEdDSA,
		Time:            at(0),
		KeyLifetimeSecs: uint32(lifetime / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// armored returns key's public key, with its revocations, armored.
func armored(t *testing.T, key *openpgp.Entity) string {
	t.Helper()
	var b bytes.Buffer
	w, err := armor.Encode(&b, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := key.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// sign returns key's detached binary signature of data, made at t0 plus an hour, as HashiCorp's .sig files hold.
func sign(t *testing.T, key *openpgp.Entity, data string) string {
	t.Helper()
	var b bytes.Buffer
	if err := openpgp.DetachSign(&b, key, strings.NewReader(data), &packet.Config{Time: at(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// signWith is sign with the hash. It signs packet by packet, without the salt notation, which has no length for
// SHA-1: openpgp.DetachSign refuses SHA-1.
func signWith(t *testing.T, key *openpgp.Entity, data string, hash crypto.Hash) string {
	t.Helper()
	salted := false
	cfg := &packet.Config{Time: at(time.Hour), NonDeterministicSignaturesViaNotation: &salted}
	sig := &packet.Signature{
		Version:      key.PrimaryKey.Version,
		SigType:      packet.SigTypeBinary,
		PubKeyAlgo:   key.PrimaryKey.PubKeyAlgo,
		Hash:         hash,
		CreationTime: cfg.Now(),
		IssuerKeyId:  &key.PrimaryKey.KeyId,
	}
	h, err := sig.PrepareSign(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.Write([]byte(data))
	if err := sig.Sign(h, key.PrivateKey, cfg); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := sig.Serialize(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// flipLast returns s with the bits of its last byte flipped.
func flipLast(s string) string { return s[:len(s)-1] + string([]byte{s[len(s)-1] ^ 0xff}) }

func TestNomad(t *testing.T) {
	key, other := newKey(t, 0), newKey(t, 0)
	const sumsPath, sigPath = "/2.0.7/nomad_2.0.7_SHA256SUMS", "/2.0.7/nomad_2.0.7_SHA256SUMS.sig"
	good := map[string]string{sumsPath: testSums, sigPath: sign(t, key, testSums)}
	opts := func(srv *httptest.Server) Options {
		return Options{Client: srv.Client(), nomadURL: srv.URL, nomadKey: armored(t, key), Now: at(2 * time.Hour)}
	}

	for _, hash := range []crypto.Hash{crypto.SHA256, crypto.SHA384, crypto.SHA512} {
		t.Run("signed with "+hash.String(), func(t *testing.T) {
			srv := serve(t, map[string]string{sumsPath: testSums, sigPath: signWith(t, key, testSums, hash)})
			if _, err := Nomad(t.Context(), opts(srv), "2.0.7", "arm64"); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, arch := range []string{"amd64", "arm64"} {
		for _, slash := range []string{"", "/"} {
			t.Run("good "+arch+slash, func(t *testing.T) {
				srv := serve(t, good)
				o := opts(srv)
				o.nomadURL += slash
				got, err := Nomad(t.Context(), o, "2.0.7", arch)
				if err != nil {
					t.Fatal(err)
				}
				sum := map[string]string{"amd64": "2222", "arm64": "3333"}[arch]
				want := Asset{
					Name:    "nomad",
					Version: "2.0.7",
					URLs:    []string{srv.URL + "/2.0.7/nomad_2.0.7_linux_" + arch + ".zip"},
					SHA256:  strings.Repeat(sum, 16),
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("Nomad (-want +got):\n%s", diff)
				}
			})
		}
	}
	t.Run("another os", func(t *testing.T) {
		srv := serve(t, good)
		got, err := nomadFor(t.Context(), opts(srv), "2.0.7", "darwin", "arm64")
		if err != nil {
			t.Fatal(err)
		}
		want := Asset{
			Name:    "nomad",
			Version: "2.0.7",
			URLs:    []string{srv.URL + "/2.0.7/nomad_2.0.7_darwin_arm64.zip"},
			SHA256:  strings.Repeat("1111", 16),
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("nomadFor (-want +got):\n%s", diff)
		}
	})

	changedSums := strings.Replace(testSums, "2222", "2223", 1)
	withoutARM := strings.Replace(testSums, strings.Repeat("3", 64)+"  nomad_2.0.7_linux_arm64.zip\n", "", 1)
	for _, tc := range []struct {
		name  string
		files map[string]string
		cut   []string
		want  []string
	}{
		{"changed sums", map[string]string{sumsPath: changedSums, sigPath: good[sigPath]}, nil,
			[]string{sumsPath, sigPath, "invalid signature"}},
		{"changed signature", map[string]string{sumsPath: testSums, sigPath: flipLast(good[sigPath])}, nil,
			[]string{sumsPath, sigPath}},
		{"signed by another key", map[string]string{sumsPath: testSums, sigPath: sign(t, other, testSums)}, nil,
			[]string{sumsPath, sigPath, "unknown entity"}},
		{"signed with SHA-1", map[string]string{sumsPath: testSums, sigPath: signWith(t, key, testSums, crypto.SHA1)},
			nil, []string{sumsPath, sigPath, "hash algorithm"}},
		{"signed with SHA-224", map[string]string{sumsPath: testSums, sigPath: signWith(t, key, testSums, crypto.SHA224)},
			nil, []string{sumsPath, sigPath, "hash algorithm"}},
		{"no line for the arch", map[string]string{sumsPath: withoutARM, sigPath: sign(t, key, withoutARM)}, nil,
			[]string{sumsPath, "no line for nomad_2.0.7_linux_arm64.zip"}},
		{"no sums", map[string]string{sigPath: good[sigPath]}, nil, []string{sumsPath, "404"}},
		{"no signature", map[string]string{sumsPath: testSums}, nil, []string{sigPath, "404"}},
		{"sums cut", good, []string{sumsPath}, []string{sumsPath, "unexpected EOF"}},
		{"signature cut", good, []string{sigPath}, []string{sigPath, "unexpected EOF"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, tc.files, tc.cut...)
			a, err := Nomad(t.Context(), opts(srv), "2.0.7", "arm64")
			wantErr(t, err, tc.want...)
			if a.SHA256 != "" || a.URLs != nil {
				t.Errorf("Nomad failed and returned %+v", a)
			}
		})
	}
}

// countingTransport counts the requests that go through it.
type countingTransport struct {
	next http.RoundTripper
	n    atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.next.RoundTrip(r)
}

func TestCheckNomad(t *testing.T) {
	key, other := newKey(t, 0), newKey(t, 0)
	const sumsPath, sigPath = "/2.0.7/nomad_2.0.7_SHA256SUMS", "/2.0.7/nomad_2.0.7_SHA256SUMS.sig"
	good := map[string]string{sumsPath: testSums, sigPath: sign(t, key, testSums)}
	opts := func(srv *httptest.Server) Options {
		return Options{Client: srv.Client(), nomadURL: srv.URL, nomadKey: armored(t, key), Now: at(2 * time.Hour)}
	}
	both := []string{"amd64", "arm64"}
	changedSums := strings.Replace(testSums, "2222", "2223", 1)

	t.Run("both architectures after two requests", func(t *testing.T) {
		srv := serve(t, good)
		counter := &countingTransport{next: srv.Client().Transport}
		o := opts(srv)
		o.Client = &http.Client{Transport: counter}
		if err := CheckNomad(t.Context(), o, "2.0.7", both...); err != nil {
			t.Fatal(err)
		}
		if got := counter.n.Load(); got != 2 {
			t.Errorf("requests = %d, want 2: the sums and the signature", got)
		}
	})
	t.Run("no arch checks the signature alone", func(t *testing.T) {
		withoutLinux := strings.Repeat("1", 64) + "  nomad_2.0.7_darwin_arm64.zip\n"
		srv := serve(t, map[string]string{sumsPath: withoutLinux, sigPath: sign(t, key, withoutLinux)})
		if err := CheckNomad(t.Context(), opts(srv), "2.0.7"); err != nil {
			t.Fatal(err)
		}
		byOther := serve(t, map[string]string{sumsPath: withoutLinux, sigPath: sign(t, other, withoutLinux)})
		wantErr(t, CheckNomad(t.Context(), opts(byOther), "2.0.7"), sumsPath, sigPath, "unknown entity")
	})
	t.Run("a line of one architecture is missing", func(t *testing.T) {
		withoutARM := strings.Replace(testSums, strings.Repeat("3", 64)+"  nomad_2.0.7_linux_arm64.zip\n", "", 1)
		srv := serve(t, map[string]string{sumsPath: withoutARM, sigPath: sign(t, key, withoutARM)})
		err := CheckNomad(t.Context(), opts(srv), "2.0.7", both...)
		wantErr(t, err, sumsPath, "no line for nomad_2.0.7_linux_arm64.zip")
		if err := CheckNomad(t.Context(), opts(srv), "2.0.7", "amd64"); err != nil {
			t.Errorf("amd64 alone: %v", err)
		}
	})
	t.Run("a line of the first architecture is missing", func(t *testing.T) {
		withoutAMD := strings.Replace(testSums, strings.Repeat("2", 64)+"  nomad_2.0.7_linux_amd64.zip\n", "", 1)
		srv := serve(t, map[string]string{sumsPath: withoutAMD, sigPath: sign(t, key, withoutAMD)})
		err := CheckNomad(t.Context(), opts(srv), "2.0.7", both...)
		wantErr(t, err, sumsPath, "no line for nomad_2.0.7_linux_amd64.zip")
	})
	t.Run("a line that is no checksum", func(t *testing.T) {
		bad := strings.Replace(testSums, strings.Repeat("2", 64), "short", 1)
		srv := serve(t, map[string]string{sumsPath: bad, sigPath: sign(t, key, bad)})
		wantErr(t, CheckNomad(t.Context(), opts(srv), "2.0.7", both...), sumsPath, "line 2")
	})

	for _, tc := range []struct {
		name  string
		files map[string]string
		cut   []string
	}{
		{"changed sums", map[string]string{sumsPath: changedSums, sigPath: good[sigPath]}, nil},
		{"changed signature", map[string]string{sumsPath: testSums, sigPath: flipLast(good[sigPath])}, nil},
		{"signed by another key", map[string]string{sumsPath: testSums, sigPath: sign(t, other, testSums)}, nil},
		{"no sums", map[string]string{sigPath: good[sigPath]}, nil},
		{"no signature", map[string]string{sumsPath: testSums}, nil},
		{"sums cut", good, []string{sumsPath}},
		{"signature cut", good, []string{sigPath}},
	} {
		t.Run(tc.name+" fails with the text of Nomad", func(t *testing.T) {
			srv := serve(t, tc.files, tc.cut...)
			_, want := Nomad(t.Context(), opts(srv), "2.0.7", "arm64")
			if want == nil {
				t.Fatal("Nomad accepted the files")
			}
			got := CheckNomad(t.Context(), opts(srv), "2.0.7", both...)
			if got == nil || got.Error() != want.Error() {
				t.Errorf("CheckNomad = %v, want the error of Nomad: %v", got, want)
			}
		})
	}
	t.Run("embedded key", func(t *testing.T) {
		const url = "https://releases.hashicorp.com/nomad/2.0.7/nomad_2.0.7_SHA256SUMS"
		sums, sig := assetstest.NomadSums()
		client := &http.Client{Transport: sites{url: sums, url + ".sig": sig}}
		on := func(date string) Options { return Options{Client: client, Now: day(t, date)} }
		if err := CheckNomad(t.Context(), on("2026-09-28"), "2.0.7", both...); err != nil {
			t.Fatal(err)
		}
		err := CheckNomad(t.Context(), on("2030-03-02"), "2.0.7", both...)
		wantErr(t, err, url+".sig", "embedded in this tent expired on "+hashicorpKeyExpiry, "newer tent")
	})
}

func TestNomadChecksTheKeyAtTheClock(t *testing.T) {
	const sumsPath, sigPath = "/2.0.7/nomad_2.0.7_SHA256SUMS", "/2.0.7/nomad_2.0.7_SHA256SUMS.sig"
	expiring := newKey(t, 24*time.Hour)
	revoked := newKey(t, 0)
	revokedSig := sign(t, revoked, testSums) // before the revocation
	if err := revoked.RevokeKey(packet.KeyRetired, "test", &packet.Config{Time: at(90 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		key  *openpgp.Entity
		sig  string
		now  func() time.Time
		want []string // the error, or nothing
	}{
		{"before the key expires", expiring, sign(t, expiring, testSums), at(23 * time.Hour), nil},
		{"after the key expires", expiring, sign(t, expiring, testSums), at(25 * time.Hour),
			[]string{sumsPath, sigPath, "openpgp: key expired"}},
		{"before the revocation", revoked, revokedSig, at(80 * time.Minute), nil},
		{"after the revocation", revoked, revokedSig, at(2 * time.Hour), []string{sigPath, "revoked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, map[string]string{sumsPath: testSums, sigPath: tc.sig})
			opts := Options{Client: srv.Client(), nomadURL: srv.URL, nomadKey: armored(t, tc.key), Now: tc.now}
			_, err := Nomad(t.Context(), opts, "2.0.7", "amd64")
			if tc.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantErr(t, err, tc.want...)
			if strings.Contains(err.Error(), "HashiCorp") {
				t.Errorf("error %q names HashiCorp's key, but a test key made the signature", err)
			}
		})
	}
}

// TestNomadDefaults reads HashiCorp's own SHA256SUMS of Nomad 2.0.7 and its signature, as releases.hashicorp.com
// published them, from the default URL with the embedded key.
func TestNomadDefaults(t *testing.T) {
	const sumsURL = "https://releases.hashicorp.com/nomad/2.0.7/nomad_2.0.7_SHA256SUMS"
	sums, sig := assetstest.NomadSums()
	for _, tc := range []struct {
		name, sums, sig, date string
		want                  []string // the error, or nothing
	}{
		{"hashicorp", sums, sig, "2026-09-28", nil},
		{"on the day the key expires", sums, sig, hashicorpKeyExpiry, nil},
		{"after the key expires", sums, sig, "2030-03-02",
			[]string{sumsURL + ".sig", "HashiCorp's release key embedded in this tent expired on 2030-03-01",
				"newer tent"}},
		{"changed sums", strings.Replace(sums, "4c9b8a", "4c9b8b", 1), sig, "2026-09-28",
			[]string{sumsURL + ".sig", "invalid signature"}},
		{"signed by another key", sums, sign(t, newKey(t, 0), sums), "2026-09-28",
			[]string{sumsURL + ".sig", "unknown entity"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: sites{sumsURL: tc.sums, sumsURL + ".sig": tc.sig}}
			a, err := Nomad(t.Context(), Options{Client: client, Now: day(t, tc.date)}, "2.0.7", "amd64")
			if tc.want != nil {
				wantErr(t, err, tc.want...)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := Asset{
				Name:    "nomad",
				Version: "2.0.7",
				URLs:    []string{"https://releases.hashicorp.com/nomad/2.0.7/nomad_2.0.7_linux_amd64.zip"},
				SHA256:  "4c9b8a0850d6fd9caadbbab09b3e6fdf8b77aa777729543c70c61b85acca68c1",
			}
			if diff := cmp.Diff(want, a); diff != "" {
				t.Errorf("Nomad (-want +got):\n%s", diff)
			}
		})
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// embeddedKey returns the embedded key, which must be the only one in its file.
func embeddedKey(t *testing.T) *openpgp.Entity {
	t.Helper()
	keys, err := openpgp.ReadArmoredKeyRing(strings.NewReader(hashicorpKey))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("the embedded key file holds %d keys, want 1", len(keys))
	}
	return keys[0]
}

func TestEmbeddedKeyIsHashiCorps(t *testing.T) {
	got := strings.ToUpper(hex.EncodeToString(embeddedKey(t).PrimaryKey.Fingerprint))
	if want := strings.ReplaceAll(hashicorpFingerprint, " ", ""); got != want {
		t.Errorf("the embedded key's primary fingerprint is %s, want %s", got, want)
	}
}

// expires returns the date, in UTC, on which a key expires by its self-signature, or "never".
func expires(pk *packet.PublicKey, self *packet.Signature) string {
	if self.KeyLifetimeSecs == nil || *self.KeyLifetimeSecs == 0 {
		return "never"
	}
	return pk.CreationTime.Add(time.Duration(*self.KeyLifetimeSecs) * time.Second).UTC().Format(time.DateOnly)
}

func TestEmbeddedKeyExpiry(t *testing.T) {
	// A new key expires on another date: then hashicorpKeyExpiry, which errors name, changes with it.
	key := embeddedKey(t)
	primary, _ := key.PrimarySelfSignature()
	signing, ok := key.SigningKey(day(t, "2026-09-28")())
	if !ok {
		t.Fatal("the embedded key has no signing key on 2026-09-28")
	}
	for what, got := range map[string]string{
		"primary key":    expires(key.PrimaryKey, primary),
		"signing subkey": expires(signing.PublicKey, signing.SelfSignature),
	} {
		if got != hashicorpKeyExpiry {
			t.Errorf("the embedded %s expires on %s, want hashicorpKeyExpiry %s", what, got, hashicorpKeyExpiry)
		}
	}
}

// TestNomadOnline checks the real SHA256SUMS of the stable channel's recommended Nomad with the embedded key, and
// that the key has half a year left.
func TestNomadOnline(t *testing.T) {
	if os.Getenv("TENT_TEST_ONLINE") != "1" {
		t.Skip("reads releases.hashicorp.com; set TENT_TEST_ONLINE=1 to run it")
	}
	expiry, err := time.Parse(time.DateOnly, hashicorpKeyExpiry)
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(expiry); left < 180*24*time.Hour {
		t.Errorf("HashiCorp's release key embedded in tent expires on %s, in %d days: embed HashiCorp's renewed key "+
			"from https://www.hashicorp.com/.well-known/pgp-key.txt", hashicorpKeyExpiry, int(left.Hours()/24))
	}

	ch, err := channels.Load("stable")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	v := ch.Nomad.Recommended
	for _, arch := range []string{"amd64", "arm64"} {
		a, err := Nomad(ctx, Options{}, v, arch)
		if err != nil {
			t.Fatal(err)
		}
		wantURL := "https://releases.hashicorp.com/nomad/" + v + "/nomad_" + v + "_linux_" + arch + ".zip"
		if len(a.URLs) != 1 || a.URLs[0] != wantURL || len(a.SHA256) != 64 {
			t.Errorf("Nomad(%s, %s) = %+v, want %s and a sha256", v, arch, a, wantURL)
		}
		t.Logf("nomad %s linux/%s: %s", v, arch, a.SHA256)
	}
}
