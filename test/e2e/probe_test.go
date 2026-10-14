package e2e

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// The outcomes of a poll. The leader outcome is led, leaderless or unreached; the service outcome is served,
// unserved, unchecked or unreached.
const (
	probeLed        = "led"
	probeLeaderless = "leaderless"
	probeUnreached  = "unreached"
	probeUnchecked  = "unchecked"
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

	// probeTotalFactor times a bad outcome's stretch limit is how long its stretches may last together in one roll.
	probeTotalFactor = 2

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
	// Unreached is the longest time that no server answers the leader read, or that the allocations or the
	// registrations read fails.
	Unreached time.Duration
	// Unchecked is the longest time that every checks read can fail while the allocations and the registrations
	// answer: a client keeps its connection to a stopped server until its keepalive fails and its next heartbeat
	// reaches another one, which took 37 s on a real run.
	Unchecked time.Duration
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
	Unchecked:  60 * time.Second,
	Gap:        30 * time.Second,
	MinPolls:   10,
}

// workerProbeLimits are the limits of a roll that stops no server. Every checks read can fail only when no client
// that runs an allocation answers while Nomad still lists them, which is an outage, so a stretch of unchecked polls
// may last as long as one of unserved polls.
var workerProbeLimits = func() probeLimits {
	l := defaultProbeLimits
	l.Unchecked = l.Unserved
	return l
}()

// longest returns the longest of the stretch limits.
func (l probeLimits) longest() time.Duration {
	return max(l.Leaderless, l.Unserved, l.Unreached, l.Unchecked)
}

// probeRecord is the result of one poll.
type probeRecord struct {
	Start time.Time
	Took  time.Duration
	// Leader is led, leaderless or unreached; LeaderAddr is the leader that a server named, if any.
	Leader     string
	LeaderAddr string
	// Service is served, unserved, unchecked or unreached.
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
	// First is the list of addresses of the first poll; Addresses replaces it every Refresh.
	First   []string
	Job     string
	Service string
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
	outMu  sync.Mutex

	// mu guards addresses and records.
	mu        sync.Mutex
	addresses []string
	records   []probeRecord
	// recorded gets a value after each record, for settle.
	recorded chan struct{}
}

// startProbe starts a probe and writes the header. The first poll uses cfg.First; a slow list of addresses never
// holds up a poll.
func startProbe(ctx context.Context, cfg probeConfig) *probe {
	ctx, cancel := context.WithCancel(ctx)
	p := &probe{cfg: cfg, cancel: cancel, addresses: cfg.First, recorded: make(chan struct{}, 1)}
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
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.records
}

// settle waits until the latest poll is led and served, for at most limit and until ctx ends, and returns how long it
// waited. The probe goes on polling meanwhile.
func (p *probe) settle(ctx context.Context, limit time.Duration) time.Duration {
	start := time.Now()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for !p.lastIsGood() {
		select {
		case <-p.recorded:
		case <-timer.C:
			return time.Since(start)
		case <-ctx.Done():
			return time.Since(start)
		}
	}
	return time.Since(start)
}

func (p *probe) lastIsGood() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.records) == 0 {
		return false
	}
	last := p.records[len(p.records)-1]
	return last.Leader == probeLed && last.Service == probeServed
}

// addRecord keeps a record, tells settle about it and writes it to the records file.
func (p *probe) addRecord(r probeRecord) {
	p.mu.Lock()
	p.records = append(p.records, r)
	p.mu.Unlock()
	select {
	case p.recorded <- struct{}{}:
	default:
	}
	p.write(probeLine(r))
}

// write sends text to the records file. A lost line costs nothing: stop returns the records as well.
func (p *probe) write(text string) {
	p.outMu.Lock()
	defer p.outMu.Unlock()
	_, _ = io.WriteString(p.cfg.Out, text)
}

