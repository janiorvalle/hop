// Package provider defines the normalized data returned by provider adapters.
package provider

import (
	"context"
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
	ResetsAt    time.Time  `json:"resets_at"`
}

// Limit is a provider-specific cap, including model-scoped binding limits.
type Limit struct {
	Kind        string    `json:"kind"`
	Group       string    `json:"group"`
	UsedPercent float64   `json:"used_percent"`
	Severity    string    `json:"severity"`
	ResetsAt    time.Time `json:"resets_at"`
	Scope       string    `json:"scope,omitempty"`
	Active      bool      `json:"active"`
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
