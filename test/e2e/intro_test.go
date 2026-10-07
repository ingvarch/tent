package e2e

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

const (
	// rogueName is the name of the second Nomad agent that starts on a client machine without an intro token.
	rogueName       = "e2e-rogue"
	rogueDataDir    = "/var/lib/e2e-rogue"
	rogueConfigPath = "/tmp/e2e-rogue.hcl"
	rogueLogPath    = "/tmp/e2e-rogue.log"
	roguePIDPath    = "/tmp/e2e-rogue.pid"
	// rogueHTTPPort differs from the port 4646 of the machine's own agent.
	rogueHTTPPort = 5646
	// rogueLifetime is in seconds: the agent ends by itself when nothing stops it.
	rogueLifetime = 120
	// rogueStopWait is how many seconds the stop script waits for the agent to end.
	rogueStopWait = 15

	// introRejectPhrase is in the line a server logs for a node that registers without an intro token.
	introRejectPhrase = "node registration without introduction token"
	// rogueRefusedText is in the log of an agent that the servers refuse.
	rogueRefusedText = `error registering: error="rpc error: Permission denied"`
)

// introEvidence is what the intro-token step has read: the server's journal lines about nodes that registered
// without a token, the rogue agent's log and the names of the nodes that /v1/nodes listed.
type introEvidence struct {
	Journal  []string
	RogueLog string
	Nodes    []string
}

// problems returns what is missing to show that the servers refused the rogue agent: a server line that holds the
// phrase of a missing token, enforcement_level=strict and node_name=e2e-rogue as fields of their own; the refusal in
// the rogue's log; and no node named e2e-rogue in the list.
func (e introEvidence) problems() []string {
	var problems []string
	if !slices.ContainsFunc(e.Journal, rejectsRogue) {
		problems = append(problems, "the server's journal has no line that rejects node "+rogueName+
			" with enforcement_level=strict")
	}
	if !strings.Contains(e.RogueLog, rogueRefusedText) {
		problems = append(problems, "the rogue's log does not hold "+rogueRefusedText)
	}
	if slices.Contains(e.Nodes, rogueName) {
		problems = append(problems, "node "+rogueName+" is listed in /v1/nodes")
	}
	return problems
}

func rejectsRogue(line string) bool {
	if !strings.Contains(line, introRejectPhrase) {
		return false
	}
	fields := strings.Fields(line)
	return slices.Contains(fields, "enforcement_level=strict") && slices.Contains(fields, "node_name="+rogueName)
}

// introRecord is the text kept for the run: the server lines, the rogue's log and the node names.
func introRecord(e introEvidence) string {
	log := "(empty)"
	if strings.TrimSpace(e.RogueLog) != "" {
		log = strings.TrimRight(e.RogueLog, "\n")
	}
	return fmt.Sprintf("server journal lines:\n%s\n\nrogue log:\n%s\n\nnodes listed:\n%s\n",
		linesOrNone(e.Journal), log, linesOrNone(e.Nodes))
}

func linesOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, "\n")
}

// rogueConfig is the configuration that makes the second agent differ from the machine's own: a name, a data
// directory without an intro token, its own HTTP port and only the exec driver, so it cannot touch the docker
// containers of the real client.
func rogueConfig() string {
	return fmt.Sprintf(`name     = %q
data_dir = %q
ports {
  http = %d
}
client {
  options {
    "driver.allowlist" = "exec"
  }
}
`, rogueName, rogueDataDir, rogueHTTPPort)
}

// rogueStartScript writes the rogue's configuration, empties its data directory and starts the agent in the
// background with the machine's own configuration first; it writes the agent's PID to a file.
func rogueStartScript() string {
	return fmt.Sprintf(`set -e
cat >%s <<'EOF'
%sEOF
rm -rf %s
nohup timeout %d /usr/local/bin/nomad agent -config /etc/nomad.d -config %s >%s 2>&1 </dev/null &
echo $! >%s
`, rogueConfigPath, rogueConfig(), rogueDataDir, rogueLifetime, rogueConfigPath, rogueLogPath, roguePIDPath)
}

