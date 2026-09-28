package secrettest_test

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secrettest"
)

func TestShows(t *testing.T) {
	ca, err := pki.NewCA("prod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key := ca.Key().Bytes()
	block, _ := pem.Decode(key)
	gossip := pki.NewGossipKey().Bytes()
	decoded, err := base64.StdEncoding.DecodeString(string(gossip))
	if err != nil {
		t.Fatal(err)
	}
	token := pki.NewBootstrapSecret().Bytes()
	var line string // a line of the key's PEM body
	for l := range strings.Lines(string(key)) {
		if !strings.HasPrefix(l, "-----") {
			line = strings.TrimSpace(l)
			break
		}
	}
	for _, tc := range []struct {
		name   string
		text   string
		secret []byte
		want   bool
	}{
		{"the token as it is", "token: " + string(token) + ".", token, true},
		{"the token in hex", fmt.Sprintf("token %x", token), token, true},
		{"the token in upper-case hex", fmt.Sprintf("token %X", token), token, true},
		{"the token in base64", "token " + base64.StdEncoding.EncodeToString(token), token, true},
		{"the token as bytes in decimal", fmt.Sprintf("token %v", token), token, true},
		{"the token's size", pki.Secret(token).String(), token, false},
		{"another token", string(pki.NewBootstrapSecret()), token, false},
		{"the gossip key as it is", string(gossip), gossip, true},
		{"the gossip key's bytes in hex", hex.EncodeToString(decoded), gossip, true},
		{"the gossip key's bytes", string(decoded), gossip, true},
		{"the key's PEM", "key:\n" + string(key), key, true},
		{"the key's PEM in hex", hex.EncodeToString(key), key, true},
		{"the key's DER", string(block.Bytes), key, true},
		{"the key's DER in hex", hex.EncodeToString(block.Bytes), key, true},
		{"the key's DER in base64", base64.StdEncoding.EncodeToString(block.Bytes), key, true},
		{"a line of the key's PEM", line, key, true},
		{"the PEM's frame alone", "-----BEGIN PRIVATE KEY-----", key, false},
		{"another key", string(pki.Secret(ca.Bundle())), key, false},
		{"an empty secret", "anything", nil, false},
		{"bytes that start with a space, in decimal", fmt.Sprintf("%d", []byte(" \x01\x02")), []byte(" \x01\x02"), true},
		{"bytes that start with a space, in base64", base64.StdEncoding.EncodeToString([]byte(" \x01\x02")),
			[]byte(" \x01\x02"), true},
	} {
		if got := secrettest.Shows(tc.text, tc.secret); got != tc.want {
			t.Errorf("%s: Shows = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// TestPrinted checks that Printed prints a value with each fmt verb, alone and through a pointer, as JSON, and with
// both slog handlers.
func TestPrinted(t *testing.T) {
	got := secrettest.Printed(t, "tok")
	var names []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10.3s"} {
		names = append(names, "Sprintf "+verb, "Sprintf "+verb+" of a pointer")
		if want := fmt.Sprintf(verb, "tok"); got["Sprintf "+verb] != want {
			t.Errorf("Sprintf %s = %q, want %q", verb, got["Sprintf "+verb], want)
		}
	}
	names = append(names, "JSON", "slog JSON", "slog text")
	if diff := cmp.Diff(slices.Sorted(slices.Values(names)), slices.Sorted(maps.Keys(got))); diff != "" {
		t.Errorf("the outputs (-want +got):\n%s", diff)
	}
	for name, want := range map[string]string{
		"Sprintf %v of a pointer": "0x", "JSON": `"tok"`, "slog JSON": `"value":"tok"`, "slog text": "value=tok",
	} {
		if !strings.Contains(got[name], want) {
			t.Errorf("%s = %q, want it to hold %q", name, got[name], want)
		}
	}
}

// recorder is a testing.TB that records the messages of Errorf instead of failing the test.
type recorder struct {
	testing.TB
	errors []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func TestCheckHidden(t *testing.T) {
	outputs := map[string]string{
		"hidden":         "[secret, 3 bytes]",
		"shown in hex":   "[secret, 3 bytes] " + hex.EncodeToString([]byte("tok")),
		"not redacted":   "nothing",
		"shows the word": "[secret, 3 bytes] a word",
	}
	secrets := map[string][]byte{"the token": []byte("tok"), "the word": []byte("word")}
	r := &recorder{TB: t}
	secrettest.CheckHidden(r, outputs, secrets, "[secret, 3 bytes]")
	want := []string{
		`not redacted lacks the redaction "[secret, 3 bytes]"`,
		"shown in hex shows the token",
		"shows the word shows the word",
	}
	if diff := cmp.Diff(want, r.errors); diff != "" {
		t.Errorf("the failures (-want +got):\n%s", diff)
	}

	r = &recorder{TB: t}
	secrettest.CheckHidden(r, map[string]string{"plain": "nothing"}, secrets, "")
	if len(r.errors) != 0 {
		t.Errorf("failures %q with no redaction to check, want none", r.errors)
	}
}
