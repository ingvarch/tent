package app

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/english"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// Create stores new objects. With a Cluster it creates the cluster and its node groups; node groups alone are added
// to the existing cluster that their metadata.cluster names. The whole cluster that results must be valid. Each
// object is stored as given, without defaults. An object stored already with the same content is unchanged, so an
// interrupted create can run again; node groups stored without their cluster.yaml are left from such a create and are
// replaced; one that the objects lack is an error. Another object with other content is an error. Without apply,
// Create only checks the objects and returns what it would do. When the lock is lost after the writes, the changes
// come back together with an error that matches statestore.ErrLockLost.
func (s *Service) Create(ctx context.Context, objs spec.Objects, apply bool) ([]Change, error) {
	return s.write(ctx, objs, create, Ref{}, apply)
}

// Replace replaces stored objects of one cluster with the given ones. A missing object is an error. The whole
// cluster that results must be valid. An object stored already with the same content is unchanged. Without apply,
// Replace only checks the objects and returns what it would do. When the lock is lost after the writes, the changes
// come back together with an error that matches statestore.ErrLockLost.
func (s *Service) Replace(ctx context.Context, objs spec.Objects, apply bool) ([]Change, error) {
	return s.write(ctx, objs, replace, Ref{}, apply)
}

// Ref names an object that Load read, with the version that Save expects.
type Ref struct {
	Cluster string
	Kind    string // v1alpha1.KindCluster or v1alpha1.KindNodeGroup
	Name    string // the node group's name, or the cluster's
	Version statestore.Version
}

// label names the object in messages.
func (r Ref) label() string {
	if r.Kind == v1alpha1.KindCluster {
		return clusterLabel(r.Cluster)
	}
	return groupLabel(r.Cluster, r.Name)
}

// Load reads one object for tent edit: the Cluster, or the node group called name. It returns the object alone and
// its Ref, which Save takes.
func (s *Service) Load(ctx context.Context, cluster, kind, name string) (_ spec.Objects, _ Ref, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return spec.Objects{}, Ref{}, err
	}
	ref := Ref{Cluster: cluster, Kind: kind, Name: name}
	var p string
	switch kind {
	case v1alpha1.KindCluster:
		p, ref.Name = l.ClusterSpec(), cluster
	case v1alpha1.KindNodeGroup:
		if err = v1alpha1.ValidateName(kind, name); err == nil {
			p, err = l.NodeGroup(name)
		}
	default:
		err = fmt.Errorf("unknown kind %q, want %s or %s", kind, v1alpha1.KindCluster, v1alpha1.KindNodeGroup)
	}
	if err != nil {
		return spec.Objects{}, Ref{}, err
	}
	data, v, err := s.Store.Get(ctx, p)
	if errors.Is(err, statestore.ErrNotFound) {
		err = s.notFound(ref.label())
	}
	if err != nil {
		return spec.Objects{}, Ref{}, err
	}
	objs, err := decodeStored(l, map[string]entry{p: {data: data, version: v}}, nil)
	if err != nil {
		return spec.Objects{}, Ref{}, err
	}
	ref.Version = v
	return objs, ref, nil
}

// Save writes the object that Load read as ref, edited since, unless the stored object has changed meanwhile. The
// object keeps its kind and names. The whole cluster that results must be valid. An object with the same content is
// unchanged. Without apply, Save only checks the object and returns what it would do. When the lock is lost after the
// write, the change comes back together with an error that matches statestore.ErrLockLost.
func (s *Service) Save(ctx context.Context, ref Ref, objs spec.Objects, apply bool) ([]Change, error) {
	n := len(objs.NodeGroups)
	if objs.Cluster != nil {
		n++
	}
	if n != 1 {
		return nil, fmt.Errorf("edit saves one Cluster or one NodeGroup; the spec holds %d objects", n)
	}
	if !sameObject(ref, objs) {
		return nil, errors.New("edit cannot rename; the spec must still be " + ref.label())
	}
	return s.write(ctx, objs, save, ref, apply)
}

