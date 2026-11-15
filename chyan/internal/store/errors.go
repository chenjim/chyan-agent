// Shared sentinel errors for store implementations.
package store

import "errors"

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("store: not found")
