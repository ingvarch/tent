package vultr_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/engine/enginetest"
	"github.com/ingvarch/tent/internal/model"
)

// Public keys of the tests, each with the first 8 hex digits of its SHA-256 digest, which ssh-keygen -l prints in
// base64 (SHA256:…).
const (
	opsKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILVMgcq7nf63leSBwZNfB40Oi4XwSKWNKchNmRGNCb9k ops@example"
	opsFP  = "8ba890ed"
	devKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINrldaS6ZmTkyJJJE5myvCxlWGxGxN3eRFaW+YJPfMqb dev@example"
	devFP  = "ae2316fa"
	ciKey  = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBN3p1Y7Kx0T9WaiKluzGTGVc+AYnZHJGq" +
		"jnI80zgp202abTI5tDvpIwDkaqrJpXTv4k7COXFOBi6pDtTKWfZ/yI= ci@example"
	ciFP = "14fb2110"
)

// Two keys whose fingerprints start with the same 8 hex digits, collidingFP.
const (
	collidingKeyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAUH a@example"
	collidingKeyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACOe b@example"
	collidingFP   = "6ccfce59"
)

// sshKeyOf returns the key of cluster prod's SSH key whose fingerprint starts with fp.
func sshKeyOf(fp string) engine.Key { return engine.Key{Kind: "vultr.SSHKey", Name: "prod-" + fp} }

// sshKeyName returns the name that tent gives cluster prod's SSH key whose fingerprint starts with fp, when the
// create carries the operation id op.
func sshKeyName(fp, op string) string { return "tent:cluster=prod;kind=ssh-key;fp=" + fp + ";op=" + op }

// newSSHKeyFixture returns the SSH key tasks of cluster prod with keys, on an empty fake. The provider's operation ids
// are op-1, op-2 and so on.
func newSSHKeyFixture(t *testing.T, keys ...string) *fixture {
	t.Helper()
	x := newFixture()
	tasks, err := infraTasks(t, x.p, model.Cluster{Name: "prod", SSHKeys: keys}, "vultr.SSHKey")
	if err != nil {
		t.Fatalf("BuildInfra: %v", err)
	}
	x.tasks, x.kinds = tasks, x.p.InfraKinds()
	return x
}

// wantSSHKeys checks the names and key material of the fake's SSH keys, in any order.
func wantSSHKeys(t *testing.T, f *vultrfake.Fake, want ...govultr.SSHKey) {
	t.Helper()
	opts := cmp.Options{
		cmpopts.IgnoreFields(govultr.SSHKey{}, "ID", "DateCreated"),
		cmpopts.SortSlices(func(a, b govultr.SSHKey) bool { return a.Name < b.Name }),
		cmpopts.EquateEmpty(),
	}
	if diff := cmp.Diff(want, f.SSHKeys(), opts); diff != "" {
		t.Errorf("SSH keys (-want +got):\n%s", diff)
	}
}

// keysOf returns the keys of tasks.
func keysOf(tasks []engine.Task) []engine.Key {
	var keys []engine.Key
	for _, t := range tasks {
		keys = append(keys, t.Key())
	}
	return keys
}

func TestSSHKeyTasks(t *testing.T) {
	sameAsOps := strings.Join(strings.Fields(opsKey)[:2], " ") + " laptop" // another comment
	p, _ := newProvider(vultrfake.New())
	tasks, err := infraTasks(t, p, model.Cluster{Name: "prod", SSHKeys: []string{opsKey, ciKey, sameAsOps}},
		"vultr.SSHKey")
	if err != nil {
		t.Fatalf("BuildInfra: %v", err)
	}
	// One task for each key in the spec's order; the same type and key data with another comment is the same key.
	if diff := cmp.Diff([]engine.Key{sshKeyOf(opsFP), sshKeyOf(ciFP)}, keysOf(tasks)); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
	for _, task := range tasks {
		if deps := task.Deps(); len(deps) != 0 {
			t.Errorf("%s depends on %v, want nothing", task.Key(), deps)
		}
	}
}

func TestSSHKeyTasksNone(t *testing.T) {
	p, _ := newProvider(vultrfake.New())
	tasks, err := infraTasks(t, p, model.Cluster{Name: "prod"}, "vultr.SSHKey")
	if err != nil || len(tasks) != 0 {
		t.Errorf("BuildInfra = %v, %v; want no SSH key tasks", keysOf(tasks), err)
	}
}

func TestSSHKeyTasksErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		want string
	}{
		{
			"no key data",
			[]string{opsKey, "ssh-ed25519"},
			`cluster prod: spec.sshKeys[1] "ssh-ed25519": not an OpenSSH public key: <type> <base64> [comment]`,
		},
		{
			"key data that is not base64",
			[]string{"ssh-ed25519 !!!"},
			`cluster prod: spec.sshKeys[0] "ssh-ed25519 !!!": the key data is not valid base64`,
		},
		{
			"two keys with the same fingerprint start",
			[]string{collidingKeyA, collidingKeyB},
			fmt.Sprintf("cluster prod: spec.sshKeys[0] %q and spec.sshKeys[1] %q have fingerprints that start with "+
				"the same %s, which tent names Vultr SSH keys by", collidingKeyA, collidingKeyB, collidingFP),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newProvider(vultrfake.New())
			tasks, err := infraTasks(t, p, model.Cluster{Name: "prod", SSHKeys: tc.keys}, "vultr.SSHKey")
			if err == nil || err.Error() != tc.want {
				t.Errorf("BuildInfra = %v, %v; want the error %q", keysOf(tasks), err, tc.want)
			}
		})
	}
}

func TestSSHKeyTasksApplyReplan(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		want []govultr.SSHKey
	}{
		{
			// The key material is the spec's line without the space around it.
			name: "one key",
			keys: []string{"  " + opsKey + " "},
			want: []govultr.SSHKey{{Name: sshKeyName(opsFP, "op-1"), SSHKey: opsKey}},
		},
		{
			name: "two keys",
			keys: []string{opsKey, ciKey},
			want: []govultr.SSHKey{
				{Name: sshKeyName(opsFP, "op-1"), SSHKey: opsKey},
				{Name: sshKeyName(ciFP, "op-2"), SSHKey: ciKey},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newSSHKeyFixture(t, tc.keys...)
			enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
			wantSSHKeys(t, x.f, tc.want...)
			if got := countCalls(x.f, "CreateSSHKey"); got != len(tc.want) {
				t.Errorf("%d CreateSSHKey calls, want %d", got, len(tc.want))
			}
		})
	}
}

func TestSSHKeyTaskOutputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		lose bool // the create's answer is lost
	}{
		{name: "created"},
		{name: "adopted after a lost answer", lose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newSSHKeyFixture(t, opsKey)
			if tc.lose {
				x.f.LoseResponse(t, "CreateSSHKey", 1)
			}
			id := x.createdID(t)
			if keys := x.f.SSHKeys(); len(keys) != 1 || id != keys[0].ID {
				t.Errorf("output id = %q, and the fake holds %+v; want the id of its one SSH key", id, keys)
			}
		})
	}
}

func TestSSHKeyTaskAdoptsAnExistingKey(t *testing.T) {
	x := newSSHKeyFixture(t, opsKey)
	// The same key with another comment, which an earlier run created.
	seeded := x.f.AddSSHKey(t, govultr.SSHKey{
		Name: sshKeyName(opsFP, "op-earlier"), SSHKey: strings.Join(strings.Fields(opsKey)[:2], " ") + " laptop",
	})
	x.wantAdopted(t, seeded.ID, "CreateSSHKey")
	wantSSHKeys(t, x.f, seeded)
}

func TestSSHKeyTaskPrunesARemovedKey(t *testing.T) {
	x := newSSHKeyFixture(t, opsKey)
	ops := x.f.AddSSHKey(t, govultr.SSHKey{Name: sshKeyName(opsFP, "op-a"), SSHKey: opsKey})
	dev := x.f.AddSSHKey(t, govultr.SSHKey{Name: sshKeyName(devFP, "op-b"), SSHKey: devKey})
	enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
	wantSSHKeys(t, x.f, ops)
	if !slices.Contains(x.f.Calls(), vultrfake.Call{Name: "DeleteSSHKey", Arg: dev.ID}) {
		t.Errorf("calls %+v, want a DeleteSSHKey of %s", x.f.Calls(), dev.ID)
	}
}

func TestSSHKeyTaskLostCreate(t *testing.T) {
	x := newSSHKeyFixture(t, opsKey)
	x.f.LoseResponse(t, "CreateSSHKey", 1)
	enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
	wantSSHKeys(t, x.f, govultr.SSHKey{Name: sshKeyName(opsFP, "op-1"), SSHKey: opsKey})
	if got := countCalls(x.f, "CreateSSHKey"); got != 1 {
		t.Errorf("%d CreateSSHKey calls, want 1", got)
	}
}