// sameObject reports whether objs is the one object that ref names.
func sameObject(ref Ref, objs spec.Objects) bool {
	if c := objs.Cluster; c != nil {
		return ref.Kind == v1alpha1.KindCluster && c.Metadata.Name == ref.Cluster
	}
	g := objs.NodeGroups[0]
	return ref.Kind == v1alpha1.KindNodeGroup && g != nil && g.Metadata.Name == ref.Name &&
		g.Metadata.Cluster == ref.Cluster
}

// Get returns a cluster's Cluster and its node groups by name, as stored. With full it fills in the defaults.
func (s *Service) Get(ctx context.Context, cluster string, full bool) (_ spec.Objects, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return spec.Objects{}, err
	}
	return s.get(ctx, l, full)
}

// get is Get once the cluster's name and tent version are checked.
func (s *Service) get(ctx context.Context, l statestore.Layout, full bool) (spec.Objects, error) {
	st, err := s.load(ctx, l)
	if err != nil {
		return spec.Objects{}, err
	}
	if _, ok := st[l.ClusterSpec()]; !ok {
		return spec.Objects{}, s.notFound(clusterLabel(l.Cluster()))
	}
	objs, err := decodeStored(l, st, nil)
	if err != nil {
		return spec.Objects{}, err
	}
	if full {
		v1alpha1.SetDefaults(objs.Cluster, objs.NodeGroups)
	}
	return objs, nil
}

// Clusters returns every cluster in the store, sorted by name, as Get does. A cluster that needs a newer tent fails
// the whole list.
func (s *Service) Clusters(ctx context.Context, full bool) (_ []spec.Objects, err error) {
	defer func() { err = stopped(ctx, err) }()
	names, err := statestore.Clusters(ctx, s.Store)
	if err != nil {
		return nil, err
	}
	clusters := make([]spec.Objects, len(names))
	for i, name := range names {
		if clusters[i], err = s.Get(ctx, name, full); err != nil {
			return nil, err
		}
	}
	return clusters, nil
}

// NodeGroups returns node groups of a cluster, as Get does: those called names in that order, or all of them by name
// when there are no names.
func (s *Service) NodeGroups(ctx context.Context, cluster string, names []string, full bool) (
	[]*v1alpha1.NodeGroup, error,
) {
	objs, err := s.Get(ctx, cluster, full)
	if err != nil || len(names) == 0 {
		return objs.NodeGroups, err
	}
	groups := make([]*v1alpha1.NodeGroup, len(names))
	for i, name := range names {
		if err := v1alpha1.ValidateName(v1alpha1.KindNodeGroup, name); err != nil {
			return nil, err
		}
		j := slices.IndexFunc(objs.NodeGroups, func(g *v1alpha1.NodeGroup) bool { return g.Metadata.Name == name })
		if j < 0 {
			return nil, s.notFound(groupLabel(cluster, name))
		}
		groups[i] = objs.NodeGroups[j]
	}
	return groups, nil
}

// layout returns the layout of a cluster that this tent may read: one that no newer tent has written.
func (s *Service) layout(ctx context.Context, cluster string) (statestore.Layout, error) {
	l, err := clusterLayout(cluster)
	if err != nil {
		return statestore.Layout{}, err
	}
	if err := statestore.CheckVersion(ctx, s.Store, l, s.Version); err != nil {
		return statestore.Layout{}, err
	}
	return l, nil
}

// clusterLayout returns the layout of a cluster whose name comes from the operator. The name must follow the name
// rule: on a disk that ignores case, PROD would name the state of prod.
func clusterLayout(cluster string) (statestore.Layout, error) {
	if err := v1alpha1.ValidateName(v1alpha1.KindCluster, cluster); err != nil {
		return statestore.Layout{}, err
	}
	return statestore.NewLayout(cluster)
}

// mode is how a use case writes objects.
type mode int

const (
	create  mode = iota // new objects only
	replace             // existing objects only
	save                // one existing object that has not changed since it was read
)

