package vultr

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/model"
)

// maxVPCsPerRegion is the most VPCs that Vultr allows in a region.
const maxVPCsPerRegion = 5

// vpcTask makes the cluster's private network a Vultr VPC in the cluster's region with the model's CIDR. The VPC's
// description is its marker.
type vpcTask struct {
	api      API
	cluster  string
	region   string
	cidr     netip.Prefix
	op       string  // the operation id that the create puts into the marker
	attempts opState // what the attempts of the create share
}

// vpcTaskOf returns the task of m's VPC.
func (p *Provider) vpcTaskOf(m *model.Cluster) engine.Task {
	return &vpcTask{api: p.api, cluster: m.Name, region: m.Region, cidr: m.CIDR, op: p.opID()}
}

// Key returns vultr.VPC/<cluster>.
func (t *vpcTask) Key() engine.Key { return vpcKey(t.cluster) }

// Deps returns nothing: a VPC needs no other object.
func (t *vpcTask) Deps() []engine.Key { return nil }

// Plan plans a create, with the CIDR and the region as its diff, when the snapshot has no VPC for the cluster. When it
// has one, Plan sets the output id. It fails when that VPC has another region or CIDR than the model: moving the
// network would need every node replaced first.
func (t *vpcTask) Plan(_ context.Context, env *engine.Env) (engine.Change, error) {
	s, err := snapshotOf(env)
	if err != nil {
		return engine.Change{}, err
	}
	cur, ok := s.vpc(t.Key())
	if !ok {
		return engine.Change{Action: engine.Create, Diff: []engine.FieldDiff{
			{Field: "cidr", New: t.cidr.String()},
			{Field: "region", New: t.region},
		}}, nil
	}
	network := cur.V4Subnet + "/" + strconv.Itoa(cur.V4SubnetMask) // as Vultr gives it
	if p, err := netip.ParsePrefix(network); err != nil || p != t.cidr || cur.Region != t.region {
		return engine.Change{}, fmt.Errorf("the VPC of cluster %s is %s in %s; the spec asks for %s in %s, and tent "+
			"cannot move a cluster's network", t.cluster, network, cur.Region, t.cidr, t.region)
	}
	env.Outputs.Set(t.Key(), outputID, cur.ID)
	return engine.Change{Action: engine.Noop}, nil
}

// Apply creates the VPC and sets the output id. Plan plans no other change.
func (t *vpcTask) Apply(ctx context.Context, env *engine.Env, _ engine.Change) error {
	id, err := createWithOp(ctx, &t.attempts, opFinder(t.api.ListVPCs, vpcType, t.cluster, t.op), t.createVPC)
	if err != nil {
		return err
	}
	env.Outputs.Set(t.Key(), outputID, id)
	return nil
}

// createVPC sends the create of the VPC and returns its id. Vultr refuses it with ErrLimitReached when the region has
// as many VPCs as Vultr allows; the error then names that limit, which Vultr does not raise on request.
func (t *vpcTask) createVPC(ctx context.Context) (string, error) {
	v, err := t.api.CreateVPC(ctx, &govultr.VPCReq{
		Region:       t.region,
		Description:  Marker{Cluster: t.cluster, Kind: KindVPC, Op: t.op}.String(),
		V4Subnet:     t.cidr.Addr().String(),
		V4SubnetMask: t.cidr.Bits(),
	})
	switch {
	case errors.Is(err, ErrLimitReached):
		limit := fmt.Sprintf("%s may already have %d VPCs, the most Vultr allows in a region", t.region,
			maxVPCsPerRegion)
		return "", &objectLimitError{limit: limit, err: err}
	case err != nil:
		return "", err
	}
	return v.ID, nil
}

// Delete deletes the VPC obj. A VPC that is gone counts as deleted. For up to about 20 s after the VPC's servers are
// gone, Vultr still counts them attached and refuses the delete with ErrInUse, so the engine tries it again.
func (t *vpcTask) Delete(ctx context.Context, _ *engine.Env, obj engine.Object) error {
	return deleted(t.api.DeleteVPC(ctx, obj.ID))
}