func TestSSHKeyTaskLostCreateAndFailedSearch(t *testing.T) {
	x := newSSHKeyFixture(t, opsKey)
	// The second attempt searches, finds the key and does not create it again.
	name := sshKeyName(opsFP, "op-1")
	x.wantSearchAfterLostCreate(t, vultrfake.Call{Name: "CreateSSHKey", Arg: name}, "ListSSHKeys", "/v2/ssh-keys")
	wantSSHKeys(t, x.f, govultr.SSHKey{Name: name, SSHKey: opsKey})
}

func TestSSHKeyTaskThrottledCreate(t *testing.T) {
	x := newSSHKeyFixture(t, opsKey)
	events := x.applyWithFaults(t, func(tb testing.TB) { x.f.Throttle(tb, "CreateSSHKey", 7*time.Second, 1) })
	i := slices.IndexFunc(events, func(e engine.Event) bool { return e.Type == engine.Retrying })
	switch {
	case i < 0:
		t.Errorf("the engine did not retry the throttled create; events: %+v", events)
	case events[i].Wait != 7*time.Second:
		t.Errorf("the engine waited %v before the retry, want the server's 7s", events[i].Wait)
	}
	// Vultr did not carry out the throttled create, so the retry creates without a search.
	name := sshKeyName(opsFP, "op-1")
	wantCalls(t, x.f, slices.Concat(listCalls, []vultrfake.Call{
		{Name: "CreateSSHKey", Arg: name}, {Name: "CreateSSHKey", Arg: name},
	}, listCalls)...)
	wantSSHKeys(t, x.f, govultr.SSHKey{Name: name, SSHKey: opsKey})
}

func TestSSHKeyTaskOtherKeyMaterial(t *testing.T) {
	for _, tc := range []struct {
		name     string
		material string // of the key in Vultr that carries the marker of the spec's key
	}{
		{"a fingerprint collision", collidingKeyA},
		{"key material that does not parse", "not a key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newSSHKeyFixture(t, collidingKeyB)
			x.f.AddSSHKey(t, govultr.SSHKey{ID: "key-a", Name: sshKeyName(collidingFP, "op-a"), SSHKey: tc.material})
			_, err := x.plan(t)
			want := fmt.Sprintf("plan vultr.SSHKey/prod-%s: the Vultr SSH key key-a carries the marker of the spec's "+
				"key %q but holds other key material; delete it in Vultr, and tent creates the spec's key",
				collidingFP, collidingKeyB)
			if err == nil || err.Error() != want {
				t.Errorf("NewPlan error = %v, want %q", err, want)
			}
		})
	}
}

// otherSnapshot is a snapshot of another provider.
type otherSnapshot struct{}

func (otherSnapshot) Objects() []engine.Object { return nil }

func TestSSHKeyTaskForeignSnapshot(t *testing.T) {
	x := newSSHKeyFixture(t, opsKey)
	_, err := engine.NewPlan(t.Context(), x.tasks, x.kinds, otherSnapshot{})
	want := "plan vultr.SSHKey/prod-" + opsFP + ": the snapshot is a vultr_test.otherSnapshot, not a Vultr snapshot"
	if err == nil || err.Error() != want {
		t.Errorf("NewPlan error = %v, want %q", err, want)
	}
}

func TestSSHKeyTaskNoSnapshot(t *testing.T) {
	x := newSSHKeyFixture(t, opsKey)
	p, err := engine.NewPlan(t.Context(), x.tasks, x.kinds, nil)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	want := []engine.PlannedChange{{Key: sshKeyOf(opsFP), Change: engine.Change{Action: engine.Create}}}
	if diff := cmp.Diff(want, p.Changes()); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
}

func TestSSHKeyDeleteOfAGoneKey(t *testing.T) {
	f := vultrfake.New()
	p, _ := newProvider(f)
	obj := engine.Object{Key: sshKeyOf(opsFP), ID: "ssh-key-9"}
	if err := deleterOf(t, p, "vultr.SSHKey").Delete(t.Context(), &engine.Env{}, obj); err != nil {
		t.Errorf("Delete of a key that is gone: %v, want success", err)
	}
	wantCalls(t, f, vultrfake.Call{Name: "DeleteSSHKey", Arg: "ssh-key-9"})
}