// operation names m in the lock's lease.
func (m mode) operation() string {
	switch m {
	case create:
		return "create"
	case replace:
		return "replace"
	}
	return "edit"
}

// write stores the objects as m says, or only plans it without apply, and returns what it did or would do to each. ref
// names the object that save writes.
func (s *Service) write(ctx context.Context, objs spec.Objects, m mode, ref Ref, apply bool) (
	_ []Change, err error,
) {
	defer func() { err = stopped(ctx, err) }()
	// The names become store paths, so nothing is read before they are valid: on a disk that ignores case, PROD would
	// read prod's state. Without the store, the objects' own problems are all that can be reported.
	if errs := v1alpha1.ValidateNames(objs.Cluster, objs.NodeGroups); len(errs) > 0 {
		if objs.Cluster == nil {
			return nil, errs // the rest needs the stored Cluster
		}
		_, err := check(objs, s.Validate, s.channel)
		return nil, cmp.Or(err, error(errs)) // Validate reports the names too
	}
	in, err := newInput(objs)
	if err != nil {
		return nil, err
	}
	var (
		changes  []Change
		warnings []string
	)
	err = s.change(ctx, in.layout, m.operation(), apply, func(ctx context.Context) ([]step, error) {
		c, steps, w, err := s.plan(ctx, in, m, ref.Version)
		changes, warnings = c, w
		return steps, err
	})
	if err != nil && !Saved(err) {
		return nil, err
	}
	if apply {
		s.warn(warnings...)
	}
	return changes, err
}

// warnings returns the warnings about the cluster c, which has its defaults and the channel ch: a Nomad API that the
// whole internet may reach, and a Nomad version that the channel allows but has not tested.
func warnings(c *v1alpha1.Cluster, ch *channels.Channel) []string {
	var warnings []string
	if openAPI(c) {
		warnings = append(warnings, openAPIWarning)
	}
	if v := c.Spec.Nomad.Version; v != "" && !ch.Tested(v) {
		warnings = append(warnings, fmt.Sprintf("Nomad %s is not tested by this tent; channel %s tests %s", v,
			ch.Name, english.And(ch.Nomad.Tested)))
	}
	return warnings
}

// openAPI reports whether the cluster c, which has its defaults, lets the whole internet reach the Nomad API.
func openAPI(c *v1alpha1.Cluster) bool {
	return slices.ContainsFunc(c.Spec.Access.API, func(cidr string) bool {
		p, err := netip.ParsePrefix(cidr)
		return err == nil && p.Bits() == 0
	})
}

// withDefaults returns a copy of the cluster c with its defaults filled in. The copy shares c's lists, which
// SetDefaults replaces rather than changes.
func withDefaults(c *v1alpha1.Cluster) v1alpha1.Cluster {
	filled := *c
	v1alpha1.SetDefaults(&filled, nil)
	return filled
}

// input is the objects a use case writes, all of one cluster.
type input struct {
	layout  statestore.Layout
	cluster *v1alpha1.Cluster     // nil when the objects hold node groups only
	groups  []*v1alpha1.NodeGroup // by name
	docs    []doc                 // the Cluster first, then the node groups
}

// doc is one object as the store keeps it.
type doc struct {
	kind  string
	name  string
	label string // names the object in messages
	path  string
	data  []byte // spec.Encode of the object alone
}

func newInput(objs spec.Objects) (input, error) {
	groups := slices.DeleteFunc(slices.Clone(objs.NodeGroups), func(g *v1alpha1.NodeGroup) bool { return g == nil })
	sortGroups(groups)
	name, err := clusterName(objs.Cluster, groups)
	if err != nil {
		return input{}, err
	}
	l, err := statestore.NewLayout(name)
	if err != nil {
		return input{}, err
	}
	in := input{layout: l, cluster: objs.Cluster, groups: groups}
	if c := objs.Cluster; c != nil {
		d, err := newDoc(v1alpha1.KindCluster, name, clusterLabel(name), l.ClusterSpec(), spec.Objects{Cluster: c})
		if err != nil {
			return input{}, err
		}
		in.docs = append(in.docs, d)
	}
	for _, g := range groups {
		p, err := l.NodeGroup(g.Metadata.Name)
		if err != nil {
			return input{}, err
		}
		one := spec.Objects{NodeGroups: []*v1alpha1.NodeGroup{g}}
		d, err := newDoc(v1alpha1.KindNodeGroup, g.Metadata.Name, groupLabel(name, g.Metadata.Name), p, one)
		if err != nil {
			return input{}, err
		}
		in.docs = append(in.docs, d)
	}
	return in, nil
}

