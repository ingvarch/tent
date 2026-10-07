package e2e

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/hcl"

	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

const (
	rejectLine = "2026-10-07T10:00:00.000Z [ERROR] nomad.client: node registration without introduction token: " +
		"enforcement_level=strict node_id=0b1c node_pool=default node_name=e2e-rogue"
	rogueLog = `    2026-10-07T10:00:01.000Z [ERROR] client: error registering: error="rpc error: Permission denied"`
)

func goodEvidence() introEvidence {
	return introEvidence{
		Journal:  []string{rejectLine},
		RogueLog: "line before\n" + rogueLog + "\nline after\n",
		Nodes:    []string{"e2e-aaaaaa-2404-servers-0", "e2e-aaaaaa-2404-workers-0"},
	}
}

func TestIntroProblemsPassWhenTheServerRejectedTheRogueTheRogueSawItAndNoNodeIsListed(t *testing.T) {
	if got := goodEvidence().problems(); len(got) != 0 {
		t.Errorf("problems = %q, want none", got)
	}
}

func TestIntroProblemsSayWhichPartIsMissing(t *testing.T) {
	cases := []struct {
		name   string
		change func(*introEvidence)
		want   string
	}{
		{"no journal line", func(e *introEvidence) { e.Journal = nil },
			"the server's journal has no line that rejects node e2e-rogue with enforcement_level=strict"},
		{"a line of another node", func(e *introEvidence) {
			e.Journal = []string{strings.Replace(rejectLine, "node_name=e2e-rogue", "node_name=other", 1)}
		}, "the server's journal has no line that rejects node e2e-rogue with enforcement_level=strict"},
		{"a node whose name only starts with the rogue's", func(e *introEvidence) {
			e.Journal = []string{rejectLine + "-2"}
		}, "the server's journal has no line that rejects node e2e-rogue with enforcement_level=strict"},
		{"enforcement warn", func(e *introEvidence) {
			e.Journal = []string{strings.Replace(rejectLine, "enforcement_level=strict", "enforcement_level=warn", 1)}
		}, "the server's journal has no line that rejects node e2e-rogue with enforcement_level=strict"},
		{"a level that only starts with strict", func(e *introEvidence) {
			e.Journal = []string{
				strings.Replace(rejectLine, "enforcement_level=strict", "enforcement_level=strictly", 1),
			}
		}, "the server's journal has no line that rejects node e2e-rogue with enforcement_level=strict"},
		{"a line about something else", func(e *introEvidence) {
			e.Journal = []string{"node registered: enforcement_level=strict node_name=e2e-rogue"}
		}, "the server's journal has no line that rejects node e2e-rogue with enforcement_level=strict"},
		{"the rogue saw no refusal", func(e *introEvidence) { e.RogueLog = "started\n" },
			`the rogue's log does not hold error registering: error="rpc error: Permission denied"`},
		{"the rogue saw another error", func(e *introEvidence) {
			e.RogueLog = `[ERROR] client: error registering: error="rpc error: no path to server"`
		}, `the rogue's log does not hold error registering: error="rpc error: Permission denied"`},
		{"the rogue is listed", func(e *introEvidence) { e.Nodes = append(e.Nodes, "e2e-rogue") },
			"node e2e-rogue is listed in /v1/nodes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := goodEvidence()
			c.change(&e)
			if got := e.problems(); len(got) != 1 || got[0] != c.want {
				t.Errorf("problems = %q, want [%q]", got, c.want)
			}
		})
	}
}

func TestIntroProblemsPassWhenAnotherNodeHasANameThatStartsWithTheRogues(t *testing.T) {
	e := goodEvidence()
	e.Nodes = append(e.Nodes, "e2e-rogue-2")
	if got := e.problems(); len(got) != 0 {
		t.Errorf("problems = %q, want none: the node is another one", got)
	}
}

func TestIntroProblemsPassWithOneGoodLineAmongOthers(t *testing.T) {
	e := goodEvidence()
	e.Journal = []string{
		strings.Replace(rejectLine, "node_name=e2e-rogue", "node_name=other", 1), rejectLine,
	}
	if got := e.problems(); len(got) != 0 {
		t.Errorf("problems = %q, want none", got)
	}
}

func TestIntroProblemsListEveryMissingPart(t *testing.T) {
	got := (introEvidence{Nodes: []string{"e2e-rogue"}}).problems()
	if len(got) != 3 {
		t.Errorf("problems = %q, want the three parts", got)
	}
}

