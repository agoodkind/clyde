// Package ratelimitsink defines the provider-agnostic contract a provider's
// client uses to report an observed rate-limit response to the rotation
// layer. The rotation layer implements Sink; provider clients call Observe
// after every response so the rotator can update the matching account's
// in-memory quota state. This package imports no provider package.
package ratelimitsink

import (
	"context"
	"time"
)

// Status is the wire value carried in the
// anthropic-ratelimit-unified-status header (or its equivalent on another
// provider). The rotator stores it on the account slot so selection can skip
// a slot that was rejected on its last observation.
type Status string

const (
	// StatusUnknown marks a response whose status header was missing or not
	// one of the known values. The rotator treats it as "no signal", neither
	// clearing nor setting a rejection.
	StatusUnknown Status = ""
	// StatusAllowed is the healthy state.
	StatusAllowed Status = "allowed"
	// StatusAllowedWarning is the healthy-with-warning state.
	StatusAllowedWarning Status = "allowed_warning"
	// StatusRejected is the refused state.
	StatusRejected Status = "rejected"
)

// Claim names the rate-limit window a signal belongs to. The provider client
// classifies the upstream response into one of these before reporting it.
type Claim string

const (
	// ClaimFiveHour is the rolling five-hour usage window.
	ClaimFiveHour Claim = "five_hour"
	// ClaimSevenDay is the rolling seven-day usage window.
	ClaimSevenDay Claim = "seven_day"
	// ClaimSevenDayOpus is the seven-day window scoped to Opus usage.
	ClaimSevenDayOpus Claim = "seven_day_opus"
	// ClaimUnknown marks a signal whose window could not be classified.
	ClaimUnknown Claim = "unknown"
)

// Signal is one observed rate-limit event reported by a provider client.
// Provider is the reporting provider's name, AccessToken identifies the
// account whose slot should be updated (reverse-looked-up by the rotation
// layer), Status is the unified-status header value, Claim names the window,
// ResetAt is when the window is expected to clear, ObservedAt is when the
// client saw the signal, and HTTPStatus is the upstream status code that
// carried it.
type Signal struct {
	Provider    string
	AccessToken string
	Status      Status
	Claim       Claim
	ResetAt     time.Time
	ObservedAt  time.Time
	HTTPStatus  int
}

// Sink receives rate-limit observations from provider clients. The rotation
// layer implements it by updating the in-memory quota state of the account
// behind the reported access token.
type Sink interface {
	// Observe records the upstream rate-limit shape for the account behind
	// sig.AccessToken. It is called on every response, not only on 429.
	Observe(ctx context.Context, sig Signal) error
}
