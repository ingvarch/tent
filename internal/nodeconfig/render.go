package nodeconfig

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/secret"
)

// Agent is what the group-level Nomad agent configuration of a node group is made from.
type Agent struct {
	Role    v1alpha1.Role
	Cluster string // the cluster's name, which clients carry in their meta as tent_cluster
	Group   string // the node group's name, which clients carry in their meta as tent_nodegroup
	Region  string // the Nomad region
	// CIDR is the cluster's private network. Nomad binds to, and finds its interface by, the address it has there.
	CIDR netip.Prefix

	// Server settings, for server and combined groups.
	ClientIntroduction v1alpha1.ClientIntroduction // how strictly servers require intro tokens from new clients
	Gossip             secret.Secret               // the key that encrypts the servers' gossip

	VerifyHTTPSClient bool // the HTTP API requires client certificates

	// Client settings, for client and combined groups. Empty strings and lists leave Nomad's defaults.
	NodePool  string
	NodeClass string
	// Drivers are the task drivers that the clients allow; none keeps all of Nomad's built-in drivers. raw_exec stays
	// disabled unless the extra configuration enables it, and java and qemu need packages that tent does not install.
	Drivers []string
	Meta    map[string]string // client metadata; keys starting with tent_ are tent's own
	// DynamicPorts are the ports that Nomad gives workloads, which the host firewall opens.
	DynamicPorts PortRange

	// ExtraServer and ExtraClient are added as given to servers and to clients. They can override anything above,
	// the dynamic ports included, and the host firewall does not follow them.
	ExtraServer string
	ExtraClient string
}

// Paths of the agent configuration files. Nomad merges the files of its configuration directory in the order of their
// names, so the operator's files come last, the server part before the client part.
const (
	tentPath       = "/etc/nomad.d/00-tent.hcl"
	gossipPath     = "/etc/nomad.d/01-gossip.hcl"
	userServerPath = "/etc/nomad.d/98-user-server.hcl"
	userClientPath = "/etc/nomad.d/99-user-client.hcl"
)

// Paths of the files that the agent configuration names, which a NodeConfig carries.
const (
	CAFile   = "/etc/nomad.d/tls/ca.pem"        // the cluster's CA certificates, alike on every node of the cluster
	CertFile = "/etc/nomad.d/tls/agent.pem"     // the node's certificate
	KeyFile  = "/etc/nomad.d/tls/agent-key.pem" // the node's private key
	// IntroTokenFile is where a client finds its intro token: intro_token.jwt in its state directory, which Nomad
	// keeps in the data directory.
	IntroTokenFile = dataDir + "/client/intro_token.jwt"
	dataDir        = "/var/lib/nomad"
)

// Owner owns every file that tent writes onto a node: the Nomad agent runs as root.
const Owner = "root:root"

// header starts every file that tent renders.
const header = "# Rendered by tent. Do not edit: changes are overwritten on the next boot.\n"

// RenderAgent returns the group-level Nomad agent configuration of a node group, the files that are alike on every
// node of the group, sorted by path:
//   - 00-tent.hcl, tent's settings: a server block for server and combined groups, a client block and an rpc block
//     for client and combined groups, and ACLs, mTLS and telemetry for all;
//   - 01-gossip.hcl, the gossip key, for server and combined groups; the only secret file;
//   - 98-user-server.hcl and 99-user-client.hcl, the extra configuration of the role's parts, when there is any.
//
// Every value is written as an HCL1 string that reads back as the value itself, or refused: a value that is not valid
// UTF-8, has a control character or U+E123, or has "${", which HCL1 reads as the start of an interpolation. A driver
// must not be empty or have a comma or white space, meta keys must not be empty or start with tent_, and the extra
// configuration must be valid UTF-8. Settings of the other role are ignored. The error is the first problem in the
// order of Agent's fields.
func RenderAgent(a Agent) ([]File, error) {
	files, err := a.render()
	if err != nil {
		return nil, fmt.Errorf("nomad agent: %w", err)
	}
	return files, nil
}

// agentView is what the template of 00-tent.hcl shows: every string is an HCL1 string already, and an empty string
// is left out.
type agentView struct {
	Server, Client    bool
	Region            string
	Private           string // the private address
	LocalAndPrivate   string // localhost and the private address
	Interface         string // the name of the private interface
	Enforcement       string
	VerifyHTTPSClient bool
	NodePool          string
	NodeClass         string
	Drivers           string
	Meta              []string // the lines of the meta block
	DynamicPorts      PortRange
}

