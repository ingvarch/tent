package e2e

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The outcomes of a poll. The leader outcome is led, leaderless or unreached; the service outcome is served,
// unserved or unreached.
const (
	probeLed        = "led"
	probeLeaderless = "leaderless"
	probeUnreached  = "unreached"
	probeServed     = "served"
	probeUnserved   = "unserved"
)

const (
	probeEvery          = time.Second
	probeRefresh        = 10 * time.Second
	probeRefreshTimeout = 5 * time.Second
	// probeLeaderTimeout bounds a stale leader read, which a server answers from its own view at once.
	probeLeaderTimeout = 3 * time.Second
	// probeServiceTimeout is above Nomad's 5 s hold of a forwarded read.
	probeServiceTimeout = 7 * time.Second

	// noLeaderAnswer ends the error of the read of a server that has no leader to forward to.
	noLeaderAnswer = ": 500: No cluster leader"

	probeHeader = "start\ttook_ms\tleader\tleader_address\tservice\tdetail\n"
)

// probeLimits are what a run may show before probeVerdict fails it.
type probeLimits struct {
	// Leaderless is the longest time without a leader: a transfer takes about 1 s, a lost quorum lasts until a
	// person acts.
	Leaderless time.Duration
	// Unserved is the longest time without a healthy allocation: two check intervals of the job.
	Unserved time.Duration
	// Unreached is the longest time that a read can fail, for the leader and for the service each: a client whose
	// server was stopped fails its checks reads for 5 to 15 s.
	Unreached time.Duration
	// Gap is the longest time between the starts of two polls; a longer one means the probe saw nothing.
	Gap time.Duration
	// MinPolls is the fewest polls that make a run worth judging.
	MinPolls int
}

// defaultProbeLimits are the limits of the scenarios.
var defaultProbeLimits = probeLimits{
	Leaderless: 10 * time.Second,
	Unserved:   10 * time.Second,
	Unreached:  30 * time.Second,
	Gap:        30 * time.Second,
	MinPolls:   10,
}

// probeRecord is the result of one poll.
type probeRecord struct {
	Start time.Time
	Took  time.Duration
	// Leader is led, leaderless or unreached; LeaderAddr is the leader that a server named, if any.
	Leader     string
	LeaderAddr string
	// Service is served, unserved or unreached.
	Service string
	// Detail holds the errors of the reads, and the answers about the service when it is not served.
	Detail string
}

// probeLine returns the record as a line of the records file; tabs and line breaks of the detail become spaces.
func probeLine(r probeRecord) string {
	return probeRow(r.Start, r.Took, r.Leader, r.LeaderAddr, r.Service, r.Detail)
}

func probeRow(start time.Time, took time.Duration, leader, leaderAddr, service, detail string) string {
	detail = strings.NewReplacer("\t", " ", "\r\n", " ", "\n", " ", "\r", " ").Replace(detail)
	return fmt.Sprintf("%s\t%d\t%s\t%s\t%s\t%s\n", start.UTC().Format("2006-01-02T15:04:05.000Z"),
		took.Milliseconds(), leader, leaderAddr, service, detail)
}

// probeConfig says what a probe asks and how often.
type probeConfig struct {
	// Nomad holds the token and the HTTP client; the probe asks each server through Nomad.at.
	Nomad *nomadAPI
	// Addresses returns the current server addresses, each as "https://<ip>:4646". It must end when its context
	// ends.
	Addresses func(ctx context.Context) ([]string, error)
	Job       string
	Service   string
	// Every is the time between the starts of two polls, Refresh the time between two calls of Addresses.
	Every, Refresh time.Duration
	// RefreshTimeout bounds a call of Addresses, LeaderTimeout a leader read, ServiceTimeout a read of the service.
	RefreshTimeout, LeaderTimeout, ServiceTimeout time.Duration
	// Out gets the header and then each record as it comes.
	Out io.Writer
}

