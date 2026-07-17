// Package ports declares provider interfaces at the dependency inversion
// boundary. Core business logic depends on these; adapters implement them.
// Ports must NOT import adapters or any third-party SDK.
package ports

import "time"

// Clock abstracts time.Now() so tests can freeze / advance the clock.
// The zero value RealClock{} delegates to time.Now.
type Clock interface {
	Now() time.Time
}

// RealClock returns wall-clock time. Default choice in production.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }
