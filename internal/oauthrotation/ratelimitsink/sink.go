// Package ratelimitsink defines the provider-agnostic contract a provider's
// client uses to report an observed rate-limit to the rotation layer. The
// rotation layer implements Sink; provider clients call Throttle when they see
// an upstream rate-limit response. This package imports no provider package.
package ratelimitsink

import (
	"context"
	"time"
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
// account whose slot should be throttled (reverse-looked-up by the rotation
// layer), Claim names the window, ResetAt is when the window is expected to
// clear, ObservedAt is when the client saw the signal, and HTTPStatus is the
// upstream status code that carried it.
type Signal struct {
	Provider    string
	AccessToken string
	Claim       Claim
	ResetAt     time.Time
	ObservedAt  time.Time
	HTTPStatus  int
}

// Sink receives rate-limit signals from provider clients. The rotation layer
// implements it by throttling the account behind the reported access token.
type Sink interface {
	// Throttle records that the account behind sig.AccessToken is rate-limited
	// until sig.ResetAt. It returns an error only when the signal cannot be
	// persisted.
	Throttle(ctx context.Context, sig Signal) error
}
