package vultr_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/engine/enginetest"
	"github.com/ingvarch/tent/internal/model"
)

// fixture is a provider on a fake, and tasks of cluster prod with the kinds of their objects.
type fixture struct {
	f     *vultrfake.Fake
	p     *vultr.Provider
	tasks []engine.Task
	kinds []engine.Kind
}

// newFixture returns a provider on an empty fake, without tasks or kinds. The provider's operation ids are op-1, op-2
// and so on.
func newFixture() *fixture {
	f := vultrfake.New()
	n := 0
	p, _ := newProvider(f, vultr.WithOpIDs(func() string {
		n++
		return fmt.Sprintf("op-%d", n)
	}))
	return &fixture{f: f, p: p}
}

func (x *fixture) inventory(ctx context.Context) (engine.Snapshot, error) {
	return x.p.Inventory(ctx, "prod")
}

// env returns an Env with a fresh inventory of cluster prod.
func (x *fixture) env(t *testing.T) *engine.Env {
	t.Helper()
	return &engine.Env{Snapshot: inventory(t, x.p), Outputs: &engine.Outputs{}}
}

// plan plans the tasks against a fresh inventory of cluster prod.
func (x *fixture) plan(t *testing.T) (*engine.Plan, error) {
	t.Helper()
	snap, err := x.inventory(t.Context())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	return engine.NewPlan(t.Context(), x.tasks, x.kinds, snap)
}

// applyInBubble plans the tasks, then sets faults on the fake and applies the plan. The engine's clock is synctest's,
// so its waits take no time. It returns the events and the error of the apply, and stops the test when the plan
// fails.
func (x *fixture) applyInBubble(t *testing.T, faults func(testing.TB)) ([]engine.Event, error) {
	t.Helper()
	var events []engine.Event
	var err error
	synctest.Test(t, func(t *testing.T) {
		p, perr := x.plan(t)
		if perr != nil {
			t.Fatalf("plan: %v", perr)
		}
		faults(t)
		err = p.Apply(t.Context(), engine.ApplyOptions{OnEvent: func(e engine.Event) { events = append(events, e) }})
	})
	return events, err
}

// applyWithFaults applies the tasks as applyInBubble does, and returns the events of the apply. It stops the test
// when the apply fails, and fails it when a plan made afterwards has changes.
func (x *fixture) applyWithFaults(t *testing.T, faults func(testing.TB)) []engine.Event {
	t.Helper()
	events, err := x.applyInBubble(t, faults)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	p, err := x.plan(t)
	switch {
	case err != nil:
		t.Errorf("plan after apply: %v", err)
	case p.HasChanges():
		t.Errorf("the plan after apply has changes, want none: %+v", p.Changes())
	}
	return events
}

// createdID plans the fixture's first task against a fresh inventory, applies its create and returns its output id.
// It stops the test when the task plans no create or the apply fails.
func (x *fixture) createdID(t *testing.T) string {
	t.Helper()
	task, env := x.tasks[0], x.env(t)
	ch, err := task.Plan(t.Context(), env)
	if err != nil || ch.Action != engine.Create {
		t.Fatalf("Plan = %+v, %v; want a create", ch, err)
	}
	if err := task.Apply(t.Context(), env, ch); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	id, _ := env.Outputs.Get(task.Key(), "id")
	return id
}

// wantAdopted checks that the fixture's first task adopts the object with the id id that the fake holds: it plans no
// change and sets its output id, and applying the tasks sends no call of the vultr.API method create.
func (x *fixture) wantAdopted(t *testing.T, id, create string) {
	t.Helper()
	task, env := x.tasks[0], x.env(t)
	ch, err := task.Plan(t.Context(), env)
	if err != nil || ch.Action != engine.Noop {
		t.Fatalf("Plan = %+v, %v; want no change", ch, err)
	}
	if got, known := env.Outputs.Get(task.Key(), "id"); got != id {
		t.Errorf("output id = %q (known: %t), want %q", got, known, id)
	}
	enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
	if got := countCalls(x.f, create); got != 0 {
		t.Errorf("%d %s calls, want none", got, create)
	}
}

// wantSearchAfterLostCreate applies the tasks while the answer to the create call is lost and the list call right
// after it fails with a 500 at path. The engine must retry once, and the second attempt must search and not create
// again: the calls start with the inventory, the create and the two searches, and the create is sent once.
func (x *fixture) wantSearchAfterLostCreate(t *testing.T, create vultrfake.Call, list, path string) {
	t.Helper()
	events := x.applyWithFaults(t, func(tb testing.TB) {
		x.f.LoseResponse(tb, create.Name, 1)
		x.f.Fail(tb, list,
			vultr.NewAPIError(http.MethodGet, path, http.StatusInternalServerError, "Internal error", 0), 1)
	})
	if n := countEvents(events, engine.Retrying); n != 1 {
		t.Errorf("the engine retried %d times, want once", n)
	}
	want := slices.Concat(listCalls, []vultrfake.Call{create, {Name: list}, {Name: list}})
	calls := x.f.Calls()
	if diff := cmp.Diff(want, calls[:min(len(want), len(calls))]); diff != "" {
		t.Errorf("the first calls (-want +got):\n%s", diff)
	}
	if got := countCalls(x.f, create.Name); got != 1 {
		t.Errorf("%d %s calls, want 1", got, create.Name)
	}
}

// infraTasks returns the tasks of the engine kind kind, such as vultr.VPC, that p.BuildInfra builds for m as a Vultr
// cluster, in their order.
func infraTasks(t *testing.T, p *vultr.Provider, m model.Cluster, kind string) ([]engine.Task, error) {
	t.Helper()
	m.Provider = v1alpha1.ProviderVultr
	all, err := p.BuildInfra(t.Context(), &m)
	if err != nil {
		return nil, err
	}
	var tasks []engine.Task
	for _, task := range all {
		if task.Key().Kind == kind {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

// deleterOf returns the deleter of the engine kind kind among p's infrastructure kinds. It stops the test when p has
// no such kind.
func deleterOf(t *testing.T, p *vultr.Provider, kind string) engine.Deleter {
	t.Helper()
	kinds := p.InfraKinds()
	i := slices.IndexFunc(kinds, func(k engine.Kind) bool { return k.Name == kind })
	if i < 0 {
		t.Fatalf("InfraKinds has no kind %s", kind)
	}
	return kinds[i].Deleter
}

// countCalls returns how many calls of the vultr.API method name reached f.
func countCalls(f *vultrfake.Fake, name string) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Name == name {
			n++
		}
	}
	return n
}

// countEvents returns how many of events have the type typ.
func countEvents(events []engine.Event, typ engine.EventType) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}