// newProbeConfig returns the configuration of the scenarios for the job and its service.
func newProbeConfig(n *nomadAPI, addresses func(context.Context) ([]string, error), job, service string,
	out io.Writer) probeConfig {
	return probeConfig{
		Nomad:          n,
		Addresses:      addresses,
		Job:            job,
		Service:        service,
		Every:          probeEvery,
		Refresh:        probeRefresh,
		RefreshTimeout: probeRefreshTimeout,
		LeaderTimeout:  probeLeaderTimeout,
		ServiceTimeout: probeServiceTimeout,
		Out:            out,
	}
}

// probe polls the servers of a cluster for a leader and for a healthy allocation of a service until it is stopped.
type probe struct {
	cfg    probeConfig
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// ready closes when the first call of Addresses has ended.
	ready chan struct{}

	outMu sync.Mutex

	mu        sync.Mutex
	addresses []string

	// records is written by the polling goroutine and read after it has ended.
	records []probeRecord
}

// startProbe starts a probe and writes the header. The first poll waits for the first list of addresses; a slow list
// after that never holds up a poll.
func startProbe(ctx context.Context, cfg probeConfig) *probe {
	ctx, cancel := context.WithCancel(ctx)
	p := &probe{cfg: cfg, cancel: cancel, ready: make(chan struct{})}
	p.write(probeHeader)
	p.wg.Go(func() { p.refreshLoop(ctx) })
	p.wg.Go(func() { p.pollLoop(ctx) })
	return p
}

// stop ends the probe and returns the records of the polls that ran to the end; a poll that stop interrupted is
// dropped, since its reads failed because of the stop.
func (p *probe) stop() []probeRecord {
	p.cancel()
	p.wg.Wait()
	return p.records
}

// write sends text to the records file. A lost line costs nothing: stop returns the records as well.
func (p *probe) write(text string) {
	p.outMu.Lock()
	defer p.outMu.Unlock()
	_, _ = io.WriteString(p.cfg.Out, text)
}

func (p *probe) refreshLoop(ctx context.Context) {
	p.refresh(ctx)
	close(p.ready)
	ticker := time.NewTicker(p.cfg.Refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refresh(ctx)
		}
	}
}

// refresh replaces the addresses with a new list. A failed list keeps the old addresses and is written as a line.
func (p *probe) refresh(ctx context.Context) {
	start := time.Now()
	listCtx, cancel := context.WithTimeout(ctx, p.cfg.RefreshTimeout)
	defer cancel()
	addresses, err := p.cfg.Addresses(listCtx)
	if err != nil {
		if ctx.Err() == nil {
			p.write(probeRow(start, time.Since(start), "-", "-", "-", "the list of servers failed: "+err.Error()))
		}
		return
	}
	p.mu.Lock()
	p.addresses = addresses
	p.mu.Unlock()
}

func (p *probe) pollLoop(ctx context.Context) {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return
	}
	ticker := time.NewTicker(p.cfg.Every)
	defer ticker.Stop()
	for {
		record := p.poll(ctx)
		if ctx.Err() != nil {
			return
		}
		p.records = append(p.records, record)
		p.write(probeLine(record))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// leaderAnswer is what one server said to the leader read.
type leaderAnswer struct {
	address string
	// answered is false when the read failed.
	answered bool
	leader   string
	err      error
}

// poll asks every known server for the leader at once and then reads the service through one of them.
func (p *probe) poll(ctx context.Context) probeRecord {
	start := time.Now()
	p.mu.Lock()
	addresses := p.addresses
	p.mu.Unlock()
	record := probeRecord{Start: start}
	if len(addresses) == 0 {
		record.Leader, record.Service, record.Detail = probeUnreached, probeUnreached, "no server address is known"
		record.Took = time.Since(start)
		return record
	}
	answers := make([]leaderAnswer, len(addresses))
	var wg sync.WaitGroup
	for i, address := range addresses {
		wg.Go(func() { answers[i] = p.readLeader(ctx, address) })
	}
	wg.Wait()

	var details []string
	via := ""
	record.Leader = probeUnreached
	for _, a := range answers {
		switch {
		case !a.answered:
			details = append(details, a.address+": "+a.err.Error())
		case a.leader != "" && record.Leader != probeLed:
			record.Leader, record.LeaderAddr, via = probeLed, a.leader, a.address
		case record.Leader == probeUnreached:
			record.Leader = probeLeaderless
			via = a.address
		}
	}
	if via == "" {
		record.Service = probeUnreached
	} else {
		var detail string
		record.Service, detail = p.readService(ctx, p.cfg.Nomad.at(via))
		if detail != "" {
			details = append(details, detail)
		}
	}
	record.Detail = strings.Join(details, "; ")
	record.Took = time.Since(start)
	return record
}

// readLeader asks one server for its leader from its own view of the Raft cluster. A server without a leader
// answers either an empty name or a 500, which both count as an answer.
func (p *probe) readLeader(ctx context.Context, address string) leaderAnswer {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.LeaderTimeout)
	defer cancel()
	var leader string
	err := p.cfg.Nomad.at(address).get(ctx, "/v1/status/leader?stale=true", &leader)
	switch {
	case err == nil:
		return leaderAnswer{address: address, answered: true, leader: leader}
	case strings.Contains(err.Error(), noLeaderAnswer):
		return leaderAnswer{address: address, answered: true}
	default:
		return leaderAnswer{address: address, err: err}
	}
}

