package nomadops

import (
	"context"
	"net/http"

	"github.com/hashicorp/nomad/api"
)

const jobsPath = "/v1/jobs"

// allNamespaces is Nomad's name for the jobs of every namespace in a query.
const allNamespaces = "*"

// Job is a job as the servers list it.
type Job struct {
	Namespace string
	ID        string
}

// Jobs returns the jobs of every namespace, in the order that the server lists them. A job that was stopped stays
// listed until it is purged.
func (c *Client) Jobs(ctx context.Context) ([]Job, error) {
	var stubs []*api.JobListStub
	err := c.call(ctx, http.MethodGet, jobsPath, func(ctx context.Context) error {
		// Not Jobs().List, which sorts the list by ID and panics on a null in it.
		q := query(ctx)
		q.Namespace = allNamespaces
		_, err := c.api.Raw().Query(jobsPath, &stubs, q)
		return err
	})
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(stubs))
	for _, s := range stubs {
		if s != nil {
			jobs = append(jobs, Job{Namespace: s.Namespace, ID: s.ID})
		}
	}
	return jobs, nil
}
