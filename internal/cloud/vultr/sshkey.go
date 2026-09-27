package vultr

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/model"
)

// sshKey is a public key in OpenSSH form: <type> <base64> [comment].
type sshKey struct {
	line string // the whole key, without the space around it
	typ  string // the key type, such as ssh-ed25519
	data string // the decoded key data
	fp   string // the first 8 lower-case hex digits of the SHA-256 digest of the key data
}

// parseSSHKey reads a public key in OpenSSH form.
func parseSSHKey(line string) (sshKey, error) {
	line = strings.TrimSpace(line)
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return sshKey{}, errors.New("not an OpenSSH public key: <type> <base64> [comment]")
	}
	data, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return sshKey{}, errors.New("the key data is not valid base64")
	}
	sum := sha256.Sum256(data)
	return sshKey{line: line, typ: fields[0], data: string(data), fp: hex.EncodeToString(sum[:4])}, nil
}

// sameKey reports whether k and o are the same key: the same type and key data, whatever their comments.
func (k sshKey) sameKey(o sshKey) bool { return k.typ == o.typ && k.data == o.data }

// sshKeyTask makes one public key of the spec an SSH key of the Vultr account. The key's name is its marker, and its
// key material the spec's key.
type sshKeyTask struct {
	api      API
	cluster  string
	key      sshKey
	op       string  // the operation id that the create puts into the marker
	attempts opState // what the attempts of the create share
}

// sshKeyTasks returns a task for each public key of m, in m's order. Keys with the same type and key data are one
// task. A key that does not parse is an error, and so are two keys whose fingerprints start with the same 8 hex
// digits, since the keys of their tasks would be the same.
func (p *Provider) sshKeyTasks(m *model.Cluster) ([]engine.Task, error) {
	var tasks []engine.Task
	keys := make([]sshKey, len(m.SSHKeys))
	first := map[string]int{} // the index of the first key with each fp
	for i, line := range m.SSHKeys {
		k, err := parseSSHKey(line)
		if err != nil {
			return nil, fmt.Errorf("cluster %s: spec.sshKeys[%d] %q: %w", m.Name, i, line, err)
		}
		keys[i] = k
		j, seen := first[k.fp]
		switch {
		case !seen:
			first[k.fp] = i
			tasks = append(tasks, &sshKeyTask{api: p.api, cluster: m.Name, key: k, op: p.opID()})
		case !keys[j].sameKey(k):
			return nil, fmt.Errorf("cluster %s: spec.sshKeys[%d] %q and spec.sshKeys[%d] %q have fingerprints that "+
				"start with the same %s, which tent names Vultr SSH keys by", m.Name, j, m.SSHKeys[j], i, line, k.fp)
		}
	}
	return tasks, nil
}

// Key returns vultr.SSHKey/<cluster>-<fp>.
func (t *sshKeyTask) Key() engine.Key { return sshKeyKey(t.cluster, t.key.fp) }

// Deps returns nothing: an SSH key needs no other object.
func (t *sshKeyTask) Deps() []engine.Key { return nil }

// Plan plans a create when the snapshot has no SSH key for the task. When it has one, Plan sets the output id; it
// fails when that key holds other key material than the spec's.
func (t *sshKeyTask) Plan(_ context.Context, env *engine.Env) (engine.Change, error) {
	s, err := snapshotOf(env)
	if err != nil {
		return engine.Change{}, err
	}
	cur, ok := s.sshKey(t.Key())
	if !ok {
		return engine.Change{Action: engine.Create}, nil
	}
	if k, err := parseSSHKey(cur.SSHKey); err != nil || !k.sameKey(t.key) {
		return engine.Change{}, fmt.Errorf("the Vultr SSH key %s carries the marker of the spec's key %q but holds "+
			"other key material; delete it in Vultr, and tent creates the spec's key", cur.ID, t.key.line)
	}
	env.Outputs.Set(t.Key(), outputID, cur.ID)
	return engine.Change{Action: engine.Noop}, nil
}

// Apply creates the SSH key and sets the output id. Plan plans no other change.
func (t *sshKeyTask) Apply(ctx context.Context, env *engine.Env, _ engine.Change) error {
	id, err := createWithOp(ctx, &t.attempts, opFinder(t.api.ListSSHKeys, sshKeyType, t.cluster, t.op), t.createKey)
	if err != nil {
		return err
	}
	env.Outputs.Set(t.Key(), outputID, id)
	return nil
}

// createKey sends the create of the SSH key and returns the key's id.
func (t *sshKeyTask) createKey(ctx context.Context) (string, error) {
	name := Marker{Cluster: t.cluster, Kind: KindSSHKey, Fingerprint: t.key.fp, Op: t.op}.String()
	k, err := t.api.CreateSSHKey(ctx, &govultr.SSHKeyReq{Name: name, SSHKey: t.key.line})
	if err != nil {
		return "", err
	}
	return k.ID, nil
}

// Delete deletes the SSH key obj. A key that is gone counts as deleted.
func (t *sshKeyTask) Delete(ctx context.Context, _ *engine.Env, obj engine.Object) error {
	return deleted(t.api.DeleteSSHKey(ctx, obj.ID))
}
