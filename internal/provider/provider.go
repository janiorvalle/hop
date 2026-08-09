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

// Usage is the provider-neutral response consumed by the glance command.
type Usage struct {
	Provider Name     `json:"provider"`
	Email    string   `json:"email,omitempty"`
	Plan     string   `json:"plan,omitempty"`
	Windows  []Window `json:"windows"`
	Limits   []Limit  `json:"limits"`
}

// Fetcher retrieves normalized usage for one account.
type Fetcher interface {
	FetchUsage(context.Context) (Usage, error)
}
