package nodeconfig

import (
	"fmt"
	"strings"

	"github.com/ingvarch/tent/api/v1alpha1"
)

// Paths of the files that only one node has. Nomad merges them between tent's settings and the operator's files.
const (
	joinPath     = "/etc/nomad.d/05-join.hcl"
	nodePath     = "/etc/nomad.d/10-node.hcl"
	instancePath = "/etc/nomad.d/11-instance.hcl"
)

// NodeHeader starts every file that tent-node renders on the node.
const NodeHeader = "# Rendered by tent-node. Do not edit: changes are overwritten.\n"

// perNodeFile returns a file that only one node has. It holds no secret, so every local user may read it, as tent's
// settings.
func perNodeFile(path string, content []byte) File {
	return File{Path: path, Mode: 0o644, Owner: Owner, Content: content, PerNode: true}
}

// RenderNode returns 10-node.hcl, the settings of one node: its name, its datacenter and, on server and combined
// nodes, the number of servers that bootstrap the cluster. Clients ignore bootstrapExpect. A bootstrapExpect of 0 on a
// server gives no server block: the node joins servers that exist and never starts a cluster of its own. The name
// must be a host name, the datacenter an HCL1 string without "*", the role server, client or combined, and
// bootstrapExpect at least 0 on a server. The error is the first problem in the order of the arguments.
func RenderNode(name, datacenter string, role v1alpha1.Role, bootstrapExpect int) (File, error) {
	content, err := renderNode(name, datacenter, role, bootstrapExpect)
	if err != nil {
		return File{}, fmt.Errorf("10-node.hcl: %w", err)
	}
	return perNodeFile(nodePath, content), nil
}

func renderNode(name, datacenter string, role v1alpha1.Role, bootstrapExpect int) ([]byte, error) {
	if err := checkHostName(name); err != nil {
		return nil, err
	}
	var p problems
	n := p.quote("name", name)
	dc := p.quoteSet("datacenter", datacenter)
	if p.err != nil {
		return nil, p.err
	}
	if strings.Contains(datacenter, "*") {
		return nil, fmt.Errorf(`datacenter %q has "*", which Nomad refuses`, datacenter)
	}
	if err := checkRole(role); err != nil {
		return nil, err
	}
	content := header + "name       = " + n + "\ndatacenter = " + dc + "\n"
	if role.RunsServer() {
		if bootstrapExpect < 0 {
			return nil, fmt.Errorf("bootstrap_expect %d is less than 0", bootstrapExpect)
		}
		if bootstrapExpect > 0 {
			content += fmt.Sprintf("\nserver {\n  bootstrap_expect = %d\n}\n", bootstrapExpect)
		}
	}
	return []byte(content), nil
}

// RenderInstance returns 11-instance.hcl, which gives a client the id of its cloud instance as the meta
// tent_instance_id. tent-node reads the id from the cloud's metadata service. The id must not be empty and must be an
// HCL1 string.
func RenderInstance(id string) (File, error) {
	var p problems
	quoted := p.quoteSet("instance id", id)
	if p.err != nil {
		return File{}, fmt.Errorf("11-instance.hcl: %w", p.err)
	}
	return perNodeFile(instancePath,
		[]byte(NodeHeader+"client {\n  meta {\n    \"tent_instance_id\" = "+quoted+"\n  }\n}\n")), nil
}
