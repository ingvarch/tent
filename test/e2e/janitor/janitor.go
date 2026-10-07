// Package janitor finds and deletes what earlier E2E runs left in a Vultr account.
package janitor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

// ClusterPrefix starts the name of every cluster the E2E suite makes.
const ClusterPrefix = "e2e-"

// The kinds of objects, in the order the sweep deletes them.
const (
	KindInstance = "instance"
	KindFirewall = "firewall"
	KindVPC      = "vpc"
	KindSSHKey   = "ssh-key"
)

// API is the part of the Vultr API the janitor uses; *vultrapi.Client implements it.
type API interface {
	Instances(ctx context.Context) ([]vultrapi.Instance, error)
	VPCs(ctx context.Context) ([]vultrapi.VPC, error)
	FirewallGroups(ctx context.Context) ([]vultrapi.FirewallGroup, error)
	SSHKeys(ctx context.Context) ([]vultrapi.SSHKey, error)
	DeleteInstance(ctx context.Context, id string) error
	DeleteVPC(ctx context.Context, id string) error
	DeleteFirewallGroup(ctx context.Context, id string) error
	DeleteSSHKey(ctx context.Context, id string) error
}

var _ API = (*vultrapi.Client)(nil)

// Object is a cloud object that belongs to an E2E cluster.
type Object struct {
	Kind    string
	ID      string
	Name    string
	Cluster string
	Created time.Time
}

// String is the line that names the object in the output.
func (o Object) String() string {
	return fmt.Sprintf("%s %s %s cluster=%s created=%s", o.Kind, o.ID, o.Name, o.Cluster,
		o.Created.UTC().Format(time.RFC3339))
}

// Janitor finds and deletes E2E leftovers. Every must be positive.
type Janitor struct {
	API     API
	Out     io.Writer
	Every   time.Duration
	Timeout time.Duration
}

// New returns a janitor that lists the instances every 10 seconds, for up to 5 minutes, to see them go.
func New(api API, out io.Writer) *Janitor {
	return &Janitor{API: api, Out: out, Every: 10 * time.Second, Timeout: 5 * time.Minute}
}

const (
	tagPrefix    = "tent/cluster="
	markerPrefix = "tent:"
	markerField  = "cluster"
)

// instanceCluster is the value of the instance's tent/cluster tag; it is empty when the instance has none or has
// two with different values.
func instanceCluster(tags []string) string {
	cluster, found := "", false
	for _, tag := range tags {
		value, ok := strings.CutPrefix(tag, tagPrefix)
		if !ok {
			continue
		}
		if found && value != cluster {
			return ""
		}
		cluster, found = value, true
	}
	return cluster
}

// markerCluster is the cluster field of a marker, "tent:" followed by fields "key=value" separated by ";". It is
// empty when the text is no marker, a field has no key or no value, or the cluster field comes twice.
func markerCluster(text string) string {
	fields, ok := strings.CutPrefix(text, markerPrefix)
	if !ok {
		return ""
	}
	cluster, found := "", false
	for _, field := range strings.Split(fields, ";") {
		key, value, _ := strings.Cut(field, "=")
		if key == "" || value == "" {
			return ""
		}
		if key != markerField {
			continue
		}
		if found {
			return ""
		}
		cluster, found = value, true
	}
	return cluster
}

// Find returns every object of the E2E clusters whose oldest object is older than olderThan at now: the objects
// whose cluster starts with ClusterPrefix, sorted by kind in deletion order, then cluster, then ID.
func (j *Janitor) Find(ctx context.Context, olderThan time.Duration, now time.Time) ([]Object, error) {
	var found []Object
	add := func(kind, id, name, cluster string, created time.Time) {
		if strings.HasPrefix(cluster, ClusterPrefix) {
			found = append(found, Object{Kind: kind, ID: id, Name: name, Cluster: cluster, Created: created})
		}
	}
	instances, err := j.API.Instances(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing instances: %w", err)
	}
	for _, in := range instances {
		add(KindInstance, in.ID, in.Label, instanceCluster(in.Tags), in.Created)
	}
	groups, err := j.API.FirewallGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing firewalls: %w", err)
	}
	for _, g := range groups {
		add(KindFirewall, g.ID, g.Description, markerCluster(g.Description), g.Created)
	}
	vpcs, err := j.API.VPCs(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing vpcs: %w", err)
	}
	for _, v := range vpcs {
		add(KindVPC, v.ID, v.Description, markerCluster(v.Description), v.Created)
	}
	keys, err := j.API.SSHKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing ssh-keys: %w", err)
	}
	for _, k := range keys {
		add(KindSSHKey, k.ID, k.Name, markerCluster(k.Name), k.Created)
	}

	oldest := map[string]time.Time{}
	for _, o := range found {
		if first, ok := oldest[o.Cluster]; !ok || o.Created.Before(first) {
			oldest[o.Cluster] = o.Created
		}
	}
	found = slices.DeleteFunc(found, func(o Object) bool { return now.Sub(oldest[o.Cluster]) <= olderThan })
	slices.SortFunc(found, func(a, b Object) int {
		return cmp.Or(
			cmp.Compare(slices.Index(deleteOrder, a.Kind), slices.Index(deleteOrder, b.Kind)),
			cmp.Compare(a.Cluster, b.Cluster),
			cmp.Compare(a.ID, b.ID),
		)
	})
	return found, nil
}

