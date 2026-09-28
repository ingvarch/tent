package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

// The defaults of ApplyOptions.
const (
	defaultParallelism   = 4
	defaultChangeTimeout = 5 * time.Minute
)

// ApplyOptions control how Apply, ApplyTaskChanges and ApplyDeletes carry out a plan.
type ApplyOptions struct {
	// Parallelism is the most changes that run at once. Zero or less means 4.
	Parallelism int
	// ChangeTimeout limits each change, its retries included. Zero or less means 5 minutes.
	ChangeTimeout time.Duration
	// OnEvent, when set, is told what happens to each change. One call never calls it concurrently.
	OnEvent func(Event)
}

// EventType is what happened to a change while a plan applies.
type EventType int

// Event types. A change that runs sends Started, then Retrying before each retry, then Succeeded or Failed. A change
// that does not run sends Skipped.
const (
	// Started means the change's first attempt begins.
	Started EventType = iota
	// Succeeded means the change is done.
	Succeeded
	// Failed means the change failed with Err.
	Failed
	// Retrying means an attempt failed with Err, and the next one follows after Wait.
	Retrying
	// Skipped means the change did not run, for the reason in Cause.
	Skipped
)

var eventTypeNames = [...]string{"started", "succeeded", "failed", "retrying", "skipped"}

// String returns the event type's name in lower case, such as started.
func (t EventType) String() string {
	if t < 0 || int(t) >= len(eventTypeNames) {
		return fmt.Sprintf("EventType(%d)", int(t))
	}
	return eventTypeNames[t]
}

// MarshalText returns the event type's name, as String does.
func (t EventType) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

// Event is what happened to one change while a plan applies.
type Event struct {
	Type   EventType
	Key    Key
	ID     string // the object's ID for a delete; empty otherwise
	Action Action
	Err    error         // why an attempt failed, for Failed and Retrying
	Wait   time.Duration // the wait before the next attempt, for Retrying
	Cause  string        // why the change did not run, for Skipped, such as "vultr.VPC/prod failed"
}

// The causes of skipped changes, besides the failure of a change they wait for.
const (
	causeCancelled     = "cancelled"
	causeEarlierFailed = "an earlier change failed"
)

// Apply carries out the plan's changes with the Env that the plan was made with: its task changes, then its deletes.
// ApplyTaskChanges and ApplyDeletes carry out one part each, so that the caller can act between the two.
//
// Task changes run in parallel. A change starts once every change it depends on, directly or through tasks without
// changes, has succeeded. A failed change skips every change that waits for it that way, and the others go on.
// When every task change has succeeded, Apply deletes the objects that no task claims, kind by kind in the order of
// the kinds, the objects of one kind in parallel; otherwise it skips the deletes. A failed delete skips the deletes of
// the later kinds, and the other deletes of its kind still finish.
//
// Each change has its own deadline, retries included. A change whose error is Retryable is tried again until the wait
// before the next attempt would reach its deadline. When ctx ends, the changes that have not started are skipped.
//
// Apply returns once every change it started has finished. Its error joins the error of each failed change in the
// order of Changes, and ctx's error when ctx ended before every change succeeded. A plan applies once; Apply fails
// when the plan or one of its parts was applied before.
func (p *Plan) Apply(ctx context.Context, opts ApplyOptions) error {
	if !p.advance(unapplied, deletesBegun) {
		return errors.New("the plan was applied already")
	}
	a := newApplier(opts)
	tasks := p.taskJobs()
	a.run(ctx, tasks)
	deletes := a.runDeletes(ctx, p.deleteJobs(), skipCause(tasks))
	return result(ctx, append(tasks, deletes...))
}

// ApplyTaskChanges carries out the plan's task changes, its creates, updates and replaces, as Apply does, and none of
// its deletes. Its error joins the error of each failed task change in order, and ctx's error when ctx ended before
// every task change succeeded. A plan applies its task changes once; ApplyTaskChanges fails when they were applied
// before, by it or by Apply.
func (p *Plan) ApplyTaskChanges(ctx context.Context, opts ApplyOptions) error {
	if !p.advance(unapplied, applyingTasks) {
		return errors.New("the task changes of the plan were applied already")
	}
	jobs := p.taskJobs()
	newApplier(opts).run(ctx, jobs)
	p.endTasks(skipCause(jobs))
	return result(ctx, jobs)
}

// ApplyDeletes carries out the plan's deletes as Apply does. It fails without doing anything until ApplyTaskChanges
// has returned. When a task change failed, it skips every delete with the cause "an earlier change failed", and when
// one was cancelled, with the cause "cancelled"; its error then says that the deletes were skipped.
//
// Its error joins that, the error of each failed delete in order, and ctx's error when ctx ended before every delete
// succeeded. A plan applies its deletes once; ApplyDeletes fails when they were applied before, by it or by Apply.
func (p *Plan) ApplyDeletes(ctx context.Context, opts ApplyOptions) error {
	skip, err := p.beginDeletes()
	if err != nil {
		return err
	}
	jobs := newApplier(opts).runDeletes(ctx, p.deleteJobs(), skip)
	err = result(ctx, jobs)
	if skip != "" && len(jobs) > 0 {
		err = errors.Join(errors.New("the deletes of the plan were skipped because a task change did not succeed"), err)
	}
	return err
}

