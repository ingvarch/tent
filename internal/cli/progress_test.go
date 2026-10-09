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

// stableUntil is the end of a stability window, given in a zone that is not UTC: 12:04:20 UTC.
var stableUntil = time.Date(2026, 10, 9, 13, 4, 20, 0, time.FixedZone("CET", 3600))

// serverStep returns the progress of the Nomad step e at step, with err for a failed one.
func serverStep(e app.NomadEvent, step app.NodeStep, err error) app.Progress {
	return app.Progress{Nomad: &e, Step: step, Err: err}
}

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
	scrubNode  = app.NodeChange{Action: app.NodeScrub, Name: "prod-workers-0", ID: "instance-4"}
	stopNode   = app.NodeChange{Action: app.NodeStop, Name: "prod-servers-0", ID: "instance-2"}
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
	{"node scrub started", app.Progress{Node: scrubNode, Step: app.NodeStarted},
		"scrubbing the user data of node prod-workers-0",
		`{"type":"node","step":"started","action":"scrub","name":"prod-workers-0","id":"instance-4"}`},
	{"node scrubbed", app.Progress{Node: scrubNode, Step: app.NodeDone},
		"scrubbed the user data of node prod-workers-0",
		`{"type":"node","step":"done","action":"scrub","name":"prod-workers-0","id":"instance-4"}`},
	{"node scrub failed", app.Progress{Node: scrubNode, Step: app.NodeFailed, Err: errBusy},
		"failed to scrub the user data of node prod-workers-0: " + errBusy.Error(),
		`{"type":"node","step":"failed","action":"scrub","name":"prod-workers-0","id":"instance-4",` +
			`"error":"` + errBusy.Error() + `"}`},
	{"node stop started", app.Progress{Node: stopNode, Step: app.NodeStarted},
		"stopping node prod-servers-0 (ID instance-2)",
		`{"type":"node","step":"started","action":"stop","name":"prod-servers-0","id":"instance-2"}`},
	{"node stopped", app.Progress{Node: stopNode, Step: app.NodeDone},
		"stopped node prod-servers-0 (ID instance-2)",
		`{"type":"node","step":"done","action":"stop","name":"prod-servers-0","id":"instance-2"}`},
	{"node stop failed", app.Progress{Node: stopNode, Step: app.NodeFailed, Err: errBusy},
		"failed to stop node prod-servers-0 (ID instance-2): " + errBusy.Error(),
		`{"type":"node","step":"failed","action":"stop","name":"prod-servers-0","id":"instance-2",` +
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
	{"Nomad keyring wait started", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadKeyring},
		Step: app.NodeStarted},
		"waiting for Nomad's keyring",
		`{"type":"nomad","step":"started","action":"keyring"}`},
	{"Nomad keyring ready", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadKeyring}, Step: app.NodeDone},
		"Nomad's keyring is ready",
		`{"type":"nomad","step":"done","action":"keyring"}`},
	{"Nomad keyring wait failed", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadKeyring},
		Step: app.NodeFailed, Err: errBusy},
		"failed to wait for Nomad's keyring: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"keyring","error":"` + errBusy.Error() + `"}`},
	{"Nomad ineligible started", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadIneligible,
		Node: "prod-workers-0"}, Step: app.NodeStarted},
		"marking node prod-workers-0 ineligible",
		`{"type":"nomad","step":"started","action":"ineligible","name":"prod-workers-0"}`},
	{"Nomad node ineligible", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadIneligible,
		Node: "prod-workers-0"}, Step: app.NodeDone},
		"node prod-workers-0 is ineligible",
		`{"type":"nomad","step":"done","action":"ineligible","name":"prod-workers-0"}`},
	{"Nomad ineligible failed", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadIneligible,
		Node: "prod-workers-0"}, Step: app.NodeFailed, Err: errBusy},
		"failed to mark node prod-workers-0 ineligible: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"ineligible","name":"prod-workers-0","error":"` +
			errBusy.Error() + `"}`},
	{"Nomad drain started", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadDrain, Node: "prod-workers-0",
		Deadline: time.Hour}, Step: app.NodeStarted},
		"draining node prod-workers-0 within 1h0m0s",
		`{"type":"nomad","step":"started","action":"drain","name":"prod-workers-0","deadline":"1h0m0s"}`},
	{"Nomad node draining", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadDrain, Node: "prod-workers-0",
		Deadline: time.Hour}, Step: app.NodeDone},
		"node prod-workers-0 is draining",
		`{"type":"nomad","step":"done","action":"drain","name":"prod-workers-0","deadline":"1h0m0s"}`},
	{"Nomad drain failed", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadDrain, Node: "prod-workers-0",
		Deadline: time.Hour}, Step: app.NodeFailed, Err: errBusy},
		"failed to drain node prod-workers-0: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"drain","name":"prod-workers-0","deadline":"1h0m0s",` +
			`"error":"` + errBusy.Error() + `"}`},
	{"Nomad drained wait started", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadDrained,
		Node: "prod-workers-0"}, Step: app.NodeStarted},
		"waiting for node prod-workers-0 to drain",
		`{"type":"nomad","step":"started","action":"drained","name":"prod-workers-0"}`},
	{"Nomad node drained", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadDrained,
		Node: "prod-workers-0"}, Step: app.NodeDone},
		"node prod-workers-0 is drained",
		`{"type":"nomad","step":"done","action":"drained","name":"prod-workers-0"}`},
	{"Nomad drained wait failed", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadDrained,
		Node: "prod-workers-0"}, Step: app.NodeFailed, Err: errBusy},
		"failed to wait for node prod-workers-0 to drain: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"drained","name":"prod-workers-0","error":"` +
			errBusy.Error() + `"}`},
	{"Nomad down wait started", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadDown, Node: "prod-workers-0",
		Address: "10.64.0.6"}, Step: app.NodeStarted},
		"waiting for Nomad to list node prod-workers-0 (10.64.0.6) as down",
		`{"type":"nomad","step":"started","action":"down","name":"prod-workers-0","address":"10.64.0.6"}`},
	{"Nomad node down", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadDown, Node: "prod-workers-0",
		Address: "10.64.0.6"}, Step: app.NodeDone},
		"Nomad lists node prod-workers-0 (10.64.0.6) as down",
		`{"type":"nomad","step":"done","action":"down","name":"prod-workers-0","address":"10.64.0.6"}`},
	{"Nomad down wait failed", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadDown, Node: "prod-workers-0",
		Address: "10.64.0.6"}, Step: app.NodeFailed, Err: errBusy},
		"failed to wait for node prod-workers-0 (10.64.0.6) to go down: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"down","name":"prod-workers-0","address":"10.64.0.6",` +
			`"error":"` + errBusy.Error() + `"}`},
	{"Nomad purge started", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadPurge, Node: "prod-workers-0",
		Address: "10.64.0.6"}, Step: app.NodeStarted},
		"purging node prod-workers-0 (10.64.0.6) from Nomad",
		`{"type":"nomad","step":"started","action":"purge","name":"prod-workers-0","address":"10.64.0.6"}`},
	{"Nomad node purged", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadPurge, Node: "prod-workers-0",
		Address: "10.64.0.6"}, Step: app.NodeDone},
		"purged node prod-workers-0 (10.64.0.6) from Nomad",
		`{"type":"nomad","step":"done","action":"purge","name":"prod-workers-0","address":"10.64.0.6"}`},
	{"Nomad purge failed", app.Progress{Nomad: &app.NomadEvent{Action: app.NomadPurge, Node: "prod-workers-0",
		Address: "10.64.0.6"}, Step: app.NodeFailed, Err: errBusy},
		"failed to purge node prod-workers-0 (10.64.0.6) from Nomad: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"purge","name":"prod-workers-0","address":"10.64.0.6",` +
			`"error":"` + errBusy.Error() + `"}`},
	{"Nomad vote wait started", serverStep(app.NomadEvent{Action: app.NomadVote, Node: "prod-servers-3"},
		app.NodeStarted, nil),
		"waiting for node prod-servers-3 to vote",
		`{"type":"nomad","step":"started","action":"vote","name":"prod-servers-3"}`},
	{"Nomad node votes", serverStep(app.NomadEvent{Action: app.NomadVote, Node: "prod-servers-3"},
		app.NodeDone, nil),
		"node prod-servers-3 votes",
		`{"type":"nomad","step":"done","action":"vote","name":"prod-servers-3"}`},
	{"Nomad vote wait failed", serverStep(app.NomadEvent{Action: app.NomadVote, Node: "prod-servers-3"},
		app.NodeFailed, errBusy),
		"failed to wait for node prod-servers-3 to vote: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"vote","name":"prod-servers-3","error":"` +
			errBusy.Error() + `"}`},
	{"Nomad stable wait started", serverStep(app.NomadEvent{Action: app.NomadStable, Until: stableUntil},
		app.NodeStarted, nil),
		"waiting until 12:04:20 for the servers to be stable",
		`{"type":"nomad","step":"started","action":"stable","until":"2026-10-09T12:04:20Z"}`},
	{"Nomad servers stable", serverStep(app.NomadEvent{Action: app.NomadStable, Until: stableUntil},
		app.NodeDone, nil),
		"the servers are stable",
		`{"type":"nomad","step":"done","action":"stable","until":"2026-10-09T12:04:20Z"}`},
	{"Nomad stable wait failed", serverStep(app.NomadEvent{Action: app.NomadStable, Until: stableUntil},
		app.NodeFailed, errBusy),
		"failed to wait for the servers to be stable: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"stable","until":"2026-10-09T12:04:20Z","error":"` +
			errBusy.Error() + `"}`},
	{"Nomad transfer started", serverStep(app.NomadEvent{Action: app.NomadTransfer, Node: "prod-servers-0",
		Leader: "prod-servers-3"}, app.NodeStarted, nil),
		"moving the leadership from prod-servers-0 to prod-servers-3",
		`{"type":"nomad","step":"started","action":"transfer","name":"prod-servers-0","leader":"prod-servers-3"}`},
	{"Nomad leadership moved", serverStep(app.NomadEvent{Action: app.NomadTransfer, Node: "prod-servers-0",
		Leader: "prod-servers-3"}, app.NodeDone, nil),
		"moved the leadership from prod-servers-0 to prod-servers-3",
		`{"type":"nomad","step":"done","action":"transfer","name":"prod-servers-0","leader":"prod-servers-3"}`},
	{"Nomad transfer failed", serverStep(app.NomadEvent{Action: app.NomadTransfer, Node: "prod-servers-0",
		Leader: "prod-servers-3"}, app.NodeFailed, errBusy),
		"failed to move the leadership from prod-servers-0 to prod-servers-3: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"transfer","name":"prod-servers-0","leader":"prod-servers-3",` +
			`"error":"` + errBusy.Error() + `"}`},
	{"Nomad server-down wait started", serverStep(app.NomadEvent{Action: app.NomadServerDown,
		Node: "prod-servers-0"}, app.NodeStarted, nil),
		"waiting until autopilot no longer counts prod-servers-0 as a healthy voter",
		`{"type":"nomad","step":"started","action":"server-down","name":"prod-servers-0"}`},
	{"Nomad server no longer counted", serverStep(app.NomadEvent{Action: app.NomadServerDown,
		Node: "prod-servers-0"}, app.NodeDone, nil),
		"autopilot no longer counts prod-servers-0 as a healthy voter",
		`{"type":"nomad","step":"done","action":"server-down","name":"prod-servers-0"}`},
	{"Nomad server-down wait failed", serverStep(app.NomadEvent{Action: app.NomadServerDown,
		Node: "prod-servers-0"}, app.NodeFailed, errBusy),
		"failed to wait for autopilot to stop counting prod-servers-0: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"server-down","name":"prod-servers-0","error":"` +
			errBusy.Error() + `"}`},
	{"Nomad remove-peer started", serverStep(app.NomadEvent{Action: app.NomadRemovePeer, Node: "prod-servers-0"},
		app.NodeStarted, nil),
		"removing prod-servers-0 from the Raft configuration",
		`{"type":"nomad","step":"started","action":"remove-peer","name":"prod-servers-0"}`},
	{"Nomad peer removed", serverStep(app.NomadEvent{Action: app.NomadRemovePeer, Node: "prod-servers-0"},
		app.NodeDone, nil),
		"removed prod-servers-0 from the Raft configuration",
		`{"type":"nomad","step":"done","action":"remove-peer","name":"prod-servers-0"}`},
	{"Nomad remove-peer failed", serverStep(app.NomadEvent{Action: app.NomadRemovePeer, Node: "prod-servers-0"},
		app.NodeFailed, errBusy),
		"failed to remove prod-servers-0 from the Raft configuration: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"remove-peer","name":"prod-servers-0","error":"` +
			errBusy.Error() + `"}`},
	{"Nomad force-leave started", serverStep(app.NomadEvent{Action: app.NomadForceLeave,
		Node: "prod-servers-0.global"}, app.NodeStarted, nil),
		"forcing prod-servers-0.global out of the gossip pool",
		`{"type":"nomad","step":"started","action":"force-leave","name":"prod-servers-0.global"}`},
	{"Nomad member forced out", serverStep(app.NomadEvent{Action: app.NomadForceLeave,
		Node: "prod-servers-0.global"}, app.NodeDone, nil),
		"forced prod-servers-0.global out of the gossip pool",
		`{"type":"nomad","step":"done","action":"force-leave","name":"prod-servers-0.global"}`},
	{"Nomad force-leave failed", serverStep(app.NomadEvent{Action: app.NomadForceLeave,
		Node: "prod-servers-0.global"}, app.NodeFailed, errBusy),
		"failed to force prod-servers-0.global out of the gossip pool: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"force-leave","name":"prod-servers-0.global","error":"` +
			errBusy.Error() + `"}`},
	{"Nomad settle started", serverStep(app.NomadEvent{Action: app.NomadSettle, Deadline: time.Minute,
		Reason: "autopilot is unhealthy"}, app.NodeStarted, nil),
		"waiting up to 1m0s for the cluster to settle: autopilot is unhealthy",
		`{"type":"nomad","step":"started","action":"settle","deadline":"1m0s","reason":"autopilot is unhealthy"}`},
	{"Nomad cluster settled", serverStep(app.NomadEvent{Action: app.NomadSettle, Deadline: time.Minute,
		Reason: "autopilot is unhealthy"}, app.NodeDone, nil),
		"the cluster settled",
		`{"type":"nomad","step":"done","action":"settle","deadline":"1m0s","reason":"autopilot is unhealthy"}`},
	{"Nomad settle failed", serverStep(app.NomadEvent{Action: app.NomadSettle, Deadline: time.Minute,
		Reason: "autopilot is unhealthy"}, app.NodeFailed, errBusy),
		"failed to wait for the cluster to settle: " + errBusy.Error(),
		`{"type":"nomad","step":"failed","action":"settle","deadline":"1m0s",` +
			`"reason":"autopilot is unhealthy","error":"` + errBusy.Error() + `"}`},
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
