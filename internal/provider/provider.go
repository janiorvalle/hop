// Package provider defines the normalized data returned by provider adapters.
package provider

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Name identifies a supported provider.
type Name string

const (
	Claude Name = "claude"
	Codex  Name = "codex"
)

// WindowKind identifies a usage window by duration, not API response position.
type WindowKind string

const (
	FiveHour WindowKind = "five_hour"
	Weekly   WindowKind = "weekly"
)

// Window is one normalized usage meter.
type Window struct {
	Kind        WindowKind `json:"kind"`
	UsedPercent float64    `json:"used_percent"`
	ResetsAt    time.Time  `json:"resets_at,omitzero"`
}

// Limit is a provider-specific cap, including model-scoped binding limits.
type Limit struct {
	Kind        string    `json:"kind"`
	Group       string    `json:"group"`
	UsedPercent float64   `json:"used_percent"`
	Severity    string    `json:"severity"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
	Scope       string    `json:"scope,omitempty"`
	Active      bool      `json:"active"`
}

// ResetCredit is one manual quota reset an account can spend before it expires.
type ResetCredit struct {
	GrantedAt time.Time `json:"granted_at,omitzero"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ResetCredits is the manual resets an account has available right now.
type ResetCredits struct {
	Count   int           `json:"count"`
	Credits []ResetCredit `json:"credits"`
}

// NoResetCredits is the value for an account that has no credit left to spend.
func NoResetCredits() ResetCredits {
	return ResetCredits{Credits: make([]ResetCredit, 0)}
}

// SoonestExpiry returns the earliest expiry among the credits, if any carries one.
func (credits ResetCredits) SoonestExpiry() (time.Time, bool) {
	var soonest time.Time
	for _, credit := range credits.Credits {
		if credit.ExpiresAt.IsZero() {
			continue
		}
		if soonest.IsZero() || credit.ExpiresAt.Before(soonest) {
			soonest = credit.ExpiresAt
		}
	}
	return soonest, !soonest.IsZero()
}

// UsageHTTPAction gives the caller a status-specific next step.
func UsageHTTPAction(providerName Name, statusCode int) string {
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return fmt.Sprintf("run 'hop login %s <account>' and retry", providerName)
	case statusCode == http.StatusTooManyRequests:
		return "the provider rate-limited the request, wait and retry 'hop ls'"
	case statusCode >= http.StatusInternalServerError:
		return "the provider usage service is unavailable, retry 'hop ls' later"
	default:
		return "retry 'hop ls', and update hop if this response persists"
	}
}

// Usage is what the provider's usage endpoint said about one account.
type Usage struct {
	Provider Name   `json:"provider"`
	Email    string `json:"email,omitempty"`
	// Plan is set only by providers whose usage endpoint names the plan; the
	// enrollment carries the plan the credentials were issued for.
	Plan    string   `json:"plan,omitempty"`
	Windows []Window `json:"windows"`
	Limits  []Limit  `json:"limits"`
	// ResetCredits is nil when the provider has no manual resets or the count is unknown.
	ResetCredits *ResetCredits `json:"reset_credits,omitempty"`
}

// Enrollment is what stored credentials say about an account before any
// request goes out. It outlives a failed usage fetch.
type Enrollment struct {
	Plan string
	// RefreshTokenExpiresAt is zero when the provider never said.
	RefreshTokenExpiresAt time.Time
}

// Fetcher is one account's credentials bound to their provider: what they say
// up front, and the usage request they authorize.
type Fetcher interface {
	Enrollment() Enrollment
	FetchUsage(context.Context) (Usage, error)
}

// Source opens an account's stored credentials, so the glance reads them once
// and learns the enrollment before spending a request on them.
type Source interface {
	Open(context.Context) (Fetcher, error)
}