// stage is how far the applying of a plan got.
type stage int

const (
	unapplied     stage = iota // nothing has begun
	applyingTasks              // ApplyTaskChanges runs
	tasksApplied               // the task changes have ended, and the deletes have not begun
	deletesBegun               // the deletes run or ran; Apply goes here at once, so that no part runs beside it
)

// advance moves the plan from stage from to stage to, and reports whether it was at stage from.
func (p *Plan) advance(from, to stage) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stage != from {
		return false
	}
	p.stage = to
	return true
}

// endTasks records that the task changes have ended, and why the deletes are skipped: empty when they may run.
func (p *Plan) endTasks(skipDeletes string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stage, p.skipDeletes = tasksApplied, skipDeletes
}

// beginDeletes moves the plan to deletesBegun and returns why the deletes are skipped: empty when they may run. It
// fails before the task changes have ended and after the deletes have begun.
func (p *Plan) beginDeletes() (skip string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.stage < tasksApplied:
		return "", errors.New("apply the task changes of the plan before its deletes")
	case p.stage > tasksApplied:
		return "", errors.New("the deletes of the plan were applied already")
	}
	p.stage = deletesBegun
	return p.skipDeletes, nil
}

// skipCause returns why the deletes are skipped after the task jobs: causeEarlierFailed when one failed,
// causeCancelled when one was skipped otherwise, which only a cancel does, and empty when every one succeeded.
func skipCause(tasks []*job) string {
	switch {
	case anyIn(tasks, failed):
		return causeEarlierFailed
	case anyIn(tasks, skipped):
		return causeCancelled
	}
	return ""
}

// jobState is how far a job got.
type jobState int

const (
	pending jobState = iota
	running
	succeeded
	failed
	skipped
)

// job is one change that an applier carries out.
type job struct {
	PlannedChange
	do         func(ctx context.Context) error
	waitsFor   int   // the number of jobs that must succeed first
	dependents []int // the jobs that wait for this one
	state      jobState
	err        error // the last error of a failed job
}

// anyIn reports whether a job of jobs is in state s.
func anyIn(jobs []*job, s jobState) bool {
	return slices.ContainsFunc(jobs, func(j *job) bool { return j.state == s })
}

// event returns an event of type t about the job's change.
func (j *job) event(t EventType) Event {
	return Event{Type: t, Key: j.Key, ID: j.ID, Action: j.Action}
}

// label names the job's change in errors: its key, and the object's ID for a delete.
func (j *job) label() string {
	if j.Action == Delete {
		return fmt.Sprintf("%s (ID %s)", j.Key, j.ID)
	}
	return j.String() // the key's
}

// taskJobs returns a job for each task change other than Noop, in topological order. A job waits for the jobs of its
// dependencies, and a dependency without changes passes on the jobs that it waits for itself.
func (p *Plan) taskJobs() []*job {
	var jobs []*job
	passOn := make(map[Key][]int) // the jobs that each task's dependents wait for, ascending
	for _, t := range p.graph.order {
		k := t.Key()
		var waits []int
		for _, d := range p.graph.deps[k] {
			waits = append(waits, passOn[d]...)
		}
		slices.Sort(waits)
		waits = slices.Compact(waits)
		ch := p.planned[k]
		if ch.Action == Noop {
			passOn[k] = waits
			continue
		}
		ch.Diff = slices.Clone(ch.Diff)
		j := &job{
			PlannedChange: PlannedChange{Key: k, Change: ch},
			do:            func(ctx context.Context) error { return t.Apply(ctx, p.env, ch) },
			waitsFor:      len(waits),
		}
		for _, i := range waits {
			jobs[i].dependents = append(jobs[i].dependents, len(jobs))
		}
		passOn[k] = []int{len(jobs)}
		jobs = append(jobs, j)
	}
	return jobs
}

// deleteJobs returns a job for each pruned object, in apply order, grouped by kind.
func (p *Plan) deleteJobs() [][]*job {
	var groups [][]*job
	for i, o := range p.pruned {
		d := p.graph.kinds[o.Key.Kind].deleter
		j := &job{
			PlannedChange: deleteOf(o),
			do:            func(ctx context.Context) error { return d.Delete(ctx, p.env, o) },
		}
		if i == 0 || o.Key.Kind != p.pruned[i-1].Key.Kind {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], j)
	}
	return groups
}

// applier carries out the jobs of one call: Apply, ApplyTaskChanges or ApplyDeletes.
type applier struct {
	limit   int
	timeout time.Duration
	mu      sync.Mutex // held while onEvent runs
	onEvent func(Event)
}

