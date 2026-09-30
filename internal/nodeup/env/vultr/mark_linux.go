//go:build linux

package vultr

import (
	"fmt"
	"syscall"

	"github.com/ingvarch/tent/internal/nodeup/env"
)

// setsockoptInt sets an integer option of a socket; tests replace it.
var setsockoptInt = syscall.SetsockoptInt

// markSocket is the Control of New's dialer: it sets SO_MARK of the socket to env.MetadataMark before it connects, so
// that the host firewall lets it reach the metadata service. Setting the mark needs CAP_NET_ADMIN or CAP_NET_RAW.
func markSocket(_, _ string, c syscall.RawConn) error {
	var err error
	if cerr := c.Control(func(fd uintptr) {
		err = setsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, env.MetadataMark)
	}); cerr != nil {
		return fmt.Errorf("mark the socket for the metadata service: %w", cerr)
	}
	if err != nil {
		return fmt.Errorf("mark the socket for the metadata service: %w (tent-node needs CAP_NET_ADMIN or CAP_NET_RAW)",
			err)
	}
	return nil
}
