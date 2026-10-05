package cli

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/engine"
)

// progressCase is one step of an update or a delete and the line that tent prints for it, as text and as JSON.
type progressCase struct {
	name       string
	p          app.Progress
	text, json string
}

var (
	vpcKey     = engine.Key{Kind: "vultr.VPC", Name: "prod"}
	serversKey = engine.Key{Kind: "vultr.FirewallGroup", Name: "prod-servers"}
	errBusy    = errors.New("vultr: POST /v2/vpcs: 503 Service Unavailable: busy")
)

// infraStep returns the progress of an infrastructure event.
func infraStep(e engine.Event) app.Progress { return app.Progress{Infra: &e} }

// createNode, waitNode and deleteNode are node changes of the test cluster.
var (
	createNode = app.NodeChange{Action: app.NodeCreate, Name: "prod-servers-0", Group: "servers", Role: "server",
		Zone: "ams", MachineType: "vc2-2c-4gb", Image: "ubuntu-24.04"}
	waitNode = app.NodeChange{Action: app.NodeWait, Name: "prod-servers-1", Group: "servers", Role: "server",
		Zone: "ams", MachineType: "vc2-2c-4gb", Image: "ubuntu-24.04", ID: "instance-2",
		Op: "5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70"}
	deleteNode = app.NodeChange{Action: app.NodeDelete, Name: "prod-workers-3", ID: "instance-8", Reason: "surplus"}
)

