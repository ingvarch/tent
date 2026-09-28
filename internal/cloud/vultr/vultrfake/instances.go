package vultrfake

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
)

// noInstance is the message of an answer about a missing instance.
const noInstance = "Invalid instance ID."

// The reads after which a new instance turns active, then ok, unless SetBootReads says otherwise.
const (
	defaultActiveAfter = 1
	defaultOKAfter     = 2
)

// instance is an instance with what the fake keeps beside govultr's fields. Its status fields hold the state after
// the boot, which it shows once GetInstance read it okAfter times.
type instance struct {
	govultr.Instance
	req      *govultr.InstanceCreateReq // the create request as sent; nil for a seeded instance
	userData string
	vpcs     []govultr.VPCInfo // in the order of attach_vpc

	reads       int // how many times GetInstance read it
	activeAfter int // the reads after which it shows as active
	okAfter     int // the reads after which it shows as ok
}

// view returns a copy of the instance as every call shows it now.
func (in *instance) view() govultr.Instance {
	v := in.Instance
	v.Tags, v.Features = slices.Clone(v.Tags), slices.Clone(v.Features)
	switch {
	case in.reads < in.activeAfter:
		v.Status, v.PowerStatus, v.ServerStatus, v.MainIP = "pending", "stopped", "none", "0.0.0.0"
	case in.reads < in.okAfter:
		v.ServerStatus = "installingbooting"
	}
	return v
}

// read returns the instance as a GetInstance shows it, and counts the read.
func (in *instance) read() govultr.Instance {
	v := in.view()
	in.reads++
	return v
}

// active reports whether the instance shows as active. Its VPC addresses are listed from then on.
func (in *instance) active() bool { return in.reads >= in.activeAfter }

// SetBootReads sets how many reads a new instance takes to boot. It shows as pending, stopped and none, with the
// main_ip 0.0.0.0, until GetInstance read it active times; then as active, running and installingbooting until
// GetInstance read it ok times; then as active, running and ok. Only GetInstance counts a read: ListInstances shows an
// instance as the next GetInstance will, so that lists running at the same time cannot change when it boots. The
// create answer shows it pending. The default is 1 and 2. It applies to the instances created after the call, and
// fails the test when active is negative or ok is below active.
func (f *Fake) SetBootReads(tb testing.TB, active, ok int) {
	tb.Helper()
	switch {
	case active < 0:
		tb.Fatalf("vultrfake: SetBootReads: active is %d, want 0 or more", active)
	case ok < active:
		tb.Fatalf("vultrfake: SetBootReads: ok is %d, want active (%d) or more", ok, active)
	default:
		f.mu.Lock()
		defer f.mu.Unlock()
		f.activeAfter, f.okAfter = active, ok
	}
}

// instance returns the instance with id, or nil. The caller holds the lock.
func (f *Fake) instance(id string) *instance {
	i := slices.IndexFunc(f.instances, func(in *instance) bool { return in.ID == id })
	if i < 0 {
		return nil
	}
	return f.instances[i]
}

// ListInstances returns every instance that has the tag, in any case, in creation order. It counts no read. It fails
// as the client does, without a call, when the tag is empty.
func (f *Fake) ListInstances(ctx context.Context, tag string) ([]govultr.Instance, error) {
	if err := vultr.CheckTag(tag); err != nil {
		return nil, err
	}
	var out []govultr.Instance
	r := request{name: "ListInstances", arg: tag, method: http.MethodGet, path: "/v2/instances"}
	err := f.run(ctx, r, func() error {
		for _, in := range f.instances {
			if slices.ContainsFunc(in.Tags, func(t string) bool { return strings.EqualFold(t, tag) }) {
				out = append(out, in.view())
			}
		}
		return nil
	})
	return result(out, err)
}

// GetInstance returns an instance, and counts a read.
func (f *Fake) GetInstance(ctx context.Context, id string) (*govultr.Instance, error) {
	if err := vultr.CheckID("GET /v2/instances/{id}", "id", id); err != nil {
		return nil, err
	}
	r := request{name: "GetInstance", arg: id, method: http.MethodGet, path: "/v2/instances/" + id}
	var out *govultr.Instance
	err := f.run(ctx, r, func() error {
		in := f.instance(id)
		if in == nil {
			return r.fail(http.StatusNotFound, noInstance)
		}
		v := in.read()
		out = &v
		return nil
	})
	return result(out, err)
}