// newApplier returns an applier with opts, their defaults filled in.
func newApplier(opts ApplyOptions) *applier {
	a := &applier{limit: opts.Parallelism, timeout: opts.ChangeTimeout, onEvent: opts.OnEvent}
	if a.limit <= 0 {
		a.limit = defaultParallelism
	}
	if a.timeout <= 0 {
		a.timeout = defaultChangeTimeout
	}
	return a
}

// runDeletes carries out the delete jobs kind by kind and returns them in order. When skip is set, it skips every job
// for that cause. A failed delete skips the jobs of the later kinds.
func (a *applier) runDeletes(ctx context.Context, kinds [][]*job, skip string) []*job {
	var jobs []*job
	for _, kind := range kinds {
		if skip != "" {
			a.skip(kind, skip)
		} else {
			a.run(ctx, kind)
			if anyIn(kind, failed) {
				skip = causeEarlierFailed
			}
		}
		jobs = append(jobs, kind...)
	}
	return jobs
}

// emit sends e to onEvent, one event at a time.
func (a *applier) emit(e Event) {
	if a.onEvent == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onEvent(e)
}

// skip marks the jobs that have not started as skipped, for cause.
func (a *applier) skip(jobs []*job, cause string) {
	for _, j := range jobs {
		if j.state == pending {
			j.state = skipped
			e := j.event(Skipped)
			e.Cause = cause
			a.emit(e)
		}
	}
}

// run carries out jobs, at most a.limit at once. A job is ready once the jobs it waits for have succeeded, and ready
// jobs start in order. A failed job skips the jobs that wait for it, directly or not. Once ctx ends, no job starts;
// when the running jobs have finished, the jobs that never started are skipped as cancelled. So a job that fails
// after the cancel still skips the jobs that wait for it with its own cause. run returns when every job it started
// has finished.
func (a *applier) run(ctx context.Context, jobs []*job) {
	type result struct {
		i   int
		err error
	}
	results := make(chan result)
	waiting := make([]int, len(jobs)) // the number of jobs that each job still waits for
	var ready []int                   // ascending
	for i, j := range jobs {
		waiting[i] = j.waitsFor
		if waiting[i] == 0 {
			ready = append(ready, i)
		}
	}
	busy := 0
	for {
		if ctx.Err() != nil {
			ready = nil
		}
		for ; len(ready) > 0 && busy < a.limit; busy++ {
			i := ready[0]
			ready = ready[1:]
			j := jobs[i]
			j.state = running
			go func() { results <- result{i, a.carryOut(ctx, j)} }()
		}
		if busy == 0 {
			if ctx.Err() != nil {
				a.skip(jobs, causeCancelled)
			}
			return
		}
		r := <-results
		busy--
		j := jobs[r.i]
		if r.err != nil {
			j.state, j.err = failed, r.err
			a.skip(waitingFor(jobs, r.i), fmt.Sprintf("%s failed", j.Key))
			continue
		}
		j.state = succeeded
		for _, d := range j.dependents {
			waiting[d]--
			if waiting[d] == 0 {
				at, _ := slices.BinarySearch(ready, d)
				ready = slices.Insert(ready, at, d)
			}
		}
	}
}

// waitingFor returns the jobs that wait for job i, directly or not, in order.
func waitingFor(jobs []*job, i int) []*job {
	found := make([]bool, len(jobs))
	next := slices.Clone(jobs[i].dependents)
	for len(next) > 0 {
		d := next[0]
		next = next[1:]
		if !found[d] {
			found[d] = true
			next = append(next, jobs[d].dependents...)
		}
	}
	var waiting []*job
	for d, f := range found {
		if f {
			waiting = append(waiting, jobs[d])
		}
	}
	return waiting
}

// carryOut runs the job's change within its own deadline and retries it while its error is retryable and the wait
// ends before the deadline. It returns the last error, or nil when the change succeeded.
func (a *applier) carryOut(ctx context.Context, j *job) error {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	a.emit(j.event(Started))
	for retry := 0; ; retry++ {
		err := j.do(ctx)
		if err == nil {
			a.emit(j.event(Succeeded))
			return nil
		}
		wait, ok := retryWait(ctx, err, retry, rand.N[time.Duration])
		if ok {
			e := j.event(Retrying)
			e.Err, e.Wait = err, wait
			a.emit(e)
			ok = sleep(ctx, wait) == nil
		}
		if !ok {
			e := j.event(Failed)
			e.Err = err
			a.emit(e)
			return err
		}
	}
}

// result joins the errors of the failed jobs in order, and ctx's error when ctx ended before every job succeeded.
func result(ctx context.Context, jobs []*job) error {
	var errs []error
	done := true
	for _, j := range jobs {
		switch j.state {
		case failed:
			errs = append(errs, fmt.Errorf("%s: %w", j.label(), j.err))
			done = false
		case skipped:
			done = false
		}
	}
	if err := ctx.Err(); err != nil && !done {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