var progressCases = []progressCase{
	{"infrastructure started", infraStep(engine.Event{Type: engine.Started, Key: vpcKey, Action: engine.Create}),
		"creating vultr.VPC/prod",
		`{"type":"infrastructure","event":"started","kind":"vultr.VPC","name":"prod","action":"create"}`},
	{"infrastructure succeeded", infraStep(engine.Event{Type: engine.Succeeded, Key: vpcKey, Action: engine.Create}),
		"created vultr.VPC/prod",
		`{"type":"infrastructure","event":"succeeded","kind":"vultr.VPC","name":"prod","action":"create"}`},
	{"update", infraStep(engine.Event{Type: engine.Started, Key: serversKey, Action: engine.Update}),
		"updating vultr.FirewallGroup/prod-servers",
		`{"type":"infrastructure","event":"started","kind":"vultr.FirewallGroup","name":"prod-servers",` +
			`"action":"update"}`},
	{"replace", infraStep(engine.Event{Type: engine.Succeeded, Key: serversKey, Action: engine.Replace}),
		"replaced vultr.FirewallGroup/prod-servers",
		`{"type":"infrastructure","event":"succeeded","kind":"vultr.FirewallGroup","name":"prod-servers",` +
			`"action":"replace"}`},
	{"delete", infraStep(engine.Event{Type: engine.Started, Key: vpcKey, ID: "vpc-1", Action: engine.Delete}),
		"deleting vultr.VPC/prod (ID vpc-1)",
		`{"type":"infrastructure","event":"started","kind":"vultr.VPC","name":"prod","action":"delete","id":"vpc-1"}`},
	{"deleted", infraStep(engine.Event{Type: engine.Succeeded, Key: vpcKey, ID: "vpc-1", Action: engine.Delete}),
		"deleted vultr.VPC/prod (ID vpc-1)",
		`{"type":"infrastructure","event":"succeeded","kind":"vultr.VPC","name":"prod","action":"delete",` +
			`"id":"vpc-1"}`},
	{"retrying", infraStep(engine.Event{Type: engine.Retrying, Key: vpcKey, Action: engine.Create, Err: errBusy,
		Wait: 1234567890 * time.Nanosecond}),
		"retrying vultr.VPC/prod in 1.2s: " + errBusy.Error(),
		`{"type":"infrastructure","event":"retrying","kind":"vultr.VPC","name":"prod","action":"create",` +
			`"wait":"1.2s","error":"` + errBusy.Error() + `"}`},
	{"failed", infraStep(engine.Event{Type: engine.Failed, Key: vpcKey, Action: engine.Create, Err: errBusy}),
		"failed to create vultr.VPC/prod: " + errBusy.Error(),
		`{"type":"infrastructure","event":"failed","kind":"vultr.VPC","name":"prod","action":"create",` +
			`"error":"` + errBusy.Error() + `"}`},
	{"skipped", infraStep(engine.Event{Type: engine.Skipped, Key: serversKey, Action: engine.Create,
		Cause: "vultr.VPC/prod failed"}),
		"skipped creating vultr.FirewallGroup/prod-servers: vultr.VPC/prod failed",
		`{"type":"infrastructure","event":"skipped","kind":"vultr.FirewallGroup","name":"prod-servers",` +
			`"action":"create","cause":"vultr.VPC/prod failed"}`},
	{"skipped delete", infraStep(engine.Event{Type: engine.Skipped, Key: vpcKey, ID: "vpc-1", Action: engine.Delete,
		Cause: "cancelled"}),
		"skipped deleting vultr.VPC/prod (ID vpc-1): cancelled",
		`{"type":"infrastructure","event":"skipped","kind":"vultr.VPC","name":"prod","action":"delete",` +
			`"id":"vpc-1","cause":"cancelled"}`},

	{"node create started", app.Progress{Node: createNode, Step: app.NodeStarted},
		"creating node prod-servers-0",
		`{"type":"node","step":"started","action":"create","name":"prod-servers-0"}`},
	{"node created", app.Progress{Node: createNode, Step: app.NodeDone,
		Instance: cloud.Instance{ID: "instance-1", PrivateIP: netip.MustParseAddr("10.64.0.3")}},
		"created node prod-servers-0 (10.64.0.3)",
		`{"type":"node","step":"done","action":"create","name":"prod-servers-0","id":"instance-1",` +
			`"address":"10.64.0.3"}`},
	{"node created without an address", app.Progress{Node: createNode, Step: app.NodeDone,
		Instance: cloud.Instance{ID: "instance-1"}},
		"created node prod-servers-0",
		`{"type":"node","step":"done","action":"create","name":"prod-servers-0","id":"instance-1"}`},
	{"node create failed", app.Progress{Node: createNode, Step: app.NodeFailed, Err: errBusy},
		"failed to create node prod-servers-0: " + errBusy.Error(),
		`{"type":"node","step":"failed","action":"create","name":"prod-servers-0","error":"` + errBusy.Error() + `"}`},
	{"node wait started", app.Progress{Node: waitNode, Step: app.NodeStarted},
		"waiting for node prod-servers-1",
		`{"type":"node","step":"started","action":"wait","name":"prod-servers-1","id":"instance-2"}`},
	{"node ready", app.Progress{Node: waitNode, Step: app.NodeDone,
		Instance: cloud.Instance{ID: "instance-2", PrivateIP: netip.MustParseAddr("10.64.0.4")}},
		"node prod-servers-1 (10.64.0.4) is ready",
		`{"type":"node","step":"done","action":"wait","name":"prod-servers-1","id":"instance-2",` +
			`"address":"10.64.0.4"}`},
	{"node wait failed", app.Progress{Node: waitNode, Step: app.NodeFailed, Err: errBusy},
		"failed to wait for node prod-servers-1: " + errBusy.Error(),
		`{"type":"node","step":"failed","action":"wait","name":"prod-servers-1","id":"instance-2",` +
			`"error":"` + errBusy.Error() + `"}`},
	{"node delete started", app.Progress{Node: deleteNode, Step: app.NodeStarted},
		"deleting node prod-workers-3 (ID instance-8)",
		`{"type":"node","step":"started","action":"delete","name":"prod-workers-3","id":"instance-8"}`},
	{"node deleted", app.Progress{Node: deleteNode, Step: app.NodeDone},
		"deleted node prod-workers-3 (ID instance-8)",
		`{"type":"node","step":"done","action":"delete","name":"prod-workers-3","id":"instance-8"}`},
	{"node delete failed", app.Progress{Node: deleteNode, Step: app.NodeFailed, Err: errBusy},
		"failed to delete node prod-workers-3 (ID instance-8): " + errBusy.Error(),
		`{"type":"node","step":"failed","action":"delete","name":"prod-workers-3","id":"instance-8",` +
			`"error":"` + errBusy.Error() + `"}`},

	{"Nomad leader started", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadLeader}, Step: app.NodeStarted},
		"waiting for a Nomad leader",
		`{"type":"nomad","step":"started","action":"leader"}`},
	{"Nomad leader found", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadLeader, Leader: "10.64.0.3:4647"},
		Step: app.NodeDone},
		"Nomad has a leader (10.64.0.3:4647)",
		`{"type":"nomad","step":"done","action":"leader","leader":"10.64.0.3:4647"}`},
	{"Nomad leader failed", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadLeader}, Step: app.NodeFailed,
		Err: errBusy},
		"failed to wait for a Nomad leader: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"leader","error":"` + errBusy.Error() + `"}`},
	{"Nomad bootstrap started", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadBootstrap},
		Step: app.NodeStarted},
		"bootstrapping the ACL system",
		`{"type":"nomad","step":"started","action":"bootstrap"}`},
	{"Nomad bootstrapped", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadBootstrap}, Step: app.NodeDone},
		"bootstrapped the ACL system",
		`{"type":"nomad","step":"done","action":"bootstrap"}`},
	{"Nomad bootstrap failed", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadBootstrap},
		Step: app.NodeFailed, Err: errBusy},
		"failed to bootstrap the ACL system: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"bootstrap","error":"` + errBusy.Error() + `"}`},
	{"Nomad servers wait started", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadHealthy, Voters: 3},
		Step: app.NodeStarted},
		"waiting for 3 healthy Nomad servers",
		`{"type":"nomad","step":"started","action":"healthy","voters":3}`},
	{"Nomad servers healthy", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadHealthy, Voters: 3},
		Step: app.NodeDone},
		"3 Nomad servers are healthy",
		`{"type":"nomad","step":"done","action":"healthy","voters":3}`},
	{"Nomad servers wait failed", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadHealthy, Voters: 3},
		Step: app.NodeFailed, Err: errBusy},
		"failed to wait for 3 healthy Nomad servers: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"healthy","voters":3,"error":"` + errBusy.Error() + `"}`},
	{"Nomad one server healthy", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadHealthy, Voters: 1},
		Step: app.NodeDone},
		"1 Nomad server is healthy",
		`{"type":"nomad","step":"done","action":"healthy","voters":1}`},
	{"Nomad one server wait started", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadHealthy, Voters: 1},
		Step: app.NodeStarted},
		"waiting for 1 healthy Nomad server",
		`{"type":"nomad","step":"started","action":"healthy","voters":1}`},
	{"Nomad register started", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadRegister,
		Node: "prod-workers-0"}, Step: app.NodeStarted},
		"waiting for node prod-workers-0 to register",
		`{"type":"nomad","step":"started","action":"register","name":"prod-workers-0"}`},
	{"Nomad node registered", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadRegister,
		Node: "prod-workers-0"}, Step: app.NodeDone},
		"node prod-workers-0 registered",
		`{"type":"nomad","step":"done","action":"register","name":"prod-workers-0"}`},
	{"Nomad register failed", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadRegister,
		Node: "prod-workers-0"}, Step: app.NodeFailed, Err: errBusy},
		"failed to wait for node prod-workers-0 to register: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"register","name":"prod-workers-0","error":"` +
			errBusy.Error() + `"}`},
	{"Nomad unknown action", app.Progress{Nomad: &app.NomadEvent{}, Step: app.NodeStarted},
		"NomadAction(0) started Nomad",
		`{"type":"nomad","step":"started","action":"NomadAction(0)"}`},

	{"wait for three nodes to go", app.Progress{Going: 3}, "waiting for 3 nodes to go", `{"type":"wait","nodes":3}`},
	{"wait for one node to go", app.Progress{Going: 1}, "waiting for 1 node to go", `{"type":"wait","nodes":1}`},
}