// CreateInstance creates an instance and answers with it pending, with a default_password that no other answer
// holds. The instance keeps the label, hostname, tags, firewall group and user data of the request, and the request
// itself for CreateRequest. It gets a main_ip from 198.18.0.1 on, in creation order, which reads show once it is
// active. For each VPC of attach_vpc it gets a new MAC, 5a:00:04:00:00:01 and on, and the lowest free address of the
// VPC's subnet from its third host on, such as 10.64.0.3; no address when the VPC has no subnet or no free address.
//
// Like Vultr, it fails with vultr.ErrInvalid without a region or a plan, with an os_id that is not one of the fake's
// images, and with a VPC, firewall group or SSH key that does not exist, or a VPC in another region.
func (f *Fake) CreateInstance(ctx context.Context, req *govultr.InstanceCreateReq) (*govultr.Instance, error) {
	in := cloneCreateReq(deref(req))
	r := request{name: "CreateInstance", arg: in.Label, method: http.MethodPost, path: "/v2/instances"}
	var out *govultr.Instance
	err := f.run(ctx, r, func() error {
		if msg := f.checkCreate(in); msg != "" {
			return r.fail(http.StatusBadRequest, msg)
		}
		inst := &instance{
			Instance: govultr.Instance{
				ID: f.newID("instance"), Region: in.Region, Plan: in.Plan, OsID: in.OsID, Label: in.Label,
				Hostname: in.Hostname, Tags: slices.Clone(in.Tags), FirewallGroupID: in.FirewallGroupID,
				MainIP: f.newMainIP(), DateCreated: f.date(), Status: "active", PowerStatus: "running", ServerStatus: "ok",
			},
			req: &in, userData: in.UserData, activeAfter: f.activeAfter, okAfter: f.okAfter,
		}
		f.instances = append(f.instances, inst)
		f.attach(inst, in.AttachVPC)
		v := inst.view()
		v.Status, v.PowerStatus, v.ServerStatus, v.MainIP = "pending", "stopped", "none", "0.0.0.0"
		v.DefaultPassword = "vultrfake-password-" + v.ID
		out = &v
		return nil
	})
	return result(out, err)
}

// checkCreate returns Vultr's message for a create request that it refuses, or "" for one it takes. The caller holds
// the lock.
func (f *Fake) checkCreate(in govultr.InstanceCreateReq) string {
	switch {
	case in.Region == "":
		return "Invalid region."
	case in.Plan == "":
		return "Invalid plan."
	case !slices.ContainsFunc(f.images, func(o govultr.OS) bool { return o.ID == in.OsID }):
		return "Invalid os_id."
	}
	for _, id := range in.AttachVPC {
		v, ok := f.findVPC(id)
		switch {
		case !ok:
			return "Invalid VPC ID."
		case v.Region != in.Region:
			return "The VPC network is not in the instance's region."
		}
	}
	if in.FirewallGroupID != "" && f.group(in.FirewallGroupID) == nil {
		return noGroup
	}
	for _, id := range in.SSHKeys {
		if !slices.ContainsFunc(f.sshKeys, func(k govultr.SSHKey) bool { return k.ID == id }) {
			return "Invalid SSH key ID."
		}
	}
	return ""
}

// findVPC returns the VPC with id. The caller holds the lock.
func (f *Fake) findVPC(id string) (govultr.VPC, bool) {
	i := slices.IndexFunc(f.vpcs, func(v govultr.VPC) bool { return v.ID == id })
	if i < 0 {
		return govultr.VPC{}, false
	}
	return f.vpcs[i], true
}

// attach attaches in, which the fake holds, to the VPCs with vpcIDs, which exist, in order. The caller holds the
// lock.
func (f *Fake) attach(in *instance, vpcIDs []string) {
	for _, id := range vpcIDs {
		v, _ := f.findVPC(id)
		f.macs++
		mac := fmt.Sprintf("5a:00:04:%02x:%02x:%02x", byte(f.macs>>16), byte(f.macs>>8), byte(f.macs))
		in.vpcs = append(in.vpcs, govultr.VPCInfo{ID: id, MacAddress: mac, IPAddress: f.freeAddress(v)})
	}
}

// freeAddress returns the lowest address of v's subnet, from its third host on, that no instance holds; "" when the
// subnet is not valid or full. The caller holds the lock.
func (f *Fake) freeAddress(v govultr.VPC) string {
	base, err := netip.ParseAddr(v.V4Subnet)
	if err != nil {
		return ""
	}
	subnet, err := base.Prefix(v.V4SubnetMask)
	if err != nil {
		return ""
	}
	taken := map[string]bool{}
	for _, in := range f.instances {
		for _, a := range in.vpcs {
			if a.ID == v.ID {
				taken[a.IPAddress] = true
			}
		}
	}
	// The network address and the first two hosts are not given out, and nor is the broadcast address, the last.
	a := subnet.Addr().Next().Next().Next()
	for ; subnet.Contains(a.Next()); a = a.Next() {
		if !taken[a.String()] {
			return a.String()
		}
	}
	return ""
}