func newDoc(kind, name, label, path string, one spec.Objects) (doc, error) {
	data, err := spec.Encode(one)
	if err != nil {
		return doc{}, fmt.Errorf("encode %s: %w", label, err)
	}
	return doc{kind: kind, name: name, label: label, path: path, data: data}, nil
}

// clusterName returns the name of the cluster the objects belong to: the Cluster's, or else the one that every node
// group names.
func clusterName(c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) (string, error) {
	if c != nil {
		return c.Metadata.Name, nil
	}
	names := make([]string, len(groups))
	for i, g := range groups {
		names[i] = g.Metadata.Cluster
	}
	slices.Sort(names)
	names = slices.Compact(names)
	switch len(names) {
	case 0:
		return "", errors.New("the spec holds no Cluster and no NodeGroup")
	case 1:
		return names[0], nil
	}
	return "", errors.New("the node groups belong to different clusters: " + strings.Join(names, ", "))
}

func clusterLabel(cluster string) string { return "cluster " + cluster }

func groupLabel(cluster, group string) string {
	return "node group " + group + " of cluster " + cluster
}

// plan decides what writing the input as m says does to each object, checks the cluster that results, and returns
// the changes, the writes that carry them out (the tent version, the node groups, and the Cluster last) and the
// warnings about the resulting cluster.
func (s *Service) plan(ctx context.Context, in input, m mode, readVersion statestore.Version) (
	[]Change, []step, []string, error,
) {
	st, err := s.load(ctx, in.layout)
	if err != nil {
		return nil, nil, nil, err
	}
	_, clusterStored := st[in.layout.ClusterSpec()]
	if !clusterStored && in.cluster == nil {
		return nil, nil, nil, s.notFound(clusterLabel(in.layout.Cluster()))
	}
	var (
		changes []Change
		writes  []write
		errs    []error
	)
	if m == create && !clusterStored {
		errs = s.leftovers(in, st)
	}
	for _, d := range in.docs {
		e, ok := st[d.path]
		// Node groups stored without their cluster.yaml are left from an interrupted create.
		action, err := s.decide(m, d, e, ok, !clusterStored, readVersion)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		changes = append(changes, Change{Kind: d.kind, Name: d.name, Action: action})
		if action != Unchanged {
			writes = append(writes, write{doc: d, version: e.version})
		}
	}
	if len(errs) > 0 {
		return nil, nil, nil, errors.Join(errs...)
	}
	warnings, err := s.validate(st, in)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(writes) == 0 {
		return changes, nil, warnings, nil
	}
	caps, err := s.Store.Capabilities(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	// The Cluster, first in the input, is written last, so an interrupted create leaves no cluster.yaml.
	if writes[0].kind == v1alpha1.KindCluster {
		writes = append(writes[1:], writes[0])
	}
	// The tent version comes first: it guards the specs that follow, and a run after an interruption may find
	// nothing else to write.
	steps := []step{func(ctx context.Context) error {
		return statestore.RaiseVersion(ctx, s.Store, in.layout, s.Version)
	}}
	for _, w := range writes {
		var opts statestore.PutOptions
		if caps.ConditionalPut {
			opts = statestore.PutOptions{IfNoneMatch: w.version == "", IfMatch: w.version}
		}
		steps = append(steps, func(ctx context.Context) error {
			_, err := s.Store.Put(ctx, w.path, w.data, opts)
			if errors.Is(err, statestore.ErrPreconditionFailed) {
				return errors.New(w.label + " changed meanwhile; run the command again")
			}
			return err
		})
	}
	return changes, steps, warnings, nil
}

// leftovers returns an error for each node group left from an interrupted create that the input does not have.
func (s *Service) leftovers(in input, st map[string]entry) []error {
	var errs []error
	for _, p := range slices.Sorted(maps.Keys(st)) {
		name, ok := groupName(in.layout, p)
		if ok && !slices.ContainsFunc(in.docs, func(d doc) bool { return d.path == p }) {
			errs = append(errs, errors.New(groupLabel(in.layout.Cluster(), name)+
				" is left from an interrupted create; add it to the spec or run delete cluster first"))
		}
	}
	return errs
}

// write is a doc to write over the stored version, or over nothing when the version is empty.
type write struct {
	doc
	version statestore.Version
}

// decide returns what writing d as m says does to the stored object e, which exists when ok is set. With leftovers,
// the stored node groups are leftovers of an interrupted create.
func (s *Service) decide(m mode, d doc, e entry, ok, leftovers bool, readVersion statestore.Version) (
	Action, error,
) {
	switch {
	case m == save && !ok:
		return "", s.notFound(d.label)
	case m == save && e.version != readVersion:
		return "", errors.New(d.label + " changed while you edited it; run edit again")
	case ok && bytes.Equal(e.data, d.data):
		return Unchanged, nil
	case m == create && !ok:
		return Created, nil
	case m == create && leftovers:
		return Replaced, nil
	case m == create:
		return "", errors.New(d.label + " already exists; change it with edit or replace -f")
	case !ok:
		return "", fmt.Errorf("%w; create it with create -f", s.notFound(d.label))
	}
	return Replaced, nil
}

func (s *Service) notFound(label string) error {
	return fmt.Errorf("%s not found in %s", label, s.Store)
}

// entry is one object in the store.
type entry struct {
	data    []byte
	version statestore.Version
}

// load reads a cluster's specs, by path.
func (s *Service) load(ctx context.Context, l statestore.Layout) (map[string]entry, error) {
	paths, err := s.Store.List(ctx, l.NodeGroups())
	if err != nil {
		return nil, err
	}
	paths = slices.DeleteFunc(paths, func(p string) bool { return !isGroup(l, p) })
	st := make(map[string]entry, len(paths)+1)
	for _, p := range append(paths, l.ClusterSpec()) {
		data, v, err := s.Store.Get(ctx, p)
		switch {
		case errors.Is(err, statestore.ErrNotFound):
			continue // deleted meanwhile
		case err != nil:
			return nil, err
		}
		st[p] = entry{data: data, version: v}
	}
	return st, nil
}

// groupName returns the name of the node group whose spec is at p, and false when p is not the path of a node
// group's spec: nodegroups/<name>.yaml.
func groupName(l statestore.Layout, p string) (string, bool) {
	file, ok := strings.CutPrefix(p, l.NodeGroups())
	name, yaml := strings.CutSuffix(file, ".yaml")
	return name, ok && yaml && !strings.Contains(file, "/")
}

// isGroup reports whether p is the path of a node group's spec.
func isGroup(l statestore.Layout, p string) bool {
	_, ok := groupName(l, p)
	return ok
}

// validate checks the cluster that results from storing the input: the stored objects that the input does not
// replace, and the input. A Cluster that replaces a stored one keeps its provider and region, and the stored one must
// decode to show them. It returns the warnings about the resulting cluster.
func (s *Service) validate(st map[string]entry, in input) ([]string, error) {
	replaced := make(map[string]bool, len(in.docs))
	for _, d := range in.docs {
		replaced[d.path] = true
	}
	objs, err := decodeStored(in.layout, st, replaced)
	if err != nil {
		return nil, err
	}
	var moves v1alpha1.Errors
	if in.cluster != nil {
		if e, ok := st[in.layout.ClusterSpec()]; ok {
			stored, err := decodeCluster(in.layout.ClusterSpec(), e.data)
			if err != nil {
				return nil, err
			}
			moves = cloudMoves(stored, in.cluster)
		}
		objs.Cluster = in.cluster
	}
	objs.NodeGroups = append(objs.NodeGroups, in.groups...)
	warnings, err := check(objs, s.Validate, s.channel)
	return warnings, withProblems(moves, err)
}

// cloudMoves returns a problem for the provider and one for the region of the cluster c when they differ from those
// of the stored cluster, both with their defaults: the cluster's cloud objects stay where tent made them, and tent
// would no longer find them. An empty value is not compared: a new one fails validation, and a stored one names no
// cloud.
func cloudMoves(stored, c *v1alpha1.Cluster) v1alpha1.Errors {
	was, now := withDefaults(stored).Spec.Cloud, withDefaults(c).Spec.Cloud
	var errs v1alpha1.Errors
	for _, f := range []struct{ path, was, now string }{
		{"spec.cloud.provider", string(was.Provider), string(now.Provider)},
		{"spec.cloud.region", was.Region, now.Region},
	} {
		if f.was != "" && f.now != "" && f.was != f.now {
			errs = append(errs, v1alpha1.FieldError{
				Object: clusterObject(c), Path: f.path,
				Detail: "cannot change from " + f.was + " to " + f.now + "; a cluster moves by creating a new one",
			})
		}
	}
	return errs
}

// withProblems returns the problems first, followed by those of err, an error of Check, as one v1alpha1.Errors. It
// returns err alone when there are no problems first or err is another error.
func withProblems(first v1alpha1.Errors, err error) error {
	more, ok := errors.AsType[v1alpha1.Errors](err)
	if len(first) == 0 || (err != nil && !ok) {
		return err
	}
	return append(first, more...)
}

// clusterObject names the cluster c in a problem, as v1alpha1.Validate does: Cluster prod, or Cluster (no name).
func clusterObject(c *v1alpha1.Cluster) string {
	return v1alpha1.KindCluster + " " + cmp.Or(c.Metadata.Name, "(no name)")
}

// Check checks a cluster and its node groups on their own, without a state store: it fills in the defaults on copies,
// validates them, and checks the cluster's channel and Nomad version against the channels embedded in tent.
func Check(objs spec.Objects, opts v1alpha1.ValidateOptions) error {
	_, err := check(objs, opts, channels.Load)
	return err
}

// channelLookup returns the release channel called name.
type channelLookup func(name string) (*channels.Channel, error)

// channel returns the release channel called name: that of Channels, or else the embedded one.
func (s *Service) channel(name string) (*channels.Channel, error) {
	if s.Channels != nil {
		return s.Channels(name)
	}
	return channels.Load(name)
}

// check is Check with the channels that lookup finds. It returns the warnings about a valid cluster.
func check(objs spec.Objects, opts v1alpha1.ValidateOptions, lookup channelLookup) ([]string, error) {
	var (
		c   *v1alpha1.Cluster
		err error
	)
	if objs.Cluster != nil {
		if c, err = clone(objs.Cluster); err != nil {
			return nil, err
		}
	}
	groups := make([]*v1alpha1.NodeGroup, 0, len(objs.NodeGroups))
	for _, g := range objs.NodeGroups {
		if g == nil {
			continue
		}
		cg, err := clone(g)
		if err != nil {
			return nil, err
		}
		groups = append(groups, cg)
	}
	v1alpha1.SetDefaults(c, groups)
	ch, err := checkCluster(c, groups, opts, lookup)
	if err != nil {
		return nil, err
	}
	return warnings(c, ch), nil
}

// checkCluster checks a cluster and its node groups, all with their defaults, as v1alpha1.Validate does, and the
// cluster's channel, which lookup finds: tent must know it, and it must allow the Nomad version that the cluster sets.
// The problems come as one v1alpha1.Errors, the channel's first. Without problems, it returns the channel.
func checkCluster(c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup, opts v1alpha1.ValidateOptions,
	lookup channelLookup,
) (*channels.Channel, error) {
	err := v1alpha1.Validate(c, groups, opts)
	problems, ok := errors.AsType[v1alpha1.Errors](err)
	if err != nil && !ok {
		return nil, err
	}
	ch, first := channelProblems(c, problems, lookup)
	if err := withProblems(first, err); err != nil {
		return nil, err
	}
	return ch, nil
}

// channelProblems returns the channel of the cluster c, which has its defaults, and the problems with it: tent must
// know the channel, and the channel must allow the Nomad version that c sets. A channel name that has a problem among
// problems already is not looked up, and then there is no channel.
func channelProblems(c *v1alpha1.Cluster, problems v1alpha1.Errors, lookup channelLookup) (
	*channels.Channel, v1alpha1.Errors,
) {
	const channelPath = "spec.channel"
	object := clusterObject(c)
	if slices.ContainsFunc(problems, func(p v1alpha1.FieldError) bool {
		return p.Object == object && p.Path == channelPath
	}) {
		return nil, nil
	}
	ch, err := lookup(c.Spec.Channel)
	if err != nil {
		return nil, v1alpha1.Errors{{Object: object, Path: channelPath, Detail: err.Error()}}
	}
	if v := c.Spec.Nomad.Version; v != "" {
		if err := ch.Allows(v); err != nil {
			return ch, v1alpha1.Errors{{Object: object, Path: "spec.nomad.version", Detail: err.Error()}}
		}
	}
	return ch, nil
}

// decodeStored decodes a cluster's stored specs except the paths in skip. The Cluster is nil when cluster.yaml is
// missing or skipped; the node groups are sorted by name.
func decodeStored(l statestore.Layout, st map[string]entry, skip map[string]bool) (spec.Objects, error) {
	var objs spec.Objects
	for _, p := range slices.Sorted(maps.Keys(st)) {
		var err error
		switch {
		case skip[p]:
		case p == l.ClusterSpec():
			objs.Cluster, err = decodeCluster(p, st[p].data)
		default:
			var g *v1alpha1.NodeGroup
			if g, err = decodeGroup(p, st[p].data); err == nil {
				objs.NodeGroups = append(objs.NodeGroups, g)
			}
		}
		if err != nil {
			return spec.Objects{}, err
		}
	}
	sortGroups(objs.NodeGroups)
	return objs, nil
}

func sortGroups(groups []*v1alpha1.NodeGroup) {
	slices.SortStableFunc(groups, func(a, b *v1alpha1.NodeGroup) int {
		return strings.Compare(a.Metadata.Name, b.Metadata.Name)
	})
}

// clone returns a deep copy of an object.
func clone[T any](v *T) (*T, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("copy %T: %w", v, err)
	}
	c := new(T)
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("copy %T: %w", v, err)
	}
	return c, nil
}