func (p *probe) refreshLoop(ctx context.Context) {
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
	ticker := time.NewTicker(p.cfg.Every)
	defer ticker.Stop()
	for {
		record := p.poll(ctx)
		if ctx.Err() != nil {
			return
		}
		p.addRecord(record)
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
	r := readServiceReads(ctx, n, p.cfg.Job, p.cfg.Service, p.cfg.ServiceTimeout)
	if r.allocationsErr != nil {
		return probeUnreached, r.allocationsErr.Error()
	}
	var errs []string
	if r.registrationsErr != nil {
		errs = append(errs, r.registrationsErr.Error())
	}
	var running []string
	for _, alloc := range r.answers.Allocations {
		if alloc.ClientStatus != "running" {
			continue
		}
		running = append(running, alloc.ID)
		if err := r.checkErrs[alloc.ID]; err != nil {
			errs = append(errs, err.Error())
		}
	}
	switch {
	case r.registrationsErr != nil:
		return probeUnreached, strings.Join(errs, "; ")
	case r.answers.missing(p.cfg.Service) == nil:
		return probeServed, strings.Join(errs, "; ")
	case len(running) > 0 && len(r.checkErrs) == len(running) &&
		slices.ContainsFunc(running, r.answers.hasRegistration):
		return probeUnchecked, strings.Join(errs, "; ")
	default:
		return probeUnserved, strings.Join(append(errs, r.answers.String()), "; ")
	}
}

// probeKind is a kind of stretch: a run of polls with the same outcome.
type probeKind struct {
	name string
	// limit is the longest time one stretch of the kind may last and total the longest time that all its stretches
	// may last together; they count only when checked is set.
	limit, total time.Duration
	checked      bool
}

// bad returns the kind of a bad outcome with its limits.
func bad(name string, limit time.Duration) probeKind {
	return probeKind{name: name, limit: limit, total: limit * probeTotalFactor, checked: true}
}

func (l probeLimits) leaderKinds() map[string]probeKind {
	return map[string]probeKind{
		probeLed:        {name: "led"},
		probeLeaderless: bad("leaderless", l.Leaderless),
		probeUnreached:  bad("leader unreached", l.Unreached),
	}
}

func (l probeLimits) serviceKinds() map[string]probeKind {
	return map[string]probeKind{
		probeServed:    {name: "served"},
		probeUnserved:  bad("unserved", l.Unserved),
		probeUnreached: bad("service unreached", l.Unreached),
		probeUnchecked: bad("service unchecked", l.Unchecked),
	}
}

// probeSummary counts the polls of each kind and names the longest stretch and the total time of each kind and the
// longest gap.
type probeSummary struct {
	counts  map[string]int
	longest map[string]time.Duration
	totals  map[string]time.Duration
	gap     time.Duration
}

func (s probeSummary) String() string {
	var counts, longest, totals []string
	counted := []string{"led", "leaderless", "leader unreached", "served", "unserved", "service unreached",
		"service unchecked"}
	stretched := []string{"leaderless", "leader unreached", "unserved", "service unreached", "service unchecked"}
	for _, kind := range counted {
		counts = append(counts, fmt.Sprintf("%s %d", kind, s.counts[kind]))
	}
	for _, kind := range stretched {
		longest = append(longest, fmt.Sprintf("%s %s", kind, s.longest[kind]))
		totals = append(totals, fmt.Sprintf("%s %s", kind, s.totals[kind]))
	}
	return fmt.Sprintf("polls: %s; longest stretch: %s; longest gap %s; total: %s", strings.Join(counts, ", "),
		strings.Join(longest, ", "), s.gap, strings.Join(totals, ", "))
}

// probeVerdict judges the records against the limits. A stretch lasts from the first poll of an outcome to the start
// of the next poll with another outcome, or to the last poll. A bad outcome also fails when its stretches add up to
// more than its total limit. The error names every breach; the summary is returned in both cases.
func probeVerdict(records []probeRecord, limits probeLimits) (probeSummary, error) {
	s := probeSummary{counts: map[string]int{}, longest: map[string]time.Duration{}, totals: map[string]time.Duration{}}
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

// addStretches walks the runs of equal outcomes, adds them to the counts, the longest stretches and the totals, and
// returns a breach for each run that is longer than its kind allows and for each kind whose runs add up to more than
// its total limit.
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
		s.totals[kind.name] += length
		if kind.checked && length > kind.limit {
			breaches = append(breaches, fmt.Sprintf("%s for %s from %s, want at most %s", kind.name, length,
				clock(records[i].Start), kind.limit))
		}
		i = j
	}
	for _, outcome := range slices.Sorted(maps.Keys(kinds)) {
		if kind := kinds[outcome]; kind.checked && s.totals[kind.name] > kind.total {
			breaches = append(breaches, fmt.Sprintf("%s for %s in all, want at most %s", kind.name,
				s.totals[kind.name], kind.total))
		}
	}
	return breaches
}

func clock(t time.Time) string {
	return t.UTC().Format("15:04:05.000")
}