// newMainIP returns the next main_ip from 198.18.0.1 on. The caller holds the lock.
func (f *Fake) newMainIP() string {
	f.mainIPs++
	return netip.AddrFrom4([4]byte{198, 18, byte(f.mainIPs >> 8), byte(f.mainIPs)}).String()
}

// cloneCreateReq returns a deep copy of r.
func cloneCreateReq(r govultr.InstanceCreateReq) govultr.InstanceCreateReq {
	r.Tags, r.AttachVPC, r.SSHKeys = slices.Clone(r.Tags), slices.Clone(r.AttachVPC), slices.Clone(r.SSHKeys)
	r.BlockDevices, r.AppVariables = slices.Clone(r.BlockDevices), maps.Clone(r.AppVariables)
	for _, p := range []**bool{
		&r.EnableIPv6, &r.DisablePublicIPv4, &r.EnableVPC, &r.VPCOnly, &r.DDOSProtection, &r.ActivationEmail,
	} {
		if *p != nil {
			b := **p
			*p = &b
		}
	}
	return r
}

// DeleteInstance destroys an instance at once, with its VPC attachments. No call shows it after that.
func (f *Fake) DeleteInstance(ctx context.Context, id string) error {
	if err := vultr.CheckID("DELETE /v2/instances/{id}", "id", id); err != nil {
		return err
	}
	r := request{name: "DeleteInstance", arg: id, method: http.MethodDelete, path: "/v2/instances/" + id}
	return f.run(ctx, r, func() error {
		if !remove(&f.instances, id, func(in *instance) string { return in.ID }) {
			return r.fail(http.StatusNotFound, noInstance)
		}
		return nil
	})
}

// HaltInstance powers an instance off: its power_status becomes stopped, and stays so while it boots.
func (f *Fake) HaltInstance(ctx context.Context, id string) error {
	if err := vultr.CheckID("POST /v2/instances/{id}/halt", "id", id); err != nil {
		return err
	}
	r := request{name: "HaltInstance", arg: id, method: http.MethodPost, path: "/v2/instances/" + id + "/halt"}
	return f.run(ctx, r, func() error {
		in := f.instance(id)
		if in == nil {
			return r.fail(http.StatusNotFound, noInstance)
		}
		in.PowerStatus = "stopped"
		return nil
	})
}

// UpdateInstance changes an instance as Vultr does: the tags when req.Tags is not nil, so null keeps them; the label,
// the user data and the firewall group when they are not empty. It ignores the other fields. It fails with
// vultr.ErrInvalid, and changes nothing, when the firewall group does not exist.
func (f *Fake) UpdateInstance(ctx context.Context, id string, req *govultr.InstanceUpdateReq) error {
	if err := vultr.CheckID("PATCH /v2/instances/{id}", "id", id); err != nil {
		return err
	}
	up := deref(req)
	tags := slices.Clone(up.Tags)
	r := request{name: "UpdateInstance", arg: id, method: http.MethodPatch, path: "/v2/instances/" + id}
	return f.run(ctx, r, func() error {
		in := f.instance(id)
		switch {
		case in == nil:
			return r.fail(http.StatusNotFound, noInstance)
		case up.FirewallGroupID != "" && f.group(up.FirewallGroupID) == nil:
			return r.fail(http.StatusBadRequest, noGroup)
		}
		if tags != nil {
			in.Tags = tags
		}
		in.Label = cmp.Or(up.Label, in.Label)
		in.userData = cmp.Or(up.UserData, in.userData)
		in.FirewallGroupID = cmp.Or(up.FirewallGroupID, in.FirewallGroupID)
		return nil
	})
}

// ListInstanceVPCs returns the VPCs an instance is attached to, with its address and MAC in each, once it shows as
// active; none before.
func (f *Fake) ListInstanceVPCs(ctx context.Context, id string) ([]govultr.VPCInfo, error) {
	if err := vultr.CheckID("GET /v2/instances/{id}/vpcs", "id", id); err != nil {
		return nil, err
	}
	r := request{name: "ListInstanceVPCs", arg: id, method: http.MethodGet, path: "/v2/instances/" + id + "/vpcs"}
	var out []govultr.VPCInfo
	err := f.run(ctx, r, func() error {
		in := f.instance(id)
		if in == nil {
			return r.fail(http.StatusNotFound, noInstance)
		}
		if in.active() {
			out = clone(in.vpcs)
		}
		return nil
	})
	return result(out, err)
}

// attachedIPs returns the addresses of the instances attached to the VPC with id, in creation order. The caller holds
// the lock.
func (f *Fake) attachedIPs(id string) []string {
	var ips []string
	for _, in := range f.instances {
		for _, a := range in.vpcs {
			if a.ID == id {
				ips = append(ips, a.IPAddress)
			}
		}
	}
	return ips
}