// readService reads the allocations of the job, the checks of each running one and the registrations of the
// service, each read under its own timeout, and returns the outcome with what to write about it.
func (p *probe) readService(ctx context.Context, n *nomadAPI) (outcome, detail string) {
	var a serviceAnswers
	if err := p.within(ctx, func(ctx context.Context) error {
		return n.get(ctx, "/v1/job/"+url.PathEscape(p.cfg.Job)+"/allocations", &a.Allocations)
	}); err != nil {
		return probeUnreached, err.Error()
	}
	var running []string
	for _, alloc := range a.Allocations {
		if alloc.ClientStatus == "running" {
			running = append(running, alloc.ID)
		}
	}
	checks := make([]map[string]allocationCheck, len(running))
	checkErrs := make([]error, len(running))
	var registrationErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		registrationErr = p.within(ctx, func(ctx context.Context) error {
			return n.get(ctx, "/v1/service/"+url.PathEscape(p.cfg.Service), &a.Registrations)
		})
	})
	for i, id := range running {
		wg.Go(func() {
			checkErrs[i] = p.within(ctx, func(ctx context.Context) error {
				return n.get(ctx, "/v1/allocation/"+url.PathEscape(id)+"/checks", &checks[i])
			})
		})
	}
	wg.Wait()

	var errs []string
	if registrationErr != nil {
		errs = append(errs, registrationErr.Error())
	}
	a.Checks = map[string]map[string]allocationCheck{}
	failed := 0
	for i, id := range running {
		if checkErrs[i] != nil {
			failed++
			errs = append(errs, checkErrs[i].Error())
			continue
		}
		a.Checks[id] = checks[i]
	}
	switch {
	case registrationErr != nil:
		return probeUnreached, strings.Join(errs, "; ")
	case a.missing(p.cfg.Service) == nil:
		return probeServed, strings.Join(errs, "; ")
	case len(running) > 0 && failed == len(running):
		return probeUnreached, strings.Join(errs, "; ")
	default:
		return probeUnserved, strings.Join(append(errs, a.String()), "; ")
	}
}

// within runs read under the timeout of a service read.
func (p *probe) within(ctx context.Context, read func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ServiceTimeout)
	defer cancel()
	return read(ctx)
}

// probeKind is a kind of stretch: a run of polls with the same outcome.
type probeKind struct {
	name string
	// limit is the longest time the kind may last; it counts only when checked is set.
	limit   time.Duration
	checked bool
}

func (l probeLimits) leaderKinds() map[string]probeKind {
	return map[string]probeKind{
		probeLed:        {name: "led"},
		probeLeaderless: {name: "leaderless", limit: l.Leaderless, checked: true},
		probeUnreached:  {name: "leader unreached", limit: l.Unreached, checked: true},
	}
}

