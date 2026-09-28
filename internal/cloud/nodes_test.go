package cloud_test

import (
	"strings"
	"testing"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/secrettest"
)

// createRequest returns a request for the first server of cluster prod with every field that Validate checks.
func createRequest() cloud.CreateRequest {
	return cloud.CreateRequest{
		Cluster: "prod", Group: "servers", Role: v1alpha1.RoleServer, Zone: "ams", Name: "prod-servers-0",
		MachineType: "vc2-2c-4gb", Image: "ubuntu-24.04", Op: "5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70",
	}
}

// notOpID is the end of the error of an operation id that NewOpID does not make.
const notOpID = "is not one that NewOpID makes (a lower-case UUID of version 4)"

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
		{"operation id not a UUID", func(r *cloud.CreateRequest) { r.Op = "OP-1" },
			`create request: operation id "OP-1" ` + notOpID},
		{"operation id in upper case", func(r *cloud.CreateRequest) { r.Op = strings.ToUpper(r.Op) },
			`create request: operation id "5F0C2A9E-8D1B-4C7E-9F3A-2B6D8E1C4A70" ` + notOpID},
		{"no role", func(r *cloud.CreateRequest) { r.Role = "" },
			`create request: role "" is not server, client or combined`},
		{"unknown role", func(r *cloud.CreateRequest) { r.Role = "worker" },
			`create request: role "worker" is not server, client or combined`},
		{"the first missing field", func(r *cloud.CreateRequest) { r.Zone, r.Op = "", "" }, "create request: no zone"},
		{"a missing field before a bad operation id", func(r *cloud.CreateRequest) { r.Zone, r.Op = "", "OP-1" },
			"create request: no zone"},
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
	outputs := map[string]string{"String": r.UserData.String(), "GoString": r.UserData.GoString()}
	for name, out := range secrettest.Printed(t, r) {
		outputs[name+" of the request"] = out
	}
	for name, out := range secrettest.Printed(t, r.UserData) {
		outputs[name+" of the user data"] = out
	}
	secrettest.CheckHidden(t, outputs, map[string][]byte{
		"the secret in the user data": []byte(secret), "the user data": r.UserData,
		"the cloud-config": []byte("cloud-config"),
	}, "[user data, 46 bytes]")
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
