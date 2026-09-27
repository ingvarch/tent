package vultrfake

import (
	"context"
	"net/http"
	"slices"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
)

// ListSSHKeys returns every SSH key.
func (f *Fake) ListSSHKeys(ctx context.Context) ([]govultr.SSHKey, error) {
	var out []govultr.SSHKey
	err := f.run(ctx, request{name: "ListSSHKeys", method: http.MethodGet, path: "/v2/ssh-keys"}, func() error {
		out = clone(f.sshKeys)
		return nil
	})
	return result(out, err)
}

// CreateSSHKey adds an SSH key. It fails with vultr.ErrInvalid without a name or a key. Names need not be unique.
func (f *Fake) CreateSSHKey(ctx context.Context, req *govultr.SSHKeyReq) (*govultr.SSHKey, error) {
	in := deref(req)
	r := request{name: "CreateSSHKey", arg: in.Name, method: http.MethodPost, path: "/v2/ssh-keys"}
	var out *govultr.SSHKey
	err := f.run(ctx, r, func() error {
		switch {
		case in.Name == "":
			return r.fail(http.StatusBadRequest, "Invalid name.")
		case in.SSHKey == "":
			return r.fail(http.StatusBadRequest, "Invalid SSH key.")
		}
		k := govultr.SSHKey{ID: f.newID("ssh-key"), Name: in.Name, SSHKey: in.SSHKey, DateCreated: f.date()}
		f.sshKeys = append(f.sshKeys, k)
		out = &k
		return nil
	})
	return result(out, err)
}

// DeleteSSHKey removes an SSH key.
func (f *Fake) DeleteSSHKey(ctx context.Context, id string) error {
	if err := vultr.CheckID("DELETE /v2/ssh-keys/{id}", "id", id); err != nil {
		return err
	}
	r := request{name: "DeleteSSHKey", arg: id, method: http.MethodDelete, path: "/v2/ssh-keys/" + id}
	return f.run(ctx, r, func() error {
		if !remove(&f.sshKeys, id, func(k govultr.SSHKey) string { return k.ID }) {
			return r.fail(http.StatusNotFound, "Invalid SSH key ID.")
		}
		return nil
	})
}

// maxVPCsPerRegion is the most VPCs a region holds.
const maxVPCsPerRegion = 5

// ListVPCs returns every VPC, in all regions.
func (f *Fake) ListVPCs(ctx context.Context) ([]govultr.VPC, error) {
	var out []govultr.VPC
	err := f.run(ctx, request{name: "ListVPCs", method: http.MethodGet, path: "/v2/vpcs"}, func() error {
		out = clone(f.vpcs)
		return nil
	})
	return result(out, err)
}

// CreateVPC creates a VPC with the subnet as given; unlike Vultr, it assigns none when the request has none. It fails
// with vultr.ErrInvalid without a region, and with vultr.ErrLimitReached when the region holds 5 VPCs.
func (f *Fake) CreateVPC(ctx context.Context, req *govultr.VPCReq) (*govultr.VPC, error) {
	in := deref(req)
	r := request{name: "CreateVPC", arg: in.Description, method: http.MethodPost, path: "/v2/vpcs"}
	var out *govultr.VPC
	err := f.run(ctx, r, func() error {
		switch {
		case in.Region == "":
			return r.fail(http.StatusBadRequest, "Invalid region.")
		case f.countVPCs(in.Region) >= maxVPCsPerRegion:
			return r.fail(http.StatusBadRequest, "You have reached the maximum number of VPC networks in this region.")
		}
		v := govultr.VPC{
			ID: f.newID("vpc"), Region: in.Region, Description: in.Description, V4Subnet: in.V4Subnet,
			V4SubnetMask: in.V4SubnetMask, DateCreated: f.date(),
		}
		f.vpcs = append(f.vpcs, v)
		out = &v
		return nil
	})
	return result(out, err)
}

// countVPCs returns how many VPCs a region holds. The caller holds the lock.
func (f *Fake) countVPCs(region string) int {
	n := 0
	for _, v := range f.vpcs {
		if v.Region == region {
			n++
		}
	}
	return n
}

// DeleteVPC deletes a VPC.
func (f *Fake) DeleteVPC(ctx context.Context, id string) error {
	if err := vultr.CheckID("DELETE /v2/vpcs/{id}", "id", id); err != nil {
		return err
	}
	r := request{name: "DeleteVPC", arg: id, method: http.MethodDelete, path: "/v2/vpcs/" + id}
	return f.run(ctx, r, func() error {
		if !remove(&f.vpcs, id, func(v govultr.VPC) string { return v.ID }) {
			return r.fail(http.StatusNotFound, "Invalid VPC ID.")
		}
		return nil
	})
}

// remove removes the object whose key is key from objs, and reports whether there was one.
func remove[T any, K comparable](objs *[]T, key K, keyOf func(T) K) bool {
	i := slices.IndexFunc(*objs, func(o T) bool { return keyOf(o) == key })
	if i < 0 {
		return false
	}
	*objs = slices.Delete(*objs, i, i+1)
	return true
}
