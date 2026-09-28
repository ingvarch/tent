package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/engine"
)

// printProgress returns what prints each step of an update or a delete on w as it happens: a line of text, such as
// "created vultr.VPC/prod", or with -o json a JSON object on one line.
func printProgress(w io.Writer, format string) func(app.Progress) {
	if format != outputJSON {
		return func(p app.Progress) {
			// A step that fails to print changes nothing.
			_, _ = io.WriteString(w, progressText(p)+"\n")
		}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return func(p app.Progress) { _ = enc.Encode(jsonEvent(p)) }
}

// verbs are the forms of the verb of an infrastructure action in progress lines.
type verbs struct{ ing, ed string }

var infraVerbs = map[engine.Action]verbs{
	engine.Create:  {"creating", "created"},
	engine.Update:  {"updating", "updated"},
	engine.Replace: {"replacing", "replaced"},
	engine.Delete:  {"deleting", "deleted"},
}

// nodeLines are the progress lines of each node action, each with %s for the node: started, done and failed.
var nodeLines = map[app.NodeAction][3]string{
	app.NodeCreate: {"creating %s", "created %s", "failed to create %s"},
	app.NodeWait:   {"waiting for %s", "%s is ready", "failed to wait for %s"},
	app.NodeDelete: {"deleting %s", "deleted %s", "failed to delete %s"},
}

// progressText returns the line of text of the step p, which says what happens to which object, such as
// "creating vultr.VPC/prod", "created node prod-servers-0 (10.64.0.3)", "deleted node prod-workers-3 (ID
// instance-8)", "retrying vultr.VPC/prod in 1.2s: <error>" or "waiting for 3 nodes to go".
func progressText(p app.Progress) string {
	switch {
	case p.Infra != nil:
		return infraText(*p.Infra)
	case p.Going == 1:
		return "waiting for 1 node to go"
	case p.Going > 1:
		return fmt.Sprintf("waiting for %d nodes to go", p.Going)
	}
	node := "node " + p.Node.Name
	switch {
	case p.Node.Action == app.NodeDelete:
		node += " (ID " + p.Node.ID + ")"
	case p.Instance.PrivateIP.IsValid():
		node += " (" + p.Instance.PrivateIP.String() + ")"
	}
	lines, ok := nodeLines[p.Node.Action]
	switch {
	case !ok:
		return fmt.Sprintf("%s %s %s", p.Node.Action, p.Step, node)
	case p.Step == app.NodeStarted:
		return fmt.Sprintf(lines[0], node)
	case p.Step == app.NodeDone:
		return fmt.Sprintf(lines[1], node)
	}
	return fmt.Sprintf(lines[2]+": %v", node, p.Err)
}

// infraText returns the line of text of the infrastructure event e.
func infraText(e engine.Event) string {
	obj := e.Key.String()
	if e.Action == engine.Delete {
		obj += " (ID " + e.ID + ")"
	}
	v, ok := infraVerbs[e.Action]
	switch {
	case !ok:
		return fmt.Sprintf("%s %s %s", e.Action, e.Type, obj)
	case e.Type == engine.Started:
		return v.ing + " " + obj
	case e.Type == engine.Succeeded:
		return v.ed + " " + obj
	case e.Type == engine.Retrying:
		return fmt.Sprintf("retrying %s in %s: %v", obj, roundWait(e.Wait), e.Err)
	case e.Type == engine.Failed:
		return fmt.Sprintf("failed to %s %s: %v", e.Action, obj, e.Err)
	case e.Type == engine.Skipped:
		return fmt.Sprintf("skipped %s %s: %s", v.ing, obj, e.Cause)
	}
	return fmt.Sprintf("%s %s %s", e.Action, e.Type, obj)
}

// roundWait rounds the wait before a retry to a tenth of a second, as progress shows it.
func roundWait(d time.Duration) time.Duration { return d.Round(100 * time.Millisecond) }

// infraEvent is an infrastructure event as -o json prints it.
type infraEvent struct {
	Type   string           `json:"type"` // infrastructure
	Event  engine.EventType `json:"event"`
	Kind   string           `json:"kind"`
	Name   string           `json:"name"`
	Action engine.Action    `json:"action"`
	ID     string           `json:"id,omitempty"`
	Wait   string           `json:"wait,omitempty"`
	Cause  string           `json:"cause,omitempty"`
	Error  string           `json:"error,omitempty"`
}

// nodeEvent is a step of a node change as -o json prints it. ID and Address are the machine's, when known.
type nodeEvent struct {
	Type    string         `json:"type"` // node
	Step    string         `json:"step"`
	Action  app.NodeAction `json:"action"`
	Name    string         `json:"name"`
	ID      string         `json:"id,omitempty"`
	Address string         `json:"address,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// waitEvent is the start of the wait for deleted nodes to go as -o json prints it.
type waitEvent struct {
	Type  string `json:"type"`  // wait
	Nodes int    `json:"nodes"` // how many nodes the cloud still lists
}

// jsonEvent returns the step p as -o json prints it.
func jsonEvent(p app.Progress) any {
	if p.Going > 0 {
		return waitEvent{Type: "wait", Nodes: p.Going}
	}
	if e := p.Infra; e != nil {
		ev := infraEvent{Type: "infrastructure", Event: e.Type, Kind: e.Key.Kind, Name: e.Key.Name, Action: e.Action,
			ID: e.ID, Cause: e.Cause, Error: errorText(e.Err)}
		if e.Type == engine.Retrying {
			ev.Wait = roundWait(e.Wait).String()
		}
		return ev
	}
	ev := nodeEvent{Type: "node", Step: p.Step.String(), Action: p.Node.Action, Name: p.Node.Name,
		ID: cmp.Or(p.Instance.ID, p.Node.ID), Error: errorText(p.Err)}
	if a := p.Instance.PrivateIP; a.IsValid() {
		ev.Address = a.String()
	}
	return ev
}

// errorText returns the message of err, or "" for nil.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
