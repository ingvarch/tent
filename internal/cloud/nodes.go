package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
)

// Nodes are the machine primitives of a provider. Drain, quorum and the order of replacements live in the core.
type Nodes interface {
	// List returns the machines of the cluster.
	List(ctx context.Context, cluster string) ([]Instance, error)
	// Create creates a machine and waits until it is ready. The wait has no deadline of its own, so the caller gives
	// ctx one. A call with the operation id of an earlier call returns the machine that the earlier call created, if
	// the cloud lists it, instead of creating another.
	Create(ctx context.Context, req CreateRequest) (Instance, error)
	// Stop powers the machine off. Where the cloud has no graceful shutdown, it is a hard power-off. A machine that is
	// gone counts as stopped.
	Stop(ctx context.Context, node Instance) error
	// Delete destroys the machine, even while it runs. A machine that is gone counts as deleted.
	Delete(ctx context.Context, node Instance) error
	// MarkJoined records on the machine that its node has joined the cluster, and replaces the machine's user data,
	// which holds secrets, with a stub that holds none where the cloud lets user data change, so that the cloud's
	// metadata service stops serving them. The core calls it once the node has joined. It is safe to repeat. A machine
	// that is gone counts as marked.
	MarkJoined(ctx context.Context, node Instance) error
	// MarkReplace records on the machine that a rolling update replaces it, so that a later run replaces it too. It
	// changes nothing else. It is safe to repeat. A machine that is gone counts as marked.
	MarkReplace(ctx context.Context, node Instance) error
}

// Instance is one machine of a cluster as the cloud reports it.
type Instance struct {
	ID        string        // the cloud's id of the machine
	Name      string        // the machine name, <cluster>-<group>-<index>, which is also the hostname
	Cluster   string        // the cluster that owns it
	Group     string        // its node group
	Role      v1alpha1.Role // the Nomad role of its node group
	Zone      string        // the failure domain it runs in, such as ams
	SpecHash  string        // the hash of the node configuration it was created with; empty when it carries none
	Op        string        // the operation id of the call that created it
	PrivateIP netip.Addr    // its address in the cluster's network; the invalid Addr until the cloud reports one
	PublicIP  netip.Addr    // its public IPv4 address; the invalid Addr until the cloud reports one
	Ready     bool          // the cloud reports it running and booted
	Joined    bool          // it carries the label LabelJoined with the value true
	Replace   bool          // it carries the label LabelReplace with the value true
	Created   time.Time     // when the cloud created it; the zero time when the cloud gives none that parses
}

// CreateRequest is one machine to create.
type CreateRequest struct {
	Cluster     string        // the cluster the machine joins
	Group       string        // its node group
	Role        v1alpha1.Role // the Nomad role of the node group: server, client or combined
	Zone        string        // the failure domain to create it in
	Name        string        // the machine name, <cluster>-<group>-<index>, which is also the hostname
	MachineType string        // the provider's plan or server type
	Image       string        // the operating system image by name, such as ubuntu-24.04
	SpecHash    string        // the hash of the node configuration; empty for none
	// Op is the operation id of the call, a lower-case UUID of version 4 as NewOpID makes. A call with the op of an
	// earlier one finds the machine that the earlier call created.
	Op       string
	UserData UserData // what cloud-init reads on the first boot
}

// UserData is what cloud-init reads on a machine's first boot. It may hold secrets, so it never prints: fmt with any
// verb, slog and encoding/json show only its size, such as [user data, 1234 bytes]. Convert it to []byte or string
// only to send it to the cloud. A go-cmp diff of two values that differ shows their bytes in a failed test, so tests
// with real secrets compare user data by a hash or with a comparer.
type UserData []byte

// String returns the size of the user data, such as [user data, 1234 bytes], and never its content.
func (u UserData) String() string { return fmt.Sprintf("[user data, %d bytes]", len(u)) }

// GoString returns what String does, so that %#v shows no content either.
func (u UserData) GoString() string { return u.String() }

// Format writes what String returns, whatever the verb, width and flags.
func (u UserData) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, u.String()) }

// LogValue makes slog log what String returns.
func (u UserData) LogValue() slog.Value { return slog.StringValue(u.String()) }

// MarshalJSON writes what String returns as a JSON string.
func (u UserData) MarshalJSON() ([]byte, error) { return json.Marshal(u.String()) }

// Validate checks that the request has what every provider needs: a cluster, group, zone, name, machine type, image
// and operation id, an operation id of the form that NewOpID makes, and the role server, client or combined. It
// returns the first problem it finds.
func (r CreateRequest) Validate() error {
	for _, f := range []struct{ name, value string }{
		{"cluster", r.Cluster}, {"group", r.Group}, {"zone", r.Zone}, {"name", r.Name},
		{"machine type", r.MachineType}, {"image", r.Image}, {"operation id", r.Op},
	} {
		if f.value == "" {
			return fmt.Errorf("create request: no %s", f.name)
		}
	}
	if !ValidOpID(r.Op) {
		return fmt.Errorf("create request: operation id %q is not one that NewOpID makes "+
			"(a lower-case UUID of version 4)", r.Op)
	}
	if !slices.Contains(v1alpha1.Roles(), r.Role) {
		return fmt.Errorf("create request: role %q is not server, client or combined", r.Role)
	}
	return nil
}
