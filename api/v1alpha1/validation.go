package v1alpha1

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// ValidateOptions change what Validate accepts.
type ValidateOptions struct {
	// AllowSingleServer accepts a server or combined group of size 1 (tent --allow-single-server).
	AllowSingleServer bool
}

// FieldError is one problem with one field of one object.
type FieldError struct {
	Object string // for example "Cluster prod" or "NodeGroup workers"
	Path   string // for example "spec.size" or "spec.access.api[1]"
	Detail string // what is wrong, for example "must not be negative"
}

// Error formats the problem as "<object>: <path>: <detail>".
func (e FieldError) Error() string { return e.Object + ": " + e.Path + ": " + e.Detail }

// Errors is every problem Validate found, in a stable order.
type Errors []FieldError

// Error lists the problems one per line.
func (e Errors) Error() string {
	lines := make([]string, len(e))
	for i, fe := range e {
		lines[i] = fe.Error()
	}
	return strings.Join(lines, "\n")
}

var (
	namePattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,18}[a-z0-9]$`)
	reservedPattern  = regexp.MustCompile(`^(con|prn|aux|nul|com[1-9]|lpt[1-9])$`) // names Windows reserves
	channelPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	regionPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`) // also cloud zones and the Nomad region
	nodePoolPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	nodeClassPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	driverPattern    = regexp.MustCompile(`^[a-z0-9_-]+$`)
	metaKeyPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`) // Nomad refuses empty parts
)

// Prefixes and values that Nomad or tent keep for themselves.
const (
	tentMetaPrefix = "tent_" // tent's own client meta, such as tent_cluster
	nodePoolAll    = "all"   // Nomad's built-in pool of every node, which no client joins
	// hclReserved is a character that Nomad's configuration parser, HCL1, refuses anywhere in a file.
	hclReserved = '\uE123'
)

// sshKeyTypes are the public key types tent installs on nodes.
var sshKeyTypes = []string{
	"ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "ssh-rsa",
	"sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com",
}

// privateRanges are the private IPv4 ranges of RFC 1918.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

// Validate checks a cluster and its node groups after SetDefaults. It returns nil or Errors with every problem, so
// the operator can fix them in one go. It checks only static rules; the provider checks the live API. It does not
// check the Nomad version, which depends on the channel and is checked by tent. Nil groups are skipped; a nil cluster
// is an error.
func Validate(c *Cluster, groups []*NodeGroup, opts ValidateOptions) error {
	if c == nil {
		return errors.New("no Cluster to validate")
	}
	var errs Errors
	ck := checker{object: label(KindCluster, c.Metadata.Name), errs: &errs}
	checkCluster(ck, c)
	sorted := sortedGroups(groups)
	named := make(map[string]int, len(sorted))
	for _, g := range sorted {
		named[g.Metadata.Name]++
	}
	for _, g := range sorted {
		name := g.Metadata.Name
		shared := named[name]
		delete(named, name) // report a shared name once, on the first group that has it
		checkGroup(checker{object: label(KindNodeGroup, name), errs: &errs}, g, c, opts, shared)
	}
	checkServerCount(ck, sorted)
	checkClientIntroduction(ck, c, sorted)
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func checkCluster(ck checker, c *Cluster) {
	ck.header(c.TypeMeta, KindCluster, c.Metadata.Name)
	s := &c.Spec
	ck.requiredMatch("spec.channel", s.Channel, channelPattern)
	checkCloud(ck, &s.Cloud)
	checkNetwork(ck, s.Networking.CIDR)
	ck.cidrs("spec.access.ssh", s.Access.SSH)
	if len(s.Access.API) == 0 {
		ck.add("spec.access.api", "must not be empty: tent needs the Nomad API; list the addresses that may reach it")
	}
	ck.cidrs("spec.access.api", s.Access.API)
	checkSSHKeys(ck, s.SSHKeys)
	checkClusterNomad(ck, &s.Nomad)
}

func checkCloud(ck checker, cl *Cloud) {
	oneOf(ck, "spec.cloud.provider", cl.Provider, Providers())
	regionOK := ck.requiredMatch("spec.cloud.region", cl.Region, regionPattern)
	switch cl.Provider {
	case ProviderVultr:
		// Zones derived from a bad region would only repeat the region's problem.
		if regionOK && !slices.Equal(cl.Zones, []string{cl.Region}) {
			ck.add("spec.cloud.zones", "must be ["+cl.Region+"]: Vultr has no zones")
		}
		if cl.Hetzner != nil {
			ck.add("spec.cloud.hetzner", "must not be set when the provider is vultr")
		}
	case ProviderHetzner:
		if len(cl.Zones) == 0 {
			ck.add("spec.cloud.zones", "required")
		}
		ck.list("spec.cloud.zones", cl.Zones, "zone", ck.matching(regionPattern))
		if cl.Vultr != nil {
			ck.add("spec.cloud.vultr", "must not be set when the provider is hetzner")
		}
	}
}

func checkNetwork(ck checker, cidr string) {
	const path = "spec.networking.cidr"
	if p, ok := ck.cidr(path, cidr); ok {
		switch {
		case !p.Addr().Is4():
			ck.add(path, "must be an IPv4 CIDR")
		case !isPrivate(p):
			ck.add(path, "must be a private range inside 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16")
		}
	}
}

// isPrivate reports whether all of p lies in one private IPv4 range.
func isPrivate(p netip.Prefix) bool {
	return slices.ContainsFunc(privateRanges, func(r netip.Prefix) bool {
		return r.Bits() <= p.Bits() && r.Contains(p.Addr())
	})
}

func checkSSHKeys(ck checker, keys []string) {
	seen := make(map[string]bool, len(keys))
	for i, key := range keys {
		path := index("spec.sshKeys", i)
		data, problem := parseSSHKey(key)
		switch {
		case problem != "":
			ck.add(path, problem)
		case seen[data]:
			ck.add(path, "duplicate key")
		}
		seen[data] = true
	}
}

// parseSSHKey returns the key data of "<type> <base64> [comment]", or what is wrong with the key.
func parseSSHKey(key string) (data, problem string) {
	if strings.ContainsAny(key, "\r\n") {
		return "", "must be one line"
	}
	fields := strings.Fields(key)
	if len(fields) < 2 {
		return "", "must be an OpenSSH public key: <type> <base64> [comment]"
	}
	typ := fields[0]
	if !slices.Contains(sshKeyTypes, typ) {
		return "", "key type must be one of " + join(sshKeyTypes)
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", "key data is not valid base64"
	}
	// The key data starts with its type: a 4-byte big-endian length, then the type itself.
	prefix := append(binary.BigEndian.AppendUint32(nil, uint32(len(typ))), typ...)
	if !bytes.HasPrefix(blob, prefix) {
		return "", "key data does not match the type " + typ
	}
	return string(blob), ""
}

// checkClusterNomad checks the Nomad settings of the whole cluster. The versions that a cluster may run come with the
// channel, so the version is left to whoever knows the channel.
func checkClusterNomad(ck checker, n *ClusterNomad) {
	ck.requiredMatch("spec.nomad.region", n.Region, regionPattern)
	oneOf(ck, "spec.nomad.clientIntroduction", n.ClientIntroduction, ClientIntroductions())
}

// checkGroup checks one node group. shared is how many groups have its name, or 0 when another group reports it.
func checkGroup(ck checker, g *NodeGroup, c *Cluster, opts ValidateOptions, shared int) {
	ck.header(g.TypeMeta, KindNodeGroup, g.Metadata.Name)
	// A missing name is reported as required, not as shared.
	if shared > 1 && g.Metadata.Name != "" {
		ck.add("metadata.name",
			fmt.Sprintf("duplicate node group name: %d groups are named %s", shared, g.Metadata.Name))
	}
	checkClusterRef(ck, g, c)
	s := &g.Spec
	oneOf(ck, "spec.role", s.Role, Roles())
	ck.requiredWord("spec.machineType", s.MachineType)
	ck.requiredWord("spec.image", s.Image)
	checkSize(ck, s.Role, s.Size, opts)
	ck.list("spec.zones", s.Zones, "zone", func(path, zone string) bool {
		if slices.Contains(c.Spec.Cloud.Zones, zone) {
			return true
		}
		ck.add(path, fmt.Sprintf("%q is not a cluster zone", zone))
		return false
	})
	checkGroupNomad(ck, s.Role, &s.Nomad)
	checkRollingUpdate(ck, s.Role, &s.RollingUpdate)
}

func checkSize(ck checker, role Role, size int, opts ValidateOptions) {
	const path = "spec.size"
	switch {
	case role.RunsServer():
		if size != 1 && size != 3 && size != 5 {
			ck.add(path, "must be 1, 3 or 5 for role="+string(role))
		} else if size == 1 && !opts.AllowSingleServer {
			ck.add(path, "size 1 needs --allow-single-server")
		}
	case role == RoleClient:
		if size < 0 {
			ck.add(path, "must not be negative")
		}
	}
}

func checkGroupNomad(ck checker, role Role, n *NodeGroupNomad) {
	if role == RoleServer {
		// Lengths, not zero values: drivers: [] decodes as an empty, non-nil slice.
		if n.NodePool != "" || n.NodeClass != "" || len(n.Drivers) > 0 || len(n.Meta) > 0 {
			ck.add("spec.nomad", "must be empty for role=server: client settings do not apply")
		}
		return
	}
	if n.NodePool == nodePoolAll {
		ck.add("spec.nomad.nodePool",
			"must not be all: Nomad keeps it for the pool of every node, which no client joins")
	} else if n.NodePool != "" {
		ck.matches("spec.nomad.nodePool", n.NodePool, nodePoolPattern)
	}
	if n.NodeClass != "" {
		ck.matches("spec.nomad.nodeClass", n.NodeClass, nodeClassPattern)
	}
	ck.list("spec.nomad.drivers", n.Drivers, "driver", ck.matching(driverPattern))
	for _, key := range slices.Sorted(maps.Keys(n.Meta)) {
		checkMeta(ck, fmt.Sprintf("spec.nomad.meta[%q]", key), key, n.Meta[key])
	}
}

// rollingUpdatePath is the path of a node group's rolling update settings in field errors.
const rollingUpdatePath = "spec.rollingUpdate"

// checkRollingUpdate checks the rolling update settings of a group after SetDefaults. A server group has none, and
// a combined group has only drainTimeout. A setting that a role would ignore is an error, so it cannot mislead.
func checkRollingUpdate(ck checker, role Role, r *RollingUpdate) {
	switch role {
	case RoleServer:
		const detail = "must be empty for role=server: a server has no client to drain and rolls one node at a time"
		checkLeftOut(ck, r, detail)
		if r.DrainTimeout != "" {
			ck.add(rollingUpdatePath+".drainTimeout", detail)
		}
		return
	case RoleCombined:
		checkLeftOut(ck, r,
			"must be left out for role=combined: combined nodes roll one at a time, with one more node first")
	case RoleClient:
		checkClientRolling(ck, r)
	default:
		// The role is reported on its own.
		return
	}
	checkDrainTimeout(ck, r)
}

// checkLeftOut reports maxSurge and maxUnavailable when they are set, with detail.
func checkLeftOut(ck checker, r *RollingUpdate, detail string) {
	if r.MaxSurge != nil {
		ck.add(rollingUpdatePath+".maxSurge", detail)
	}
	if r.MaxUnavailable != nil {
		ck.add(rollingUpdatePath+".maxUnavailable", detail)
	}
}

// checkClientRolling checks maxSurge and maxUnavailable of a client group.
func checkClientRolling(ck checker, r *RollingUpdate) {
	const path = rollingUpdatePath
	if r.MaxSurge != nil && *r.MaxSurge < 0 {
		ck.add(path+".maxSurge", "must not be negative")
	}
	if r.MaxUnavailable != nil && *r.MaxUnavailable < 0 {
		ck.add(path+".maxUnavailable", "must not be negative")
	}
	if r.MaxSurge != nil && r.MaxUnavailable != nil && *r.MaxSurge == 0 && *r.MaxUnavailable == 0 {
		ck.add(path, "maxSurge and maxUnavailable must not both be 0: the group could not roll")
	}
}

// checkDrainTimeout checks that drainTimeout is a duration above zero.
func checkDrainTimeout(ck checker, r *RollingUpdate) {
	const path = rollingUpdatePath + ".drainTimeout"
	d, err := r.Drain()
	switch {
	case err != nil:
		ck.add(path, "must be a duration such as 1h or 30m")
	case d <= 0:
		ck.add(path, "must be above zero: Nomad reads 0 as a drain without a deadline")
	}
}

// checkMeta checks one client meta key and its value, which tent writes into the Nomad configuration.
func checkMeta(ck checker, path, key, value string) {
	switch {
	case !metaKeyPattern.MatchString(key):
		ck.add(path, "key must match "+metaKeyPattern.String())
	case strings.HasPrefix(key, tentMetaPrefix):
		ck.add(path, "key must not start with "+tentMetaPrefix+": tent keeps those keys for its own meta, such as "+
			"tent_cluster")
	}
	switch {
	case strings.ContainsFunc(value, unicode.IsControl):
		ck.add(path, "value must not have a control character, such as a line end or a tab")
	case strings.ContainsRune(value, hclReserved):
		ck.add(path, fmt.Sprintf("value must not have %U, which Nomad's configuration parser refuses", hclReserved))
	case strings.Contains(value, "${"):
		ck.add(path, "value must not have ${, which Nomad reads as the start of an interpolation")
	}
}

// checkServerCount checks that the cluster has exactly one server or combined group.
func checkServerCount(ck checker, groups []*NodeGroup) {
	var servers []string
	for _, g := range groups {
		if g.Spec.Role.RunsServer() {
			servers = append(servers, displayName(g.Metadata.Name))
		}
	}
	if len(servers) != 1 {
		detail := fmt.Sprintf("need exactly one server or combined group, found %d", len(servers))
		if len(servers) > 1 {
			detail += ": " + strings.Join(servers, ", ")
		}
		ck.add("nodeGroups", detail)
	}
}

// checkClientIntroduction checks that a combined group's client can join: it registers before intro tokens exist.
func checkClientIntroduction(ck checker, c *Cluster, groups []*NodeGroup) {
	combined := slices.ContainsFunc(groups, func(g *NodeGroup) bool { return g.Spec.Role == RoleCombined })
	if combined && c.Spec.Nomad.ClientIntroduction == ClientIntroductionStrict {
		ck.add("spec.nomad.clientIntroduction",
			"must not be strict: a combined group registers its client before intro tokens exist")
	}
}

// sortedGroups returns the groups without nil entries, sorted by name; groups with one name keep their order.
func sortedGroups(groups []*NodeGroup) []*NodeGroup {
	sorted := slices.DeleteFunc(slices.Clone(groups), func(g *NodeGroup) bool { return g == nil })
	slices.SortStableFunc(sorted, func(a, b *NodeGroup) int {
		return strings.Compare(a.Metadata.Name, b.Metadata.Name)
	})
	return sorted
}

// label names an object in a FieldError, for example "NodeGroup workers".
func label(kind, name string) string { return kind + " " + displayName(name) }

func displayName(name string) string {
	if name == "" {
		return "(no name)"
	}
	return name
}

// checker adds the problems of one object to a shared list.
type checker struct {
	object string
	errs   *Errors
}

func (ck checker) add(path, detail string) {
	*ck.errs = append(*ck.errs, FieldError{Object: ck.object, Path: path, Detail: detail})
}

// header checks the fields every object has.
func (ck checker) header(t TypeMeta, kind, name string) {
	if t.APIVersion != APIVersion {
		ck.add("apiVersion", "must be "+APIVersion)
	}
	if t.Kind != kind {
		ck.add("kind", "must be "+kind)
	}
	ck.name("metadata.name", name)
}

// name checks that v is set and follows the name rule.
func (ck checker) name(path, v string) {
	if ck.required(path, v) {
		if problem := nameProblem(v); problem != "" {
			ck.add(path, problem)
		}
	}
}

// checkClusterRef checks a node group's metadata.cluster: it must be c's name, or without c, a valid name.
func checkClusterRef(ck checker, g *NodeGroup, c *Cluster) {
	switch {
	case c == nil:
		ck.name("metadata.cluster", g.Metadata.Cluster)
	// A cluster with a missing or invalid name has nothing to compare with; the cluster reports it.
	case nameProblem(c.Metadata.Name) == "" && g.Metadata.Cluster != c.Metadata.Name:
		ck.add("metadata.cluster", "must be "+c.Metadata.Name+", the cluster's name")
	}
}

// ValidateNames checks only the names, as Validate does: each object's metadata.name, and each node group's
// metadata.cluster, which must name the cluster, or a valid cluster when c is nil. Tools that keep objects under
// their names call it before they read or write anything under them. It returns nil when the names are valid.
func ValidateNames(c *Cluster, groups []*NodeGroup) Errors {
	var errs Errors
	if c != nil {
		checker{object: label(KindCluster, c.Metadata.Name), errs: &errs}.name("metadata.name", c.Metadata.Name)
	}
	for _, g := range sortedGroups(groups) {
		ck := checker{object: label(KindNodeGroup, g.Metadata.Name), errs: &errs}
		ck.name("metadata.name", g.Metadata.Name)
		checkClusterRef(ck, g, c)
	}
	return errs
}

// ValidateName checks the name of a cluster or a node group, by kind, against the rule that Validate applies to
// metadata.name. Tools call it on names given on their own, such as on the command line.
func ValidateName(kind, name string) error {
	problem := nameProblem(name)
	if problem == "" {
		return nil
	}
	what := "cluster"
	if kind == KindNodeGroup {
		what = "node group"
	}
	return fmt.Errorf("invalid %s name %q: %s", what, name, problem)
}

// RegionOK reports whether r is a region, the rule that Validate applies to spec.cloud.region, the zones and
// spec.nomad.region: lower-case letters, digits and dashes, starting with a letter or digit. Callers that keep a
// region of their own check it with the same rule.
func RegionOK(r string) bool { return regionPattern.MatchString(r) }

// nameProblem returns what is wrong with a cluster or node group name, or "" when nothing is.
func nameProblem(name string) string {
	switch {
	case !namePattern.MatchString(name):
		return "must be 2 to 20 lowercase letters, digits or dashes, starting with a letter and ending with a " +
			"letter or digit"
	case reservedPattern.MatchString(name):
		// The state store names files and directories after clusters and node groups; Windows cannot create these.
		return "must not be " + name + ": Windows reserves that name"
	}
	return ""
}

// required reports whether v is set.
func (ck checker) required(path, v string) bool {
	if v == "" {
		ck.add(path, "required")
		return false
	}
	return true
}

// matches reports whether v matches re.
func (ck checker) matches(path, v string, re *regexp.Regexp) bool {
	if re.MatchString(v) {
		return true
	}
	ck.add(path, "must match "+re.String())
	return false
}

// matching returns matches with re fixed, for list.
func (ck checker) matching(re *regexp.Regexp) func(path, v string) bool {
	return func(path, v string) bool { return ck.matches(path, v, re) }
}

// requiredMatch reports whether v is set and matches re.
func (ck checker) requiredMatch(path, v string, re *regexp.Regexp) bool {
	return ck.required(path, v) && ck.matches(path, v, re)
}

// requiredWord checks that v is set and has no whitespace.
func (ck checker) requiredWord(path, v string) {
	if ck.required(path, v) && strings.ContainsFunc(v, unicode.IsSpace) {
		ck.add(path, "must not contain whitespace")
	}
}

// cidr checks that v is a CIDR with no bits set after the prefix length.
func (ck checker) cidr(path, v string) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(v)
	switch {
	case err != nil:
		ck.add(path, "must be a CIDR in address/bits form")
	case p != p.Masked():
		ck.add(path, fmt.Sprintf("must be a network address: %s has bits set after /%d", p.Addr(), p.Bits()))
	default:
		return p, true
	}
	return p, false
}

func (ck checker) cidrs(path string, list []string) {
	for i, v := range list {
		ck.cidr(index(path, i), v)
	}
}

// list checks each item with valid, and reports a valid item that repeats an earlier one.
func (ck checker) list(path string, items []string, noun string, valid func(path, item string) bool) {
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		p := index(path, i)
		if valid(p, item) && seen[item] {
			ck.add(p, fmt.Sprintf("duplicate %s %q", noun, item))
		}
		seen[item] = true
	}
}

// oneOf checks that v is one of allowed.
func oneOf[T ~string](ck checker, path string, v T, allowed []T) {
	if !slices.Contains(allowed, v) {
		ck.add(path, "must be one of "+join(allowed))
	}
}

// join lists values separated by commas.
func join[T ~string](values []T) string {
	s := make([]string, len(values))
	for i, v := range values {
		s[i] = string(v)
	}
	return strings.Join(s, ", ")
}

func index(path string, i int) string { return fmt.Sprintf("%s[%d]", path, i) }