// TestProgress prints each step on a line of its own: text for -o table and -o yaml, a JSON object for -o json.
func TestProgress(t *testing.T) {
	for _, format := range []string{outputTable, outputYAML, outputJSON} {
		t.Run(format, func(t *testing.T) {
			for _, tc := range progressCases {
				t.Run(tc.name, func(t *testing.T) {
					var b strings.Builder
					printProgress(&b, format)(tc.p)
					want := tc.text
					if format == outputJSON {
						want = tc.json
					}
					if diff := cmp.Diff(want+"\n", b.String()); diff != "" {
						t.Errorf("the line (-want +got):\n%s", diff)
					}
				})
			}
		})
	}
}

// TestProgressJSONKeepsHTMLCharacters leaves < and & in an error as they are.
func TestProgressJSONKeepsHTMLCharacters(t *testing.T) {
	var b strings.Builder
	printProgress(&b, outputJSON)(app.Progress{Node: createNode, Step: app.NodeFailed,
		Err: errors.New("plan <vc2> & more")})
	const want = `{"type":"node","step":"failed","action":"create","name":"prod-servers-0",` +
		`"error":"plan <vc2> & more"}` + "\n"
	if got := b.String(); got != want {
		t.Errorf("the line\n%s\nwant\n%s", got, want)
	}
}
