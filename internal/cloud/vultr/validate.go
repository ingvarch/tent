package vultr

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/model"
)

// Validate checks the specs against Vultr's live API before tent changes anything: the region exists, each node
// group's plan exists and the region can deploy it now, and each group's image is one that tent supports and Vultr
// offers. It fills in the defaults on copies, so the specs stay as they are, and makes three calls: the region's
// availability, the plans and the images.
//
// It returns v1alpha1.Errors with every problem: the cluster's first, then each node group's, by name. With an
// unknown region it skips the availability checks. Any other error of the API is returned wrapped, not as a problem.
func (p *Provider) Validate(ctx context.Context, c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) error {
	m, err := model.New(c, groups)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if err := onVultr(m); err != nil {
		return err
	}
	o, err := p.offer(ctx, m.Region)
	if err != nil {
		return fmt.Errorf("preflight of cluster %s: %w", m.Name, err)
	}
	if errs := o.problems(m); len(errs) > 0 {
		return errs
	}
	return nil
}

// offer is what Vultr offers a cluster in one region.
type offer struct {
	region      string
	regionKnown bool            // whether Vultr has the region
	available   map[string]bool // the plans the region can deploy now
	plans       map[string]bool // every plan, by id
	images      map[int]bool    // every image, by os_id
}

// offer reads what Vultr offers in region. When the availability call gets the answer to an unknown region, the offer
// says that Vultr has no such region.
func (p *Provider) offer(ctx context.Context, region string) (*offer, error) {
	available, err := p.api.AvailablePlans(ctx, region, "")
	regionKnown := !unknownRegion(err)
	if err != nil && regionKnown {
		return nil, err
	}
	plans, err := p.api.ListPlans(ctx, "")
	if err != nil {
		return nil, err
	}
	systems, err := p.api.ListOS(ctx)
	if err != nil {
		return nil, err
	}
	o := &offer{
		region:      region,
		regionKnown: regionKnown,
		available:   make(map[string]bool, len(available)),
		plans:       make(map[string]bool, len(plans)),
		images:      make(map[int]bool, len(systems)),
	}
	for _, id := range available {
		o.available[id] = true
	}
	for _, plan := range plans {
		o.plans[plan.ID] = true
	}
	for _, im := range systems {
		o.images[im.ID] = true
	}
	return o, nil
}

// unknownRegion reports whether err is Vultr's answer to the availability of a region that it does not have: a 400
// of the class ErrInvalid, such as "400 Invalid region.". Other ErrInvalid answers, such as a 405, are API errors.
func unknownRegion(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusBadRequest && errors.Is(err, ErrInvalid)
}

// problems returns what Vultr cannot give the cluster m, in the order Validate reports it.
func (o *offer) problems(m *model.Cluster) v1alpha1.Errors {
	var errs v1alpha1.Errors
	if !o.regionKnown {
		errs = append(errs, v1alpha1.FieldError{
			Object: v1alpha1.KindCluster + " " + m.Name, Path: "spec.cloud.region",
			Detail: fmt.Sprintf("Vultr has no region %q", m.Region),
		})
	}
	for _, g := range m.Groups {
		object := v1alpha1.KindNodeGroup + " " + g.Name
		if detail := o.planProblem(g.MachineType); detail != "" {
			errs = append(errs, v1alpha1.FieldError{Object: object, Path: "spec.machineType", Detail: detail})
		}
		if detail := o.imageProblem(g.Image); detail != "" {
			errs = append(errs, v1alpha1.FieldError{Object: object, Path: "spec.image", Detail: detail})
		}
	}
	return errs
}

// planProblem returns what is wrong with a node group's plan, or "" when nothing is.
func (o *offer) planProblem(plan string) string {
	switch {
	case !o.plans[plan]:
		return fmt.Sprintf("plan %q does not exist", plan)
	case o.regionKnown && !o.available[plan]:
		return fmt.Sprintf("plan %q is not available in %s now", plan, o.region)
	}
	return ""
}

// imageProblem returns what is wrong with a node group's image, or "" when nothing is.
func (o *offer) imageProblem(name string) string {
	id, ok := osID(name)
	switch {
	case !ok:
		return fmt.Sprintf("tent supports %s on Vultr, not %q", imageNames(), name)
	case !o.images[id]:
		return fmt.Sprintf("Vultr does not offer %s (os_id %d) now", name, id)
	}
	return ""
}