func TestIntroRecordHoldsTheServerLinesTheRogueLogAndTheNodes(t *testing.T) {
	e := introEvidence{
		Journal: []string{"line one", "line two"}, RogueLog: "rogue says\nmore\n", Nodes: []string{"a", "b"},
	}
	want := "server journal lines:\nline one\nline two\n\nrogue log:\nrogue says\nmore\n\nnodes listed:\na\nb\n"
	if got := introRecord(e); got != want {
		t.Errorf("introRecord = %q, want %q", got, want)
	}
}

func TestIntroRecordSaysNoneForEmptyParts(t *testing.T) {
	want := "server journal lines:\n(none)\n\nrogue log:\n(empty)\n\nnodes listed:\n(none)\n"
	if got := introRecord(introEvidence{}); got != want {
		t.Errorf("introRecord = %q, want %q", got, want)
	}
}

func TestRogueConfigNamesTheAgentGivesItItsOwnPortAndDataDirAndOnlyTheExecDriver(t *testing.T) {
	var doc map[string]any
	if err := hcl.Decode(&doc, rogueConfig()); err != nil {
		t.Fatalf("decode the rogue config: %v", err)
	}
	if doc["name"] != "e2e-rogue" || doc["data_dir"] != "/var/lib/e2e-rogue" {
		t.Errorf("config = %v, want name e2e-rogue and data_dir /var/lib/e2e-rogue", doc)
	}
	if ports := block(t, doc, "ports"); ports["http"] != 5646 {
		t.Errorf("ports = %v, want http 5646", ports)
	}
	opts := block(t, doc, "client", "options")
	if opts["driver.allowlist"] != "exec" {
		t.Errorf("client options = %v, want driver.allowlist exec", opts)
	}
}

func TestRogueStartScriptStartsAFreshAgentInTheBackgroundAndRecordsItsPID(t *testing.T) {
	script := rogueStartScript()
	for _, want := range []string{
		"set -e\n",
		"cat >/tmp/e2e-rogue.hcl <<'EOF'\n" + rogueConfig() + "EOF\n",
		"rm -rf /var/lib/e2e-rogue\n",
		"nohup timeout 120 /usr/local/bin/nomad agent -config /etc/nomad.d -config /tmp/e2e-rogue.hcl " +
			">/tmp/e2e-rogue.log 2>&1 </dev/null &\n",
		"echo $! >/tmp/e2e-rogue.pid\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("start script = %q, want it to hold %q", script, want)
		}
	}
	if strings.Index(script, "rm -rf") > strings.Index(script, "nohup") {
		t.Errorf("start script = %q, want the data directory removed before the agent starts", script)
	}
}

func TestRogueStopScriptKillsTheAgentByItsPIDAndRemovesEveryFile(t *testing.T) {
	script := rogueStopScript()
	for _, want := range []string{
		`pid=$(cat /tmp/e2e-rogue.pid)`,
		`kill "$pid" 2>/dev/null || true`,
		`while kill -0 "$pid" 2>/dev/null`,
		"rm -rf /var/lib/e2e-rogue /tmp/e2e-rogue.hcl /tmp/e2e-rogue.log /tmp/e2e-rogue.pid\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("stop script = %q, want it to hold %q", script, want)
		}
	}
	if strings.Contains(script, "pkill") || strings.Contains(script, "killall") {
		t.Errorf("stop script = %q, kills by name", script)
	}
	if strings.Index(script, "kill ") > strings.Index(script, "rm -rf") {
		t.Errorf("stop script = %q, want the agent stopped before its files go", script)
	}
}

func TestJournalScriptReadsTheNomadUnitSinceTheEpochAndKeepsTheRejectionLines(t *testing.T) {
	want := "journalctl -u nomad --since @1760000000 --no-pager -o cat | " +
		"grep -F 'node registration without introduction token' || true\n"
	if got := journalScript(1760000000); got != want {
		t.Errorf("journalScript = %q, want %q", got, want)
	}
}

func TestRogueLogScriptPrintsTheLogAndSucceedsWhenItIsNotThereYet(t *testing.T) {
	want := "cat /tmp/e2e-rogue.log 2>/dev/null || true\n"
	if got := rogueLogScript(); got != want {
		t.Errorf("rogueLogScript = %q, want %q", got, want)
	}
}