func (a Agent) render() ([]File, error) {
	if err := checkRole(a.Role); err != nil {
		return nil, err
	}
	server, client := a.Role.RunsServer(), a.Role.RunsClient()
	// The checks run in the order of Agent's fields, and p keeps the first problem.
	var p problems
	cluster := p.quoteSet("cluster", a.Cluster)
	group := p.quoteSet("node group", a.Group)
	v := agentView{Server: server, Client: client, Region: p.quoteSet("region", a.Region)}
	if err := checkNetwork(a.CIDR); err != nil {
		p.add(fmt.Errorf("CIDR %w", err))
	}
	v.Private = p.quote("CIDR", sockaddr(a.CIDR, "address"))
	v.LocalAndPrivate = p.quote("CIDR", "127.0.0.1 "+sockaddr(a.CIDR, "address"))
	v.Interface = p.quote("CIDR", sockaddr(a.CIDR, "name"))
	var gossip string
	if server {
		if !slices.Contains(v1alpha1.ClientIntroductions(), a.ClientIntroduction) {
			p.add(fmt.Errorf("client introduction %q is not strict, warn or none", a.ClientIntroduction))
		}
		v.Enforcement = p.quote("client introduction", string(a.ClientIntroduction))
		gossip = p.quoteSet("gossip key", string(a.Gossip))
	}
	v.VerifyHTTPSClient = a.VerifyHTTPSClient
	if client {
		a.clientView(&v, &p, cluster, group)
	}
	if server && !utf8.ValidString(a.ExtraServer) {
		p.add(errors.New("spec.nomad.extraConfig.server is not valid UTF-8"))
	}
	if client && !utf8.ValidString(a.ExtraClient) {
		p.add(errors.New("spec.nomad.extraConfig.client is not valid UTF-8"))
	}
	if p.err != nil {
		return nil, p.err
	}
	var buf bytes.Buffer
	if err := agentTemplate.Execute(&buf, v); err != nil {
		return nil, fmt.Errorf("render %s: %w", tentPath, err)
	}
	// tent's settings hold no secret, so every local user may read them, as other files in /etc.
	files := []File{{Path: tentPath, Mode: 0o644, Owner: Owner, Content: buf.Bytes()}}
	if server {
		files = append(files, File{Path: gossipPath, Mode: 0o600, Owner: Owner, Secret: true,
			Content: []byte(header + "server {\n  encrypt = " + gossip + "\n}\n")})
		if a.ExtraServer != "" {
			files = append(files, userFile(userServerPath, "server", a.ExtraServer))
		}
	}
	if client && a.ExtraClient != "" {
		files = append(files, userFile(userClientPath, "client", a.ExtraClient))
	}
	return files, nil
}

// clientView checks the client settings and puts them into v, with the cluster and the node group as HCL1 strings.
func (a Agent) clientView(v *agentView, p *problems, cluster, group string) {
	if a.NodePool != "" {
		v.NodePool = p.quote("node pool", a.NodePool)
	}
	if a.NodeClass != "" {
		v.NodeClass = p.quote("node class", a.NodeClass)
	}
	for _, d := range a.Drivers {
		// Nomad splits the allow list at commas and trims white space.
		if d == "" || strings.ContainsFunc(d, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
			p.add(fmt.Errorf("driver %q is empty or has a comma or white space", d))
		}
		p.quote(fmt.Sprintf("driver %q", d), d)
	}
	if len(a.Drivers) > 0 {
		v.Drivers = p.quote("drivers", strings.Join(a.Drivers, ","))
	}
	pairs := [][2]string{{`"tent_cluster"`, cluster}, {`"tent_nodegroup"`, group}}
	for _, key := range slices.Sorted(maps.Keys(a.Meta)) {
		switch {
		case key == "":
			p.add(errors.New("meta has an empty key"))
		case strings.HasPrefix(key, "tent_"):
			p.add(fmt.Errorf("meta key %q starts with tent_, which tent keeps for its own keys", key))
		}
		pairs = append(pairs, [2]string{
			p.quote(fmt.Sprintf("meta key %q", key), key),
			p.quote(fmt.Sprintf("meta %q: the value", key), a.Meta[key]),
		})
	}
	width := 0
	for _, kv := range pairs {
		width = max(width, utf8.RuneCountInString(kv[0]))
	}
	for _, kv := range pairs {
		v.Meta = append(v.Meta, fmt.Sprintf("%-*s = %s", width, kv[0], kv[1]))
	}
	if err := a.DynamicPorts.check(); err != nil {
		p.add(fmt.Errorf("dynamic %w", err))
	}
	v.DynamicPorts = a.DynamicPorts
}

