package vultr

import (
	"context"
	"net"
	"time"
)

// NewForTest returns an Environment that connects through dial, gives each try at most try, and waits wait(n) after
// failed try n, counting from 1, for tests.
func NewForTest(dial func(ctx context.Context, network, addr string) (net.Conn, error), try time.Duration,
	wait func(n int) time.Duration,
) *Environment {
	return newEnvironment(dial, try, wait)
}

// Defaults returns how long New gives each try and how long it waits after the first failed one, for tests.
func Defaults() (try, wait time.Duration) {
	e := New()
	return e.try, e.wait(1)
}