// decodeCluster decodes the Cluster stored at p. A file that does not decode fails with brokenCluster.
func decodeCluster(p string, data []byte) (*v1alpha1.Cluster, error) {
	objs, err := spec.Decode(data)
	switch {
	case err != nil:
		return nil, brokenCluster(p, err)
	case objs.Cluster == nil || len(objs.NodeGroups) > 0:
		return nil, brokenCluster(p, errors.New("want one "+v1alpha1.KindCluster))
	}
	return objs.Cluster, nil
}

// brokenCluster returns the error of the stored Cluster at p that does not decode, for the reason err, which it wraps.
// It says how to repair the file: tent reads the cluster's cloud from it, so deleting it would leave the cluster's
// cloud objects behind.
func brokenCluster(p string, err error) error {
	return fmt.Errorf("%s: %w; fix %s in the state store by hand and keep its spec.cloud.provider and "+
		"spec.cloud.region, since deleting the file would leave the cloud objects of the cluster behind", p, err, p)
}

func decodeGroup(p string, data []byte) (*v1alpha1.NodeGroup, error) {
	objs, err := spec.Decode(data)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%s: %w", p, err)
	case objs.Cluster != nil || len(objs.NodeGroups) != 1:
		return nil, fmt.Errorf("%s: want one %s", p, v1alpha1.KindNodeGroup)
	}
	return objs.NodeGroups[0], nil
}