func (l probeLimits) serviceKinds() map[string]probeKind {
	return map[string]probeKind{
		probeServed:    {name: "served"},
		probeUnserved:  {name: "unserved", limit: l.Unserved, checked: true},
		probeUnreached: {name: "service unreached", limit: l.Unreached, checked: true},
	}
}

// probeSummary counts the polls of each kind and names the longest stretch of each kind and the longest gap.
type probeSummary struct {
	counts  map[string]int
	longest map[string]time.Duration
	gap     time.Duration
}

func (s probeSummary) String() string {
	var counts, longest []string
	for _, kind := range []string{"led", "leaderless", "leader unreached", "served", "unserved", "service unreached"} {
		counts = append(counts, fmt.Sprintf("%s %d", kind, s.counts[kind]))
	}
	for _, kind := range []string{"leaderless", "leader unreached", "unserved", "service unreached"} {
		longest = append(longest, fmt.Sprintf("%s %s", kind, s.longest[kind]))
	}
	return fmt.Sprintf("polls: %s; longest stretch: %s; longest gap %s", strings.Join(counts, ", "),
		strings.Join(longest, ", "), s.gap)
}

// probeVerdict judges the records against the limits. A stretch lasts from the first poll of an outcome to the start
// of the next poll with another outcome, or to the last poll. The error names every breach; the summary is returned
// in both cases.
func probeVerdict(records []probeRecord, limits probeLimits) (probeSummary, error) {
	s := probeSummary{counts: map[string]int{}, longest: map[string]time.Duration{}}
	var breaches []string
	if len(records) < limits.MinPolls {
		breaches = append(breaches, fmt.Sprintf("%d polls, at least %d are needed", len(records), limits.MinPolls))
	}
	if len(records) > 0 {
		first, last := records[0], records[len(records)-1]
		if first.Leader != probeLed || first.Service != probeServed {
			breaches = append(breaches, fmt.Sprintf("the first poll at %s was %s and %s, want led and served",
				clock(first.Start), first.Leader, first.Service))
		}
		if last.Leader != probeLed || last.Service != probeServed {
			breaches = append(breaches, fmt.Sprintf("the last poll at %s was %s and %s, want led and served",
				clock(last.Start), last.Leader, last.Service))
		}
	}
	for i := 1; i < len(records); i++ {
		gap := records[i].Start.Sub(records[i-1].Start)
		s.gap = max(s.gap, gap)
		if gap > limits.Gap {
			breaches = append(breaches, fmt.Sprintf("no poll started for %s after %s, want at most %s", gap,
				clock(records[i-1].Start), limits.Gap))
		}
	}
	breaches = append(breaches, s.addStretches(records, func(r probeRecord) string { return r.Leader },
		limits.leaderKinds())...)
	breaches = append(breaches, s.addStretches(records, func(r probeRecord) string { return r.Service },
		limits.serviceKinds())...)
	if len(breaches) > 0 {
		return s, fmt.Errorf("the probe failed: %s", strings.Join(breaches, "; "))
	}
	return s, nil
}

// addStretches walks the runs of equal outcomes, adds them to the counts and the longest stretches and returns a
// breach for each run that is longer than its kind allows.
func (s *probeSummary) addStretches(records []probeRecord, outcome func(probeRecord) string,
	kinds map[string]probeKind) []string {
	var breaches []string
	for i := 0; i < len(records); {
		j := i
		for j < len(records) && outcome(records[j]) == outcome(records[i]) {
			j++
		}
		kind := kinds[outcome(records[i])]
		end := records[len(records)-1].Start
		if j < len(records) {
			end = records[j].Start
		}
		length := end.Sub(records[i].Start)
		s.counts[kind.name] += j - i
		s.longest[kind.name] = max(s.longest[kind.name], length)
		if kind.checked && length > kind.limit {
			breaches = append(breaches, fmt.Sprintf("%s for %s from %s, want at most %s", kind.name, length,
				clock(records[i].Start), kind.limit))
		}
		i = j
	}
	return breaches
}

func clock(t time.Time) string {
	return t.UTC().Format("15:04:05.000")
}
