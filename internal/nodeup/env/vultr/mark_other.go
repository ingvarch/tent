//go:build !linux

package vultr

import "syscall"

// markSocket is nil, so New's dialer marks no socket: only Linux has SO_MARK, and tent-node runs only on Linux.
var markSocket func(network, address string, c syscall.RawConn) error
