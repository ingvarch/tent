package cloud_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
)

// createRequest returns a request for the first server of cluster prod with every field that Validate checks.
func createRequest() cloud.CreateRequest {
	return cloud.CreateRequest{
		Cluster: "prod", Group: "servers", Role: v1alpha1.RoleServer, Zone: "ams", Name: "prod-servers-0",
		MachineType: "vc2-2c-4gb", Image: "ubuntu-24.04", Op: "5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70",
	}
}

func TestCreateRequestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(r *cloud.CreateRequest)
		want string // the error's text; "" for none
	}{
		{"server", func(*cloud.CreateRequest) {}, ""},
		{"client", func(r *cloud.CreateRequest) { r.Role = v1alpha1.RoleClient }, ""},
		{"combined", func(r *cloud.CreateRequest) { r.Role = v1alpha1.RoleCombined }, ""},
		{"spec hash and user data", func(r *cloud.CreateRequest) {
			r.SpecHash, r.UserData = "3f9a1c0b7d2e4f68", []byte("#cloud-config\n")
		}, ""},
		{"no cluster", func(r *cloud.CreateRequest) { r.Cluster = "" }, "create request: no cluster"},
		{"no group", func(r *cloud.CreateRequest) { r.Group = "" }, "create request: no group"},
		{"no zone", func(r *cloud.CreateRequest) { r.Zone = "" }, "create request: no zone"},
		{"no name", func(r *cloud.CreateRequest) { r.Name = "" }, "create request: no name"},
		{"no machine type", func(r *cloud.CreateRequest) { r.MachineType = "" }, "create request: no machine type"},
		{"no image", func(r *cloud.CreateRequest) { r.Image = "" }, "create request: no image"},
		{"no operation id", func(r *cloud.CreateRequest) { r.Op = "" }, "create request: no operation id"},
		{"no role", func(r *cloud.CreateRequest) { r.Role = "" },
			`create request: role "" is not server, client or combined`},
		{"unknown role", func(r *cloud.CreateRequest) { r.Role = "worker" },
			`create request: role "worker" is not server, client or combined`},
		{"the first missing field", func(r *cloud.CreateRequest) { r.Zone, r.Op = "", "" }, "create request: no zone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := createRequest()
			tc.edit(&r)
			err := r.Validate()
			if got := errText(err); got != tc.want {
				t.Errorf("Validate() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUserDataNeverPrints checks that no way of printing or logging a create request shows its user data: only its
// size.
func TestUserDataNeverPrints(t *testing.T) {
	const secret = "gossip-key-Zm9vYmFyYmF6"
	r := createRequest()
	r.UserData = cloud.UserData("#cloud-config\nsecret: " + secret + "\n")
	const size = "[user data, 46 bytes]"
	// Every form the secret could take in the output: as it is, hex in either case, and base64.
	forms := []string{
		secret, hex.EncodeToString([]byte(secret)), strings.ToUpper(hex.EncodeToString([]byte(secret))),
		base64.StdEncoding.EncodeToString(r.UserData), "cloud-config",
	}
	outputs := map[string]string{}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10.3s"} {
		outputs["Sprintf "+verb+" of the request"] = fmt.Sprintf(verb, r)
		outputs["Sprintf "+verb+" of a pointer to the request"] = fmt.Sprintf(verb, &r)
		outputs["Sprintf "+verb+" of the user data"] = fmt.Sprintf(verb, r.UserData)
	}
	outputs["String"], outputs["GoString"] = r.UserData.String(), r.UserData.GoString()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	outputs["JSON"] = string(b)
	for name, h := range map[string]func(w io.Writer) slog.Handler{
		"slog JSON": func(w io.Writer) slog.Handler { return slog.NewJSONHandler(w, nil) },
		"slog text": func(w io.Writer) slog.Handler { return slog.NewTextHandler(w, nil) },
	} {
		var buf bytes.Buffer
		slog.New(h(&buf)).Info("create", "request", r, "pointer", &r, "user_data", r.UserData)
		outputs[name] = buf.String()
	}

	for name, out := range outputs {
		for _, form := range forms {
			if strings.Contains(out, form) {
				t.Errorf("%s shows the user data (%q): %s", name, form, out)
			}
		}
		if !strings.Contains(out, size) {
			t.Errorf("%s = %s, want the size %q", name, out, size)
		}
	}
	// The value itself stays the bytes that the provider sends.
	if got := string(r.UserData); !strings.Contains(got, secret) {
		t.Errorf("string(UserData) = %q, want the bytes as given", got)
	}
}

// errText returns the error's message, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
