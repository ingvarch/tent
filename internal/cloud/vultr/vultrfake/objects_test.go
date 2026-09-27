package vultrfake_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

func TestSSHKeys(t *testing.T) {
	const name = "tent:cluster=prod;kind=ssh-key;fp=1f2e3d4c"
	f := newFake()
	ctx := t.Context()
	var want []govultr.SSHKey
	for i, id := range []string{"ssh-key-1", "ssh-key-2"} { // names are not unique
		key := "ssh-ed25519 AAAA" + id
		got, err := f.CreateSSHKey(ctx, &govultr.SSHKeyReq{Name: name, SSHKey: key})
		if err != nil {
			t.Fatalf("CreateSSHKey %d: %v", i, err)
		}
		w := govultr.SSHKey{ID: id, Name: name, SSHKey: key, DateCreated: date}
		if diff := cmp.Diff(&w, got); diff != "" {
			t.Errorf("CreateSSHKey %d (-want +got):\n%s", i, diff)
		}
		want = append(want, w)
	}
	wantSSHKeys(t, f, want...)

	if err := f.DeleteSSHKey(ctx, "ssh-key-1"); err != nil {
		t.Fatalf("DeleteSSHKey: %v", err)
	}
	wantSSHKeys(t, f, want[1])
	wantAPIError(t, f.DeleteSSHKey(ctx, "ssh-key-1"), vultr.ErrNotFound,
		"vultr: DELETE /v2/ssh-keys/ssh-key-1: 404 Not Found: Invalid SSH key ID.")

	// An id is never given out again.
	if k := mustCreateSSHKey(t, f, "c"); k.ID != "ssh-key-3" {
		t.Errorf("the third key's id is %q, want ssh-key-3", k.ID)
	}
}

func TestCreateSSHKeyInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *govultr.SSHKeyReq
		text string
	}{
		{"no request", nil, "vultr: POST /v2/ssh-keys: 400 Bad Request: Invalid name."},
		{"no name", &govultr.SSHKeyReq{SSHKey: "ssh-ed25519 AAAA"}, "vultr: POST /v2/ssh-keys: 400 Bad Request: Invalid name."},
		{"no key", &govultr.SSHKeyReq{Name: "k"}, "vultr: POST /v2/ssh-keys: 400 Bad Request: Invalid SSH key."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			k, err := f.CreateSSHKey(t.Context(), tc.req)
			wantAPIError(t, err, vultr.ErrInvalid, tc.text)
			if k != nil {
				t.Errorf("CreateSSHKey returned %+v with the error", k)
			}
			wantSSHKeys(t, f)
		})
	}
}

// wantSSHKeys checks the fake's SSH keys.
func wantSSHKeys(t *testing.T, f *vultrfake.Fake, want ...govultr.SSHKey) {
	t.Helper()
	got, err := f.ListSSHKeys(t.Context())
	if err != nil {
		t.Fatalf("ListSSHKeys: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("SSH keys (-want +got):\n%s", diff)
	}
}

func TestVPCs(t *testing.T) {
	const marker = "tent:cluster=prod;kind=vpc"
	f := newFake()
	ctx := t.Context()
	got, err := f.CreateVPC(ctx, &govultr.VPCReq{
		Region: "ams", Description: marker, V4Subnet: "10.64.0.0", V4SubnetMask: 16,
	})
	if err != nil {
		t.Fatalf("CreateVPC: %v", err)
	}
	want := govultr.VPC{
		ID: "vpc-1", Region: "ams", Description: marker, V4Subnet: "10.64.0.0", V4SubnetMask: 16, DateCreated: date,
	}
	if diff := cmp.Diff(&want, got); diff != "" {
		t.Errorf("CreateVPC (-want +got):\n%s", diff)
	}
	wantVPCs(t, f, want)

	if err := f.DeleteVPC(ctx, "vpc-1"); err != nil {
		t.Fatalf("DeleteVPC: %v", err)
	}
	wantVPCs(t, f)
	wantAPIError(t, f.DeleteVPC(ctx, "vpc-1"), vultr.ErrNotFound,
		"vultr: DELETE /v2/vpcs/vpc-1: 404 Not Found: Invalid VPC ID.")
	wantCalls(t, f,
		vultrfake.Call{Name: "CreateVPC", Arg: marker},
		vultrfake.Call{Name: "ListVPCs"},
		vultrfake.Call{Name: "DeleteVPC", Arg: "vpc-1"},
		vultrfake.Call{Name: "ListVPCs"},
		vultrfake.Call{Name: "DeleteVPC", Arg: "vpc-1"},
	)
}

func TestCreateVPCNeedsRegion(t *testing.T) {
	for _, req := range []*govultr.VPCReq{nil, {Description: "tent:cluster=prod;kind=vpc"}} {
		f := newFake()
		v, err := f.CreateVPC(t.Context(), req)
		wantAPIError(t, err, vultr.ErrInvalid, "vultr: POST /v2/vpcs: 400 Bad Request: Invalid region.")
		if v != nil {
			t.Errorf("CreateVPC returned %+v with the error", v)
		}
		wantVPCs(t, f)
	}
}

func TestVPCLimitPerRegion(t *testing.T) {
	f := newFake()
	ctx := t.Context()
	create := func(region string) (*govultr.VPC, error) {
		return f.CreateVPC(ctx, &govultr.VPCReq{Region: region, Description: "d"})
	}
	for i := range 5 {
		if _, err := create("ams"); err != nil {
			t.Fatalf("VPC %d in ams: %v", i+1, err)
		}
	}
	v, err := create("ams")
	wantAPIError(t, err, vultr.ErrLimitReached,
		"vultr: POST /v2/vpcs: 400 Bad Request: You have reached the maximum number of VPC networks in this region.")
	if v != nil {
		t.Errorf("the sixth VPC in ams is %+v, want none", v)
	}
	if _, err := create("fra"); err != nil {
		t.Errorf("a VPC in fra: %v", err)
	}
	if err := f.DeleteVPC(ctx, "vpc-3"); err != nil {
		t.Fatalf("DeleteVPC: %v", err)
	}
	if v, err := create("ams"); err != nil || v.ID != "vpc-7" {
		t.Errorf("CreateVPC after a delete = %+v, %v; want vpc-7", v, err)
	}
}

// wantVPCs checks the fake's VPCs.
func wantVPCs(t *testing.T, f *vultrfake.Fake, want ...govultr.VPC) {
	t.Helper()
	got, err := f.ListVPCs(t.Context())
	if err != nil {
		t.Fatalf("ListVPCs: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("VPCs (-want +got):\n%s", diff)
	}
}
