package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// UpgradeCluster plans the move of a cluster to the Nomad version that its channel recommends, and with apply writes
// that version into spec.nomad.version of the stored Cluster under the cluster's lock, where it plans again. It
// reads the state store and, when an upgrade is due, the signed checksums of the new release; it reaches no cloud
// and no Nomad. A plan with nothing to upgrade takes no lock and writes nothing. When the lock is lost after the
// write, the plan comes back together with an error that matches statestore.ErrLockLost.
func (s *Service) UpgradeCluster(ctx context.Context, cluster string, apply bool) (_ UpgradePlan, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := clusterLayout(cluster)
	if err != nil {
		return UpgradePlan{}, err
	}
	var (
		plan     UpgradePlan
		warnings []string
		verified string
	)
	err = s.change(ctx, l, "upgrade", apply, func(ctx context.Context) ([]step, error) {
		p, steps, w, err := s.planUpgrade(ctx, l, &verified)
		plan, warnings = p, w
		return steps, err
	})
	if err != nil && !Saved(err) {
		return UpgradePlan{}, err
	}
	plan.Applied = apply
	if apply {
		s.warn(warnings...)
	}
	return plan, err
}

// planUpgrade decides what an upgrade of the cluster does, from one read of its specs and of its completed spec, and
// returns the writes that carry it out and the warnings about the cluster that results. It checks the release of the
// Nomad version that it writes, unless an earlier plan of the run checked that version, which verified holds; the
// version it checks goes into verified.
func (s *Service) planUpgrade(ctx context.Context, l statestore.Layout, verified *string) (
	UpgradePlan, []step, []string, error,
) {
	fail := func(err error) (UpgradePlan, []step, []string, error) { return UpgradePlan{}, nil, nil, err }
	st, err := s.load(ctx, l)
	if err != nil {
		return fail(err)
	}
	if _, ok := st[l.ClusterSpec()]; !ok {
		return fail(s.notFound(clusterLabel(l.Cluster())))
	}
	objs, err := decodeStored(l, st, nil)
	if err != nil {
		return fail(err)
	}
	ch, err := s.checkForUpgrade(objs)
	if err != nil {
		return fail(err)
	}
	completed, err := s.readCompleted(ctx, l)
	if err != nil {
		return fail(err)
	}
	stored := objs.Cluster
	plan := UpgradePlan{
		Cluster: stored.Metadata.Name, Channel: ch.Name, Recommended: ch.Nomad.Recommended,
		Spec: stored.Spec.Nomad.Version,
	}
	pinned, pinErr := storedPin(l, completed)
	switch {
	case plan.Spec != "":
		err = moveError(plan.Cluster, l, pinned, plan.Spec)
	case pinErr != nil:
		err = unreadablePin(l, pinErr)
	case pinned != "" && !ch.OlderThanMinimum(pinned):
		err = pinOutsideChannel(plan.Cluster, l, ch, pinned)
	}
	if err != nil {
		return fail(err)
	}
	plan.Pinned, plan.Current = pinned, cmp.Or(plan.Spec, pinned)
	if plan.Current == "" || channels.Compare(plan.Recommended, plan.Current) <= 0 {
		return plan, nil, nil, nil
	}
	if err := upgradeMove(plan.Cluster, ch, pinned, plan.Recommended); err != nil {
		return fail(err)
	}
	if *verified != plan.Recommended {
		err := assets.CheckNomad(ctx, s.assetOptions(), plan.Recommended, cloud.ArchAMD64, cloud.ArchARM64)
		if err != nil {
			return fail(fmt.Errorf("tent could not verify the release of Nomad %s, which channel %s recommends: %w",
				plan.Recommended, ch.Name, err))
		}
		*verified = plan.Recommended
	}
	plan.Target = plan.Recommended
	next, err := clone(stored)
	if err != nil {
		return fail(err)
	}
	next.Spec.Nomad.Version = plan.Target
	in, err := newInput(spec.Objects{Cluster: next})
	if err != nil {
		return fail(err)
	}
	_, steps, warnings, err := s.planOver(ctx, in, replace, "", st, pinned)
	if err != nil {
		return fail(err)
	}
	return plan, steps, warnings, nil
}

// checkForUpgrade checks the stored specs as an update does, with one difference: a Nomad version that the spec sets
// and that is older than the channel's minimum is left out of the check, so that the upgrade can mend it. It returns
// the cluster's channel.
func (s *Service) checkForUpgrade(objs spec.Objects) (*channels.Channel, error) {
	c, groups, err := defaulted(objs)
	if err != nil {
		return nil, err
	}
	if s.setsOlderThanMinimum(c) {
		c.Spec.Nomad.Version = ""
	}
	return checkCluster(c, groups, s.Validate, s.channel)
}

// upgradeMove returns the error of a cluster pinned to the Nomad version pinned when it may not move to the version to
// that channel ch recommends, and nil otherwise. A move across more than one minor version names the way through the
// minor version between, unless the channel allows no release of it.
func upgradeMove(cluster string, ch *channels.Channel, pinned, to string) error {
	v, ok := errors.AsType[*channels.VersionError](channels.CheckMove(pinned, to))
	if !ok {
		return nil
	}
	msg := fmt.Sprintf("%s is pinned to Nomad %s, and channel %s recommends %s: %s", clusterLabel(cluster), pinned,
		ch.Name, to, v)
	if !sameMajor(pinned, to) {
		return errors.New(msg)
	}
	next := channels.NextMinor(pinned)
	minor := strings.TrimSuffix(next, ".0")
	if channels.Compare(ch.Nomad.Minimum, channels.NextMinor(next)) < 0 {
		return fmt.Errorf("%s; set spec.nomad.version to a %s release with tent edit cluster, run tent update cluster "+
			"and tent rolling-update cluster, and upgrade again", msg, minor)
	}
	return fmt.Errorf("%s; channel %s of this tent allows no %s release: move the cluster to %s with an older tent first",
		msg, ch.Name, minor, minor)
}
