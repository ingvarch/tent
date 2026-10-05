package vultr

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
)

var _ cloud.Nodes = (*Provider)(nil)

// joinedTag is the tag of a machine whose node has joined the cluster.
const joinedTag = cloud.LabelJoined + "=true"

// taggedInstance is an instance with the canonical labels that its tags hold.
type taggedInstance struct {
	govultr.Instance
	labels cloud.Labels
}

// List returns the machines of the cluster, sorted by name, then by id. It lists the instances with the cluster's tag
// and reads the private address of each: its address in its first VPC, since tent attaches one. It skips, with a
// warning, an instance whose tent tags do not decode, such as one with a tag in upper case, which Vultr's tag filter
// lists too, and one whose cluster label names another cluster.
//
// When Vultr answers 404 for the addresses, List reads the instance. It skips the instance only when that read answers
// 404 too: the instance was deleted after the list. Otherwise it keeps the instance without a private address, so a
// caller does not create a second node with its name. A 404 alone does not count as gone: a read of a pending
// instance's addresses 31 s after its create answered with the address, and what Vultr answers in the first 30 s is
// not known.
func (p *Provider) List(ctx context.Context, cluster string) ([]cloud.Instance, error) {
	out, err := p.list(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("list the nodes of cluster %s: %w", cluster, err)
	}
	return out, nil
}

