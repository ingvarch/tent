package app_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/nomadops"
)

// debugLog returns a logger that writes each record as a JSON line without its time to buf.
func debugLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

// logRecords returns the records that buf holds, in order.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line is not JSON: %v\n%s", err, line)
		}
		out = append(out, rec)
	}
	return out
}

// peerReads returns how many times the roll read the Raft configuration after the first n calls to the Nomad fake:
// once for each observation.
func peerReads(w *nomadWorld, n int) int {
	reads := 0
	for _, name := range nomadCallNames(w, n) {
		if name == "Peers" {
			reads++
		}
	}
	return reads
}

// TestRollLoopLogsOneDebugLineForEachObservation tells the cluster, the leader's node, the voters (not the peers),
// autopilot's health and failure tolerance, and the next step at every observation.
func TestRollLoopLogsOneDebugLineForEachObservation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		var buf bytes.Buffer
		svc.Log = debugLog(&buf)
		calls := len(w.Log())

		_, err := rollUntil(svc, "node done scrub prod-servers-3")

		wantInterrupted(t, err)
		records := logRecords(t, &buf)
		if want := peerReads(w, calls); len(records) != want || want == 0 {
			t.Fatalf("the roll logged %d lines and observed %d times, want one line for each", len(records), want)
		}
		want := map[string]any{
			"level": "DEBUG", "msg": "rolling update observed", "cluster": "prod", "leader": "prod-servers-0",
			"voters": 3.0, "healthy": true, "tolerance": 1.0,
			"next": "create node prod-servers-3 (server of servers, ams)",
		}
		if diff := cmp.Diff(want, records[0]); diff != "" {
			t.Errorf("the first line (-want +got):\n%s", diff)
		}
		waiting := 0
		for _, rec := range records {
			if rec["voters"] == 3.0 && rec["next"] == "wait until node prod-servers-3 joins" {
				waiting++
			}
		}
		if waiting == 0 {
			t.Errorf("no line shows the new server as a peer that does not vote yet: %v", records)
		}
		wantLast := map[string]any{
			"level": "DEBUG", "msg": "rolling update observed", "cluster": "prod", "leader": "prod-servers-0",
			"voters": 4.0, "healthy": true, "tolerance": 1.0, "next": "wait until node prod-servers-3 joins",
		}
		if diff := cmp.Diff(wantLast, records[len(records)-1]); diff != "" {
			t.Errorf("the last line, before the new server is scrubbed (-want +got):\n%s", diff)
		}
	})
}

// TestRollLoopLogsTheRefusalAsTheNextStep logs the text of the decisions' refusal where the step would be, and the
// leader as none when no peer leads.
func TestRollLoopLogsTheRefusalAsTheNextStep(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		w.Unhealthy()
		w.ChangePeers(func(peers []nomadops.Peer) []nomadops.Peer {
			peers = slices.Clone(peers)
			for i := range peers {
				peers[i].Leader = false
			}
			return peers
		})
		var buf bytes.Buffer
		svc.Log = debugLog(&buf)

		_, err := roll(svc, app.RollOptions{})

		if err == nil {
			t.Fatal("the roll ended without an error")
		}
		records := logRecords(t, &buf)
		if len(records) == 0 {
			t.Fatal("the roll logged nothing")
		}
		last := records[len(records)-1]
		if last["leader"] != "none" || last["next"] != err.Error() {
			t.Errorf("the last line is %v, want leader none and the refusal %q", last, err)
		}
	})
}

// TestRollLoopLogsTheObservationThatListsAgainBeforeAStop logs a line also for the observation whose stop waits for a
// list of the machines: the stop is the next step of two lines, the one before the list and the one after it.
func TestRollLoopLogsTheObservationThatListsAgainBeforeAStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		var buf bytes.Buffer
		svc.Log = debugLog(&buf)
		calls := len(w.Log())

		_, err := rollUntil(svc, "node done stop prod-servers-1")

		wantInterrupted(t, err)
		records := logRecords(t, &buf)
		if want := peerReads(w, calls); len(records) != want || want == 0 {
			t.Fatalf("the roll logged %d lines and observed %d times, want one line for each", len(records), want)
		}
		stops := 0
		for _, rec := range records {
			if next, _ := rec["next"].(string); strings.HasPrefix(next, "stop node prod-servers-1 ") {
				stops++
			}
		}
		if stops != 2 {
			t.Errorf("%d lines give the stop as the next step, want 2: one before the list and one after it", stops)
		}
	})
}