func TestParseEpoch(t *testing.T) {
	if got, err := parseEpoch("1760000000\n"); err != nil || got != 1760000000 {
		t.Errorf("parseEpoch = %d, %v, want 1760000000", got, err)
	}
	for _, in := range []string{"", "abc", "12 34", "-5", "0"} {
		if got, err := parseEpoch(in); err == nil {
			t.Errorf("parseEpoch(%q) = %d, want an error", in, got)
		}
	}
}

func TestNonEmptyLinesDropsBlankLinesAndTrimsTheRest(t *testing.T) {
	want := []string{"a b", "c"}
	if got := nonEmptyLines("\n a b \r\n\n c\n"); !slices.Equal(got, want) {
		t.Errorf("nonEmptyLines = %q, want %q", got, want)
	}
	if got := nonEmptyLines(""); len(got) != 0 {
		t.Errorf("nonEmptyLines(\"\") = %q, want none", got)
	}
}

func TestListNodeNamesReturnsTheNameOfEachNode(t *testing.T) {
	f, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/nodes": fixed(200, `[{"ID":"1","Name":"a"},{"ID":"2","Name":"b"}]`),
	})
	got, err := listNodeNames(t.Context(), n)
	if err != nil || !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("listNodeNames = %q, %v, want a and b (requests %q)", got, err, f.requests)
	}
}

func TestListNodeNamesReturnsTheErrorOfTheCall(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){"GET /v1/nodes": fixed(500, "down")})
	if _, err := listNodeNames(t.Context(), n); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("listNodeNames error = %v, want the 500", err)
	}
}

func instance(name, role, ip, cluster string) vultrapi.Instance {
	return vultrapi.Instance{Label: name, MainIP: ip, Tags: []string{"tent/cluster=" + cluster, "tent/role=" + role}}
}

func TestOnlyMachineFindsTheOneMachineOfTheRoleInTheCluster(t *testing.T) {
	all := []vultrapi.Instance{
		instance("s", "server", "192.0.2.1", "e2e-a"),
		instance("c", "client", "192.0.2.2", "e2e-a"),
		instance("c2", "client", "192.0.2.3", "e2e-b"),
	}
	got, err := onlyMachine(all, "e2e-a", "client")
	if err != nil || got.MainIP != "192.0.2.2" {
		t.Errorf("onlyMachine = %+v, %v, want the client of e2e-a", got, err)
	}
}

func TestOnlyMachineFailsForNoneOrManyOrNoAddress(t *testing.T) {
	cases := []struct {
		name string
		in   []vultrapi.Instance
		want string
	}{
		{"none", nil, "found 0 machines with the role client in e2e-a, want 1"},
		{"another role", []vultrapi.Instance{instance("s", "server", "192.0.2.1", "e2e-a")},
			"found 0 machines with the role client in e2e-a, want 1"},
		{"two", []vultrapi.Instance{
			instance("c1", "client", "192.0.2.1", "e2e-a"), instance("c2", "client", "192.0.2.2", "e2e-a"),
		}, "found 2 machines with the role client in e2e-a, want 1"},
		{"no address", []vultrapi.Instance{instance("c1", "client", "", "e2e-a")},
			"the client machine c1 of e2e-a has no main address"},
		{"the placeholder address", []vultrapi.Instance{instance("c1", "client", "0.0.0.0", "e2e-a")},
			"the client machine c1 of e2e-a has no main address"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := onlyMachine(c.in, "e2e-a", "client"); err == nil || err.Error() != c.want {
				t.Errorf("onlyMachine error = %v, want %q", err, c.want)
			}
		})
	}
}

// introReads returns a reader that answers from a list, one answer for each poll, and the last one again after
// that. It counts its calls.
type introReads struct {
	mu      sync.Mutex
	answers []introAnswer
	calls   int
}

type introAnswer struct {
	ev  introEvidence
	err error
}

func (r *introReads) read(context.Context) (introEvidence, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.answers[min(r.calls, len(r.answers)-1)]
	r.calls++
	return a.ev, a.err
}

func TestWaitIntroRejectedReturnsAtOnceWhenTheFirstPollPasses(t *testing.T) {
	r := &introReads{answers: []introAnswer{{ev: goodEvidence()}}}
	ev, err := waitIntroRejected(t.Context(), r.read, time.Millisecond, 5*time.Second)
	if err != nil || r.calls != 1 || len(ev.Journal) != 1 {
		t.Errorf("waitIntroRejected = %+v, %v after %d polls, want a pass after 1", ev, err, r.calls)
	}
}

