package nodeconfig

// Paths that the Nomad agent's installation uses.
const (
	// NomadServiceFile is the unit that runs the Nomad agent. It is a group-level file, so a change of it marks
	// every node of the group out of date.
	NomadServiceFile = "/etc/systemd/system/nomad.service"
	// NomadBinary is where the node puts the Nomad binary of the nomad asset.
	NomadBinary = "/usr/local/bin/nomad"
)

// nomadService is nomad.service. It has no [Install] section, so it is never enabled for boot: tent-node's up starts
// it after the host firewall, so that no workload runs before the rules, and no ordering ties it to
// tent-node.service, which would deadlock. It is ordered after docker.service and tied to it in no other way:
// systemd stops Nomad before Docker, so the agent stops while the Docker daemon still answers it, and a restart of
// Docker leaves Nomad running. On a node without Docker the order does nothing. Type=notify, since Nomad answers
// sd_notify; KillMode=process, so that executors and logmon survive a restart of the agent; SIGTERM, which a
// client answers by leaving the cluster and a server by exiting at once, still a Raft peer; and systemd's default
// stop timeout of 90 seconds, which covers Nomad's 5-second graceful wait.
var nomadService = header + `[Unit]
Description=The Nomad agent of this node
Documentation=https://developer.hashicorp.com/nomad
Wants=network-online.target
After=network-online.target docker.service

[Service]
Type=notify
ExecStart=` + NomadBinary + ` agent -config /etc/nomad.d
ExecReload=/bin/kill -HUP $MAINPID
KillMode=process
KillSignal=SIGTERM
Restart=on-failure
RestartSec=2
LimitNOFILE=65536
LimitNPROC=infinity
TasksMax=infinity
OOMScoreAdjust=-1000
`

// RenderNomadService returns the nomad.service unit file. It holds no setting of the node or the group, so every
// node has the same one. It keeps the stock unit's ExecReload, which sends SIGHUP, so that operators can still run
// systemctl reload nomad.
func RenderNomadService() File {
	return File{Path: NomadServiceFile, Mode: 0o644, Owner: Owner, Content: []byte(nomadService)}
}