// rogueStopScript stops the agent by its PID, waits a little for it to end and removes its files.
func rogueStopScript() string {
	return fmt.Sprintf(`if [ -f %[1]s ]; then
  pid=$(cat %[1]s)
  kill "$pid" 2>/dev/null || true
  i=0
  while kill -0 "$pid" 2>/dev/null && [ "$i" -lt %[2]d ]; do
    sleep 1
    i=$((i + 1))
  done
fi
rm -rf %[3]s %[4]s %[5]s %[1]s
`, roguePIDPath, rogueStopWait, rogueDataDir, rogueConfigPath, rogueLogPath)
}

// rogueLogScript prints the rogue's log; it succeeds when the agent has not written one yet.
func rogueLogScript() string {
	return fmt.Sprintf("cat %s 2>/dev/null || true\n", rogueLogPath)
}

// journalScript prints the lines of the nomad unit's journal since epoch (Unix seconds) that tell of a node that
// registered without an intro token.
func journalScript(epoch int64) string {
	return fmt.Sprintf("journalctl -u nomad --since @%d --no-pager -o cat | grep -F '%s' || true\n",
		epoch, introRejectPhrase)
}

// parseEpoch reads the answer of date +%s.
func parseEpoch(out string) (int64, error) {
	epoch, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("read the server's clock from %q: %w", strings.TrimSpace(out), err)
	}
	if epoch <= 0 {
		return 0, fmt.Errorf("the server's clock says %d", epoch)
	}
	return epoch, nil
}

// nonEmptyLines splits out into lines, trims them and drops the empty ones.
func nonEmptyLines(out string) []string {
	var lines []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// listNodeNames returns the names of the nodes /v1/nodes lists.
func listNodeNames(ctx context.Context, n *nomadAPI) ([]string, error) {
	var nodes []struct{ Name string }
	if err := n.get(ctx, "/v1/nodes", &nodes); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	return names, nil
}

// onlyMachine returns the one machine of a cluster that has role, which must have a main address.
func onlyMachine(instances []vultrapi.Instance, cluster, role string) (vultrapi.Instance, error) {
	found := machinesOf(instances, cluster, role)
	if len(found) != 1 {
		return vultrapi.Instance{}, fmt.Errorf("found %d machines with the role %s in %s, want 1",
			len(found), role, cluster)
	}
	if ip := found[0].MainIP; ip == "" || ip == "0.0.0.0" {
		return vultrapi.Instance{}, fmt.Errorf("the %s machine %s of %s has no main address",
			role, found[0].Label, cluster)
	}
	return found[0], nil
}

// waitIntroRejected calls read every `every` until the evidence it returns shows that the rogue was refused, or
// until timeout. A poll passes only when all its reads succeeded. A node list that holds the rogue ends the wait
// with a failure at once, and the names of all polls are kept, so a rogue that was listed once never goes
// unnoticed. It returns the last evidence, with every node name seen.
func waitIntroRejected(
	ctx context.Context, read func(context.Context) (introEvidence, error), every, timeout time.Duration,
) (introEvidence, error) {
	var last introEvidence
	seen := map[string]bool{}
	err := pollUntil(ctx, every, timeout, func(ctx context.Context) string {
		ev, err := read(ctx)
		for _, name := range ev.Nodes {
			seen[name] = true
		}
		ev.Nodes = slices.Sorted(maps.Keys(seen))
		last = ev
		switch {
		case seen[rogueName]:
			return ""
		case err != nil:
			return err.Error()
		default:
			return strings.Join(ev.problems(), "; ")
		}
	})
	if err != nil {
		return last, err
	}
	if problems := last.problems(); len(problems) > 0 {
		return last, fmt.Errorf("the rogue agent was not refused: %s", strings.Join(problems, "; "))
	}
	return last, nil
}