// sockaddr returns the go-sockaddr template that Nomad resolves to an attribute of the private interface, the one
// with an address in cidr, such as its address or its name.
func sockaddr(cidr netip.Prefix, attr string) string {
	return fmt.Sprintf(`{{ GetPrivateInterfaces | include "network" %q | attr %q }}`, cidr.String(), attr)
}

// userFile returns the file that holds the extra configuration of one part of the agent, server or client, as given.
func userFile(path, part, hcl string) File {
	content := "# spec.nomad.extraConfig." + part + " of the cluster, added as given. Do not edit: changes are " +
		"overwritten on the next boot.\n" + hcl
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	// tent does not know what the operator's configuration holds, so only root reads it, unlike tent's own settings.
	return File{Path: path, Mode: 0o600, Owner: Owner, Content: []byte(content)}
}

// problems keeps the first problem that a render finds.
type problems struct{ err error }

func (p *problems) add(err error) {
	if p.err == nil {
		p.err = err
	}
}

// quote returns s as an HCL1 string, and adds the problem when it cannot, named by what s is.
func (p *problems) quote(what, s string) string {
	q, err := quote(s)
	if err != nil {
		p.add(fmt.Errorf("%s %w", what, err))
	}
	return q
}

// quoteSet is quote for a value that must not be empty.
func (p *problems) quoteSet(what, s string) string {
	if s == "" {
		p.add(errors.New("no " + what))
		return ""
	}
	return p.quote(what, s)
}

// agentTemplate renders 00-tent.hcl from an agentView.
var agentTemplate = template.Must(template.New("00-tent.hcl").Parse(header + `region               = {{.Region}}
data_dir             = "` + dataDir + `"
{{- if .Server}}
leave_on_terminate   = false # a stopped server stays a Raft peer; tent removes servers through the Nomad API
{{- else}}
leave_on_terminate   = true # leave the cluster gracefully when Nomad stops
{{- end}}
disable_update_check = true

addresses {
{{- if .Server}}
  http = "0.0.0.0" # the tent CLI reaches it through the cloud firewall
{{- else}}
  http = {{.LocalAndPrivate}}
{{- end}}
  rpc  = {{.Private}}
{{- if .Server}}
  serf = {{.Private}}
{{- end}}
}

advertise {
  http = {{.Private}}
  rpc  = {{.Private}}
{{- if .Server}}
  serf = {{.Private}}
{{- end}}
}
{{- if .Server}}

server {
  enabled         = true
  heartbeat_grace = "20s" # after a missed heartbeat a client has 20 s to reach another server before it reads down

  client_introduction {
    enforcement = {{.Enforcement}}
  }
}
{{- end}}
{{- if .Client}}

client {
  enabled           = true
{{- with .NodePool}}
  node_pool         = {{.}}
{{- end}}
{{- with .NodeClass}}
  node_class        = {{.}}
{{- end}}
  network_interface = {{.Interface}}
  min_dynamic_port  = {{.DynamicPorts.First}}
  max_dynamic_port  = {{.DynamicPorts.Last}}

  options {
{{- with .Drivers}}
    "driver.allowlist"     = {{.}}
{{- end}}
    # Cloud fingerprinters only slow down startup on Vultr and Hetzner.
    "fingerprint.denylist" = "env_aws,env_gce,env_azure,env_digitalocean"
  }

  meta {
{{- range .Meta}}
    {{.}}
{{- end}}
  }
}

# A client pings its server every 5 s, so that it drops a server that stopped answering and heartbeats another one
# before the servers' heartbeat_grace runs out and they count its node as down.
rpc {
  keep_alive_interval = "5s"
}
{{- end}}

acl {
  enabled = true
}

tls {
  http = true
  rpc  = true

  ca_file   = "` + CAFile + `"
  cert_file = "` + CertFile + `"
  key_file  = "` + KeyFile + `"

  verify_server_hostname = true
  verify_https_client    = {{.VerifyHTTPSClient}}
}

# There is no Consul: do not look for Nomad servers in it.
consul {
  server_auto_join = false
  client_auto_join = false
}
{{- if .Server}}

autopilot {
  cleanup_dead_servers = true
}
{{- end}}

telemetry {
  prometheus_metrics   = true
  publish_node_metrics = true
}
`))
