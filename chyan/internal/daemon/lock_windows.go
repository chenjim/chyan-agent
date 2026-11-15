//go:build windows

package daemon

import "os"

// acquireHomeLock is a no-op on Windows: there is no unix socket to unlink, and
// the TCP listener already refuses a second bind on the same port.
func acquireHomeLock(string) (*os.File, error) { return nil, nil }