func TestWaitIntroRejectedPollsUntilTheJournalHoldsTheLine(t *testing.T) {
	noLine := goodEvidence()
	noLine.Journal = nil
	r := &introReads{answers: []introAnswer{{ev: noLine}, {ev: noLine}, {ev: goodEvidence()}}}
	_, err := waitIntroRejected(t.Context(), r.read, time.Millisecond, time.Minute)
	if err != nil || r.calls != 3 {
		t.Errorf("waitIntroRejected = %v after %d polls, want a pass after 3", err, r.calls)
	}
}

func TestWaitIntroRejectedPassesOnlyOnAPollWhereEveryReadSucceeded(t *testing.T) {
	r := &introReads{answers: []introAnswer{
		{ev: goodEvidence(), err: errors.New("ssh: connection reset")}, {ev: goodEvidence()},
	}}
	_, err := waitIntroRejected(t.Context(), r.read, time.Millisecond, time.Minute)
	if err != nil || r.calls != 2 {
		t.Errorf("waitIntroRejected = %v after %d polls, want a pass after 2: the first poll had a read error",
			err, r.calls)
	}
}

func TestWaitIntroRejectedFailsAtTheTimeoutWithTheLastReadError(t *testing.T) {
	r := &introReads{answers: []introAnswer{{ev: goodEvidence(), err: errors.New("ssh: connection reset")}}}
	_, err := waitIntroRejected(t.Context(), r.read, time.Millisecond, 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "ssh: connection reset") {
		t.Errorf("waitIntroRejected error = %v, want the read error", err)
	}
}

func TestWaitIntroRejectedFailsAtTheTimeoutNamingWhatIsMissing(t *testing.T) {
	noLog := goodEvidence()
	noLog.RogueLog = ""
	r := &introReads{answers: []introAnswer{{ev: noLog}}}
	ev, err := waitIntroRejected(t.Context(), r.read, time.Millisecond, 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "the rogue's log does not hold") {
		t.Errorf("waitIntroRejected error = %v, want the missing log line", err)
	}
	if len(ev.Journal) != 1 {
		t.Errorf("evidence = %+v, want the last poll's evidence", ev)
	}
}

func TestWaitIntroRejectedStopsAtOnceWhenTheRogueIsListed(t *testing.T) {
	listed := introEvidence{Nodes: []string{"e2e-rogue"}}
	r := &introReads{answers: []introAnswer{{ev: listed}, {ev: goodEvidence()}}}
	ev, err := waitIntroRejected(t.Context(), r.read, time.Millisecond, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "node e2e-rogue is listed in /v1/nodes") || r.calls != 1 {
		t.Errorf("waitIntroRejected = %v after %d polls, want the listing to fail at once", err, r.calls)
	}
	if !slices.Contains(ev.Nodes, "e2e-rogue") {
		t.Errorf("evidence = %+v, want the listed node kept", ev)
	}
}

func TestWaitIntroRejectedRemembersARogueThatWasListedInAnEarlierPoll(t *testing.T) {
	listedFirst := goodEvidence()
	listedFirst.Nodes = []string{"e2e-rogue"}
	r := &introReads{answers: []introAnswer{{ev: listedFirst, err: errors.New("ssh: reset")}, {ev: goodEvidence()}}}
	_, err := waitIntroRejected(t.Context(), r.read, time.Millisecond, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "node e2e-rogue is listed in /v1/nodes") {
		t.Errorf("waitIntroRejected error = %v, want the rogue's listing to fail the step", err)
	}
}

func TestWaitIntroRejectedKeepsEveryNodeNameSeen(t *testing.T) {
	first := goodEvidence()
	first.Nodes = []string{"a", "b"}
	second := goodEvidence()
	second.Nodes = []string{"b", "c"}
	r := &introReads{answers: []introAnswer{{ev: first, err: errors.New("later read failed")}, {ev: second}}}
	ev, err := waitIntroRejected(t.Context(), r.read, time.Millisecond, time.Minute)
	if err != nil || !slices.Equal(ev.Nodes, []string{"a", "b", "c"}) {
		t.Errorf("waitIntroRejected = %+v, %v, want the nodes a, b and c once each", ev, err)
	}
}