// list does the work of List.
func (p *Provider) list(ctx context.Context, cluster string) ([]cloud.Instance, error) {
	nodes, err := p.listNodes(ctx, cluster, cloud.LabelCluster, cluster)
	if err != nil {
		return nil, err
	}
	out := make([]cloud.Instance, 0, len(nodes))
	for _, n := range nodes {
		addr, gone, err := p.listedAddress(ctx, n.ID)
		switch {
		case err != nil:
			return nil, fmt.Errorf("instance %s: %w", n.ID, err)
		case gone:
			continue
		}
		out = append(out, n.cloudInstance(addr))
	}
	slices.SortFunc(out, func(a, b cloud.Instance) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return out, nil
}

// listedAddress returns the private address of the listed instance with id, as privateAddress does, and whether the
// instance is gone. After a 404 for the addresses it reads the instance: only a 404 for that read too means gone, and
// otherwise the address is the invalid Addr.
func (p *Provider) listedAddress(ctx context.Context, id string) (addr netip.Addr, gone bool, err error) {
	addr, err = p.privateAddress(ctx, id)
	if !errors.Is(err, ErrNotFound) {
		return addr, false, err
	}
	_, err = p.api.GetInstance(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return netip.Addr{}, true, nil
	}
	return netip.Addr{}, false, err
}

// Create creates a machine of the cluster, waits until it is ready and returns it. It is idempotent per operation id:
// it first lists the instances with the tag of req.Op, and adopts the cluster's instance it finds instead of creating
// one. Of several, it adopts the oldest and logs a warning that names the others. It fails when the instance it finds
// has another name (its hostname, or its label when it has none), node group, role or zone than req: the caller gave
// one operation id to two nodes. Vultr's names are not unique, so a create that may have been carried out is never
// sent again:
//
//   - After a create without an answer (ErrUnavailable), or with an answer that holds no instance id, it searches
//     again and adopts what it finds. When the search fails or lists nothing yet, the error matches ErrUnavailable,
//     and a Create with the same operation id searches before it creates.
//   - An ErrLimitReached error tells how to raise the account's limit. Any other error of the create is returned as
//     it is.
//
// The machine joins the copies that Inventory keeps: the cluster's VPC, the servers' firewall group for a server or
// combined node or the clients' group for a client node, and all the cluster's SSH keys. Its label and hostname are
// req.Name, and its tags hold its canonical labels. Create fails without a call for an invalid request, an image that
// tent does not support on Vultr and labels that cannot be tags, such as a group name in upper case. It fails before
// the create when the cluster has no VPC or no such firewall group, or req.Zone is not the VPC's region.
//
// It returns once Vultr reports the instance active, running and ok and lists its address in its VPC, other than
// 0.0.0.0, reading the instance every 5 seconds. The wait has no deadline of its own: when ctx ends first, the error
// matches ctx's error and names the instance.
func (p *Provider) Create(ctx context.Context, req cloud.CreateRequest) (cloud.Instance, error) {
	if err := req.Validate(); err != nil {
		return cloud.Instance{}, err
	}
	in, err := p.create(ctx, req)
	if err != nil {
		return cloud.Instance{}, fmt.Errorf("create node %s of cluster %s: %w", req.Name, req.Cluster, err)
	}
	return in, nil
}

// create does the work of Create for a valid request.
func (p *Provider) create(ctx context.Context, req cloud.CreateRequest) (cloud.Instance, error) {
	r, labels, err := instanceRequest(req)
	if err != nil {
		return cloud.Instance{}, err
	}
	n, found, err := p.findByOp(ctx, req)
	switch {
	case err != nil:
		return cloud.Instance{}, fmt.Errorf("search by operation id: %w", err)
	case found:
		if err := n.isNode(req); err != nil {
			return cloud.Instance{}, err
		}
	default:
		if err := p.joinInfra(ctx, r, req.Cluster, req.Role); err != nil {
			return cloud.Instance{}, err
		}
		if n, err = p.sendCreate(ctx, r, req, labels); err != nil {
			return cloud.Instance{}, err
		}
	}
	in, err := p.waitReady(ctx, n)
	if err != nil {
		return cloud.Instance{}, fmt.Errorf("wait for instance %s: %w", n.ID, err)
	}
	return in, nil
}

// instanceRequest returns the create request of the machine that req asks for, without the cluster's VPC, firewall
// group and SSH keys, and the machine's labels. It fails for an image that tent does not support on Vultr and for
// labels that cannot be tags.
func instanceRequest(req cloud.CreateRequest) (*govultr.InstanceCreateReq, cloud.Labels, error) {
	os, ok := osID(req.Image)
	if !ok {
		return nil, nil, errors.New(unsupportedImage(req.Image))
	}
	labels := cloud.Labels{
		cloud.LabelCluster:   req.Cluster,
		cloud.LabelNodeGroup: req.Group,
		cloud.LabelRole:      string(req.Role),
		cloud.LabelOp:        req.Op,
	}
	if req.SpecHash != "" {
		labels[cloud.LabelSpecHash] = req.SpecHash
	}
	tags, err := EncodeTags(labels)
	if err != nil {
		return nil, nil, err
	}
	r := &govultr.InstanceCreateReq{
		Region: req.Zone, Plan: req.MachineType, OsID: os, Label: req.Name, Hostname: req.Name, Tags: tags,
		Backups: "disabled",
	}
	if len(req.UserData) > 0 {
		r.UserData = encodeUserData(req.UserData)
	}
	return r, labels, nil
}

// encodeUserData returns user data as Vultr takes it: in base64.
func encodeUserData(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// joinInfra puts into r, the create request of a machine of the cluster with role, the objects that the inventory
// keeps: the cluster's VPC, the firewall group of the role and all the cluster's SSH keys. It fails when the cluster
// has no VPC or no firewall group for the role, and when r's region is not the VPC's.
func (p *Provider) joinInfra(ctx context.Context, r *govultr.InstanceCreateReq, cluster string,
	role v1alpha1.Role) error {
	s, err := p.inventory(ctx, cluster)
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	vpc, ok := s.vpc(vpcKey(cluster))
	switch {
	case !ok:
		return fmt.Errorf("cluster %s has no VPC; apply its infrastructure first", cluster)
	case vpc.Region != r.Region: // on Vultr the region is the only zone
		return fmt.Errorf("zone %q: the VPC of cluster %s is in %s", r.Region, cluster, vpc.Region)
	}
	key := firewallGroupKey(cluster, firewallRole(role))
	group, ok := s.firewallGroup(key)
	if !ok {
		return fmt.Errorf("cluster %s has no firewall group %s; apply its infrastructure first", cluster, key.Name)
	}
	r.AttachVPC, r.FirewallGroupID, r.SSHKeys = []string{vpc.ID}, group.ID, s.sshKeyIDs()
	return nil
}

// firewallRole returns the role of the firewall group that a machine with role joins: the clients' group for a
// client, the servers' group for a server or a combined node.
func firewallRole(role v1alpha1.Role) string {
	if role == v1alpha1.RoleClient {
		return roleClient
	}
	return roleServer
}

// errNoInstanceID is why a create answer that holds no instance id cannot be read.
var errNoInstanceID = errors.New("the answer holds no instance id")

// sendCreate sends r, the create request of the machine that req asks for, with its labels, and returns the new
// instance. After a create without an answer, or with an answer that holds no instance id, it searches by the
// operation id and returns what it finds; when the search fails, finds nothing or finds another node, the error
// matches ErrUnavailable. An ErrLimitReached error tells how to raise the limit.
func (p *Provider) sendCreate(ctx context.Context, r *govultr.InstanceCreateReq, req cloud.CreateRequest,
	labels cloud.Labels) (taggedInstance, error) {
	in, err := p.api.CreateInstance(ctx, r)
	switch {
	case err == nil && (in == nil || in.ID == ""):
		err = NewNoAnswerError(http.MethodPost, "/v2/instances", errNoInstanceID)
	case err == nil:
		// The answer holds the root password: keep only the id.
		return taggedInstance{Instance: govultr.Instance{ID: in.ID}, labels: labels}, nil
	case !errors.Is(err, ErrUnavailable):
		return taggedInstance{}, withLimitHint(err)
	}
	n, found, serr := p.findByOp(ctx, req)
	switch {
	case serr != nil:
		return taggedInstance{}, fmt.Errorf("%w; search by operation id: %w", err, serr)
	case !found:
		return taggedInstance{}, fmt.Errorf("%w; no instance with the operation id %s is listed yet", err, req.Op)
	}
	if nerr := n.isNode(req); nerr != nil {
		return taggedInstance{}, fmt.Errorf("%w; %w", err, nerr)
	}
	return n, nil
}

// findByOp returns the cluster's instance whose tags carry the operation id of req, and whether there is one. Of
// several, it returns the oldest, then the one with the lowest id, as the inventory keeps SSH keys and VPCs, and logs
// a warning that names the others. A date that does not parse counts as the newest.
func (p *Provider) findByOp(ctx context.Context, req cloud.CreateRequest) (taggedInstance, bool, error) {
	nodes, err := p.listNodes(ctx, req.Cluster, cloud.LabelOp, req.Op)
	if err != nil || len(nodes) == 0 {
		return taggedInstance{}, false, err
	}
	slices.SortFunc(nodes, func(a, b taggedInstance) int {
		return cmp.Or(compareCreated(a.DateCreated, b.DateCreated), strings.Compare(a.ID, b.ID))
	})
	n := nodes[0]
	if len(nodes) > 1 {
		others := make([]string, 0, len(nodes)-1)
		for _, o := range nodes[1:] {
			others = append(others, o.ID)
		}
		p.log.WarnContext(ctx, "adopting the oldest of several Vultr instances with one operation id",
			"cluster", req.Cluster, "op", req.Op, "id", n.ID, "others", strings.Join(others, ", "))
	}
	return n, true, nil
}

// isNode fails when n, an instance with the operation id of req, has another name, node group, role or zone than req
// asks for: the caller gave one operation id to two nodes. The error names what differs.
func (n taggedInstance) isNode(req cloud.CreateRequest) error {
	var diffs []string
	for _, f := range []struct{ field, have, want string }{
		{"name", instanceName(n.Instance), req.Name},
		{"group", n.labels[cloud.LabelNodeGroup], req.Group},
		{"role", n.labels[cloud.LabelRole], string(req.Role)},
		{"zone", n.Region, req.Zone},
	} {
		if f.have != f.want {
			diffs = append(diffs, fmt.Sprintf("%s %s (want %s)", f.field, f.have, f.want))
		}
	}
	if len(diffs) == 0 {
		return nil
	}
	return fmt.Errorf("instance %s with the operation id %s is another node: %s; each node needs its own operation id",
		n.ID, req.Op, strings.Join(diffs, ", "))
}

// instanceName returns the name of the machine that in is: its hostname, which only a reinstall changes and which
// Nomad uses as the node name, or its label when it has no hostname. The label can be changed in the Vultr console.
func instanceName(in govultr.Instance) string { return cmp.Or(in.Hostname, in.Label) }

// listNodes lists the instances with the tag of the label key=value, and returns those the cluster owns: their tent
// tags decode and their cluster label is the cluster. It logs a warning for each other instance.
func (p *Provider) listNodes(ctx context.Context, cluster, key, value string) ([]taggedInstance, error) {
	list, err := instancesByLabel(ctx, p.api, key, value)
	if err != nil {
		return nil, err
	}
	var nodes []taggedInstance
	for _, in := range list {
		labels, err := DecodeTags(in.Tags)
		why := ""
		switch {
		case err != nil:
			why = err.Error()
		case labels[cloud.LabelCluster] != cluster:
			why = fmt.Sprintf("its cluster label is %q", labels[cloud.LabelCluster])
		}
		if why != "" {
			p.log.WarnContext(ctx, "skipping a Vultr instance whose tags do not mark it as the cluster's",
				"cluster", cluster, "id", in.ID, "reason", why)
			continue
		}
		nodes = append(nodes, taggedInstance{Instance: in, labels: labels})
	}
	return nodes, nil
}

// instancesByLabel lists every instance that Vultr's filter by the tag of the label key=value lists, whether or not
// its tent tags decode. It fails for a label that cannot be a tag.
func instancesByLabel(ctx context.Context, api API, key, value string) ([]govultr.Instance, error) {
	tags, err := EncodeTags(cloud.Labels{key: value})
	if err != nil {
		return nil, err
	}
	return api.ListInstances(ctx, tags[0])
}

// waitReady reads the instance n every p.pollEvery until Vultr reports it active, running and ok and lists its
// address in its VPC, and returns it then. It fails when a call fails or ctx ends.
func (p *Provider) waitReady(ctx context.Context, n taggedInstance) (cloud.Instance, error) {
	for {
		in, err := p.api.GetInstance(ctx, n.ID)
		if err != nil {
			return cloud.Instance{}, err
		}
		n.Instance = *in
		if ready(n.Instance) {
			addr, err := p.privateAddress(ctx, n.ID)
			if err != nil {
				return cloud.Instance{}, err
			}
			if addr.IsValid() {
				return n.cloudInstance(addr), nil
			}
		}
		select {
		case <-ctx.Done():
			return cloud.Instance{}, ctx.Err()
		case <-time.After(p.pollEvery):
		}
	}
}

// ready reports whether Vultr reports the instance running and booted: active, running and ok.
func ready(in govultr.Instance) bool {
	return in.Status == "active" && in.PowerStatus == "running" && in.ServerStatus == "ok"
}

// privateAddress returns the address of the instance with id in its first VPC, the one tent attaches, or the invalid
// Addr while Vultr lists none: no VPC, or an address that does not parse or is 0.0.0.0.
func (p *Provider) privateAddress(ctx context.Context, id string) (netip.Addr, error) {
	vpcs, err := p.api.ListInstanceVPCs(ctx, id)
	if err != nil || len(vpcs) == 0 {
		return netip.Addr{}, err
	}
	return parseAddr(vpcs[0].IPAddress), nil
}

// cloudInstance returns the machine that n is, with the private address addr.
func (n taggedInstance) cloudInstance(addr netip.Addr) cloud.Instance {
	return cloud.Instance{
		ID:        n.ID,
		Name:      instanceName(n.Instance),
		Cluster:   n.labels[cloud.LabelCluster],
		Group:     n.labels[cloud.LabelNodeGroup],
		Role:      v1alpha1.Role(n.labels[cloud.LabelRole]),
		Zone:      n.Region,
		SpecHash:  n.labels[cloud.LabelSpecHash],
		Op:        n.labels[cloud.LabelOp],
		PrivateIP: addr,
		PublicIP:  parseAddr(n.MainIP),
		Ready:     ready(n.Instance),
		Joined:    n.labels[cloud.LabelJoined] == "true",
		Created:   createdAt(n.DateCreated),
	}
}

// parseAddr returns the address that Vultr gives as s, or the invalid Addr when s does not parse or is an unspecified
// address, such as the main_ip 0.0.0.0 that Vultr gives until an instance is active.
func parseAddr(s string) netip.Addr {
	addr, err := netip.ParseAddr(s)
	if err != nil || addr.IsUnspecified() {
		return netip.Addr{}
	}
	return addr
}

// Stop powers the machine node off hard, like pulling its plug: Vultr has no graceful shutdown, so its processes get
// no chance to stop. A machine that is gone counts as stopped.
func (p *Provider) Stop(ctx context.Context, node cloud.Instance) error {
	return nodeResult(p.api.HaltInstance(ctx, node.ID), "stop", node)
}

// Delete destroys the machine node at once, even while it runs. A machine that is gone counts as deleted.
func (p *Provider) Delete(ctx context.Context, node cloud.Instance) error {
	return nodeResult(p.api.DeleteInstance(ctx, node.ID), "delete", node)
}

// scrubbedUserData replaces a node's user data once the node has joined the cluster. It holds no secrets and no
// modules: cloud-init runs the per-boot modules of the current user data on every boot. The empty mapping is there
// because cloud-init refuses a cloud-config that loads to nothing, such as one of comments only, and then reports a
// degraded status (seen with cloud-init 26.1 on Ubuntu 24.04).
const scrubbedUserData = "#cloud-config\n# tent removed this node's user data after the node joined the cluster\n{}\n"

// MarkJoined records on the machine node that its node has joined the cluster, and replaces its user data, which holds
// its secrets, with a stub: a cloud-config with a comment, an empty mapping and no modules. It reads the instance, then
// sends its tags in their order, with the tag tent/joined=true added at the end when it is missing, and the stub, in
// one update. A tent/joined tag with another value is replaced, and every other tag stays. A tag that an operator
// changes between the read and the update, about a second, is lost. It is safe to repeat. A machine that is gone
// counts as marked. After an error that matches ErrUnavailable the caller may call again.
func (p *Provider) MarkJoined(ctx context.Context, node cloud.Instance) error {
	in, err := p.api.GetInstance(ctx, node.ID)
	if err != nil {
		return nodeResult(err, "scrub the user data of", node)
	}
	tags := slices.DeleteFunc(slices.Clone(in.Tags), func(tag string) bool {
		return tag != joinedTag && strings.HasPrefix(tag, cloud.LabelJoined+"=")
	})
	if !slices.Contains(tags, joinedTag) {
		// The list holds the joined tag at least, so it is never nil and Vultr replaces the tags.
		tags = append(tags, joinedTag)
	}
	req := &govultr.InstanceUpdateReq{Tags: tags, UserData: encodeUserData([]byte(scrubbedUserData))}
	return nodeResult(p.api.UpdateInstance(ctx, node.ID, req), "scrub the user data of", node)
}

// nodeResult returns the outcome of a call that acts on the machine node: nil when the call succeeded or the machine
// is gone, and otherwise the call's error with what failed, such as "stop node prod-servers-0 (<id>)".
func nodeResult(err error, action string, node cloud.Instance) error {
	if err == nil || errors.Is(err, ErrNotFound) {
		return nil
	}
	return fmt.Errorf("%s node %s (%s): %w", action, node.Name, node.ID, err)
}

// createdAt returns an instance's creation date, RFC 3339 as Vultr gives it, in UTC; the zero time when it does not
// parse.
func createdAt(date string) time.Time {
	t, err := time.Parse(time.RFC3339, date)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
