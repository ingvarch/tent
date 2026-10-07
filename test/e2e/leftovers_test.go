package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ingvarch/tent/test/e2e/janitor"
	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

// inventory lists the four kinds of objects a cluster can leave on Vultr.
type inventory interface {
	Instances(ctx context.Context) ([]vultrapi.Instance, error)
	VPCs(ctx context.Context) ([]vultrapi.VPC, error)
	FirewallGroups(ctx context.Context) ([]vultrapi.FirewallGroup, error)
	SSHKeys(ctx context.Context) ([]vultrapi.SSHKey, error)
}

// leftoversOf returns one line per listed object that belongs to cluster, an instance's with its three states: an
// instance with the tag
// tent/cluster=<cluster> or a label that starts with <cluster>-, a VPC or firewall group with the cluster's name in
// its description, an SSH key with it in its name. Matching free text is wider than the janitor's markers on
// purpose: it also finds an object that tent made without its marker.
func leftoversOf(
	cluster string, instances []vultrapi.Instance, vpcs []vultrapi.VPC, groups []vultrapi.FirewallGroup,
	keys []vultrapi.SSHKey,
) []string {
	var out []string
	add := func(kind, id, name string) { out = append(out, kind+" "+id+" "+name) }
	for _, in := range instances {
		if slices.Contains(in.Tags, "tent/cluster="+cluster) || strings.HasPrefix(in.Label, cluster+"-") {
			add(janitor.KindInstance, in.ID, in.Label+" status="+in.Status+"/"+in.PowerStatus+"/"+in.ServerStatus)
		}
	}
	for _, v := range vpcs {
		if strings.Contains(v.Description, cluster) {
			add(janitor.KindVPC, v.ID, v.Description)
		}
	}
	for _, g := range groups {
		if strings.Contains(g.Description, cluster) {
			add(janitor.KindFirewall, g.ID, g.Description)
		}
	}
	for _, k := range keys {
		if strings.Contains(k.Name, cluster) {
			add(janitor.KindSSHKey, k.ID, k.Name)
		}
	}
	return out
}

// leftoversNow lists the four kinds and returns the objects that belong to cluster.
func leftoversNow(ctx context.Context, inv inventory, cluster string) ([]string, error) {
	instances, err := inv.Instances(ctx)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	vpcs, err := inv.VPCs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list VPCs: %w", err)
	}
	groups, err := inv.FirewallGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("list firewall groups: %w", err)
	}
	keys, err := inv.SSHKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list SSH keys: %w", err)
	}
	return leftoversOf(cluster, instances, vpcs, groups, keys), nil
}

// waitNoLeftovers lists the objects of cluster every `every` until there are none or timeout has passed, and logs
// through logf what each poll that found objects saw, with the time since the first poll, and each poll whose lists
// failed, with the time they took. The error of a timeout names the objects that were still there.
func waitNoLeftovers(
	ctx context.Context, inv inventory, cluster string, every, timeout time.Duration, logf func(string, ...any),
) error {
	start := time.Now()
	return pollUntil(ctx, every, timeout, func(ctx context.Context) string {
		listed := time.Now()
		left, err := leftoversNow(ctx, inv, cluster)
		if err != nil {
			logf("%s: a poll failed; its lists took %s: %v", cluster, time.Since(listed).Round(time.Second), err)
			return err.Error()
		}
		if len(left) == 0 {
			return ""
		}
		logf("%s after %s still listed: %s", cluster, time.Since(start).Round(time.Second), strings.Join(left, "; "))
		return "left behind: " + strings.Join(left, "; ")
	})
}
