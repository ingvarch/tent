// Package secrettest holds the checks that tests run to find a secret in what a program prints or logs.
package secrettest

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
)

// Shows reports whether text shows the secret in any of the forms that a leak takes: as it is, and without the white
// space around it; in lower- or upper-case hex; in standard base64; as the decimal bytes that fmt prints for a []byte;
// and each line of a PEM secret's body. When the secret encodes bytes, as the DER of a PEM key or the bytes of a base64
// gossip key, those bytes count in the same forms. A secret of white space alone shows nowhere.
func Shows(text string, secret []byte) bool {
	if len(bytes.TrimSpace(secret)) == 0 {
		return false
	}
	for _, form := range forms(secret) {
		if strings.Contains(text, form) {
			return true
		}
	}
	return false
}

// Printed returns every way of printing or logging v, by name: fmt with each verb, for v and a pointer to it, JSON,
// and slog with its JSON and text handlers. A failure to encode JSON stops t.
func Printed[T any](t testing.TB, v T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10.3s"} {
		out["Sprintf "+verb] = fmt.Sprintf(verb, v)
		out["Sprintf "+verb+" of a pointer"] = fmt.Sprintf(verb, &v)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	out["JSON"] = string(b)
	for name, h := range map[string]func(w io.Writer) slog.Handler{
		"slog JSON": func(w io.Writer) slog.Handler { return slog.NewJSONHandler(w, nil) },
		"slog text": func(w io.Writer) slog.Handler { return slog.NewTextHandler(w, nil) },
	} {
		var buf bytes.Buffer
		slog.New(h(&buf)).Info("print", "value", v, "pointer", &v)
		out[name] = buf.String()
	}
	return out
}

// CheckHidden fails t for every output that shows one of the secrets, as Shows looks for them, or lacks the
// redaction; an empty redaction is not checked. Its messages name the output and the secret, and never print either.
func CheckHidden(t testing.TB, outputs map[string]string, secrets map[string][]byte, redaction string) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(outputs)) {
		out := outputs[name]
		for _, secret := range slices.Sorted(maps.Keys(secrets)) {
			if Shows(out, secrets[secret]) {
				t.Errorf("%s shows %s", name, secret)
			}
		}
		if !strings.Contains(out, redaction) {
			t.Errorf("%s lacks the redaction %q", name, redaction)
		}
	}
}

// forms returns the forms of the secret that Shows looks for.
func forms(secret []byte) []string {
	raw := bytes.TrimSpace(secret)
	contents := [][]byte{raw}
	if len(raw) < len(secret) {
		// Binary bytes, such as a key's scalar, may start or end with bytes that read as white space.
		contents = append(contents, secret)
	}
	var all []string
	isPEM := false
	for block, rest := pem.Decode(raw); block != nil; block, rest = pem.Decode(rest) {
		contents, isPEM = append(contents, block.Bytes), true
	}
	if isPEM {
		for line := range strings.Lines(string(raw)) {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "-----") {
				all = append(all, line)
			}
		}
	}
	if decoded, err := base64.StdEncoding.DecodeString(string(raw)); err == nil && len(decoded) > 0 {
		contents = append(contents, decoded)
	}
	for _, c := range contents {
		h := hex.EncodeToString(c)
		all = append(all, string(c), h, strings.ToUpper(h), base64.StdEncoding.EncodeToString(c), fmt.Sprintf("%d", c))
	}
	return all
}
