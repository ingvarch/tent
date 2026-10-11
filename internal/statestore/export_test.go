package statestore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gofrs/flock"
)

// Unexported functions and values, for tests.
var (
	SyncDir             = syncDir
	ParentsBelowRoot    = parentsBelowRoot
	OwnerName           = ownerName
	NewLeaseLocker      = newLeaseLocker
	NewBestEffortLocker = newBestEffortLocker
)

// HeldFlocks returns the handles of the flock locks that are held.
func HeldFlocks() []*flock.Flock {
	var held []*flock.Flock
	heldFlocks.Range(func(h, _ any) bool {
		held = append(held, h.(*flock.Flock))
		return true
	})
	return held
}

// ReleaseFlock releases a held flock lock as the end of its holder's process would: it closes the handle and forgets
// it, and leaves the lease file. The locker that held it must not be used again.
func ReleaseFlock(h *flock.Flock) error {
	heldFlocks.Delete(h)
	return h.Unlock()
}

// ThrottleAttempts is how often an s3 store sends a request that the server keeps throttling.
const ThrottleAttempts = throttleAttempts

// SetThrottleWait sets the s3 stores' wait after a throttled request until the test ends. Tests that call it must
// not run in parallel.
func SetThrottleWait(t *testing.T, d time.Duration) {
	old := throttleWait
	throttleWait = d
	t.Cleanup(func() { throttleWait = old })
}

// PurgeS3 deletes every object below the prefix of an s3 store that Open returned, the ones List hides too. It
// refuses a store without a prefix: that would empty the bucket.
func PurgeS3(ctx context.Context, s Store) error {
	c, ok := s.(checkedStore)
	if !ok {
		return errors.New("not a store that Open returned")
	}
	b, ok := c.backend.(*s3Store)
	switch {
	case !ok:
		return errors.New("not an s3 store")
	case b.prefix == "":
		return errors.New("the store has no prefix: purging it would empty the bucket")
	}
	keys, err := b.keys(ctx, b.prefix+"/")
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := b.deleteKey(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// SetS3HTTPClient makes an s3 store that Open returned send its requests through c.
func SetS3HTTPClient(s Store, c s3.HTTPClient) {
	b := s.(checkedStore).backend.(*s3Store)
	b.client = s3.New(b.client.Options(), func(o *s3.Options) { o.HTTPClient = c })
}

// SyncLog records the syncs of a file store: "file" for a synced file and "dir" for a synced directory, in order.
type SyncLog struct {
	mu     sync.Mutex
	events []string
}

func (l *SyncLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

// Take returns the syncs recorded since the last call and forgets them.
func (l *SyncLog) Take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	events := l.events
	l.events = nil
	return events
}

// RecordSyncs makes a file store that Open or OpenWith returned record each sync, which still runs.
func RecordSyncs(t *testing.T, s Store) *SyncLog {
	t.Helper()
	f, ok := fileStoreOf(s)
	if !ok {
		t.Fatalf("%T is not a file store that Open or OpenWith returned", s)
	}
	log := &SyncLog{}
	f.onSync = log.add
	return log
}
