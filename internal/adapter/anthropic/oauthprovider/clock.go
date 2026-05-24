package oauthprovider

import "time"

// nowFunc supplies the current wall-clock time. It is a field on Provider so
// tests can substitute a fixed clock; production wires it to [time.Now] (passed
// as a function value, never called here, so it stays testable).
type nowFunc func() time.Time
