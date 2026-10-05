package nodeup

import (
	"context"
	"time"
)

// LockPath is the file whose lock serializes up and refresh-join, so that a refresh-join run by hand never changes
// the machine while up runs. /run is a tmpfs, so a reboot leaves no stale lock.
const LockPath = "/run/tent-node.lock"

// Lock takes the lock at LockPath and returns how to leave it. A caller that finds it taken waits for it, once a
// second, until the context ends.
func Lock(ctx context.Context) (func(), error) {
	return lock(ctx, LockPath, time.Second)
}