var deleteOrder = []string{KindInstance, KindFirewall, KindVPC, KindSSHKey}

// Sweep deletes the objects of the four kinds: the instances first, then, once none of them is listed any more,
// the firewall groups, the VPCs and the SSH keys. Vultr refuses a VPC delete for a while after the instances are
// gone, so a refused VPC delete is repeated every Every until Timeout passes and only its last error is reported.
// It writes "delete <object>" to Out before each delete. A failed delete does not stop the sweep, and neither does
// an instance that is still listed after Timeout. The error joins every failure and is nil when none failed. When
// ctx has ended after the instances, Sweep deletes nothing more and the error holds the error of ctx.
func (j *Janitor) Sweep(ctx context.Context, objects []Object) error {
	failures, deleted := j.deleteKind(ctx, objects, KindInstance, j.API.DeleteInstance)
	if len(deleted) > 0 {
		if err := j.waitGone(ctx, deleted); err != nil {
			failures = append(failures, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, step := range []struct {
		kind   string
		delete func(context.Context, string) error
	}{
		{KindFirewall, j.API.DeleteFirewallGroup},
		{KindVPC, j.retrying(j.API.DeleteVPC)},
		{KindSSHKey, j.API.DeleteSSHKey},
	} {
		more, _ := j.deleteKind(ctx, objects, step.kind, step.delete)
		failures = append(failures, more...)
	}
	return errors.Join(failures...)
}

// retrying repeats del every Every until it succeeds, Timeout passes or ctx ends, and returns the last error.
func (j *Janitor) retrying(del func(context.Context, string) error) func(context.Context, string) error {
	return func(ctx context.Context, id string) error {
		retryCtx, cancel := context.WithTimeout(ctx, j.Timeout)
		defer cancel()
		ticker := time.NewTicker(j.Every)
		defer ticker.Stop()
		for {
			err := del(ctx, id)
			if err == nil {
				return nil
			}
			select {
			case <-retryCtx.Done():
				return err
			case <-ticker.C:
			}
		}
	}
}

// deleteKind deletes the objects of one kind and returns the failures and the IDs it deleted.
func (j *Janitor) deleteKind(
	ctx context.Context, objects []Object, kind string, del func(context.Context, string) error,
) (failures []error, deleted []string) {
	for _, o := range objects {
		if o.Kind != kind {
			continue
		}
		_, _ = fmt.Fprintf(j.Out, "delete %s\n", o)
		if err := del(ctx, o.ID); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", o, err))
			continue
		}
		deleted = append(deleted, o.ID)
	}
	return failures, deleted
}

// waitGone lists the instances every Every until none of the IDs is listed, for up to Timeout. It returns nil when
// ctx ends.
func (j *Janitor) waitGone(ctx context.Context, ids []string) error {
	waitCtx, cancel := context.WithTimeout(ctx, j.Timeout)
	defer cancel()
	ticker := time.NewTicker(j.Every)
	defer ticker.Stop()
	for {
		listed, err := j.API.Instances(ctx)
		if err != nil {
			return fmt.Errorf("listing instances: %w", err)
		}
		left := slices.DeleteFunc(slices.Clone(ids), func(id string) bool {
			return !slices.ContainsFunc(listed, func(in vultrapi.Instance) bool { return in.ID == id })
		})
		if len(left) == 0 {
			return nil
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("instances still listed after %s: %s", j.Timeout, strings.Join(left, ", "))
		case <-ticker.C:
		}
	}
}
