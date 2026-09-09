// clock.go provides a testable time abstraction layer, decoupling business
// logic from a direct dependency on system time.
//
// This file contains:
//   - Clock interface: abstracts time.Now() calls to improve code testability
//   - RealClock: production implementation that returns the real system time
//   - FakeClock: test implementation that supports manually advancing time,
//     making it easy to simulate timeout and scheduled scenarios
//
// Usage scenarios:
//   - In business code, obtain time through the Clock interface instead of
//     calling time.Now() directly
//   - In unit tests, inject FakeClock to precisely control time progression
//     and verify timeout logic
//   - Use the Clock interface in scheduled task schedulers to support fast
//     verification in test environments
package clock

import "time"

// The Clock interface abstracts time.Now() calls to improve code testability.
// Implementers provide time retrieval capability; business code calls the
// interface instead of depending on the time package directly.
//
// Production: use RealClock to return the real system time.
// Testing: use FakeClock to return a fixed, manually controllable time,
// making it easy to simulate time-progression scenarios.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
}

// RealClock is the production implementation of the Clock interface; it
// returns the real system time.
type RealClock struct{}

// Now returns the current system time.
func (RealClock) Now() time.Time { return time.Now() }

// FakeClock is the test implementation of the Clock interface; it returns a
// fixed time that can be advanced manually.
// It is used in unit tests to simulate time-related behavior such as
// timeouts and scheduled task triggers.
type FakeClock struct {
	fixed time.Time
}

// NewFakeClock creates a FakeClock instance set to the specified time.
//
// Parameters:
//   - t: the initial time of the fake clock
//
// Returns:
//   - *FakeClock: the initialized fake clock instance
func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{fixed: t}
}

// Now returns the time currently set on the fake clock; it does not change
// with the system clock.
func (f *FakeClock) Now() time.Time { return f.fixed }

// Advance advances the fake clock forward by the specified duration to
// simulate the passage of time.
//
// Parameters:
//   - d: the duration to advance; negative values are supported (rewinding time)
func (f *FakeClock) Advance(d time.Duration) {
	f.fixed = f.fixed.Add(d)
}
