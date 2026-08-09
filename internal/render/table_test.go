package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
)

func TestTablePlainWideSnapshotIncludesBindingLimit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 8, 6, 0, 0, 0, time.UTC)
	rows := []Row{{
		Provider: provider.Claude,
		Account:  "work",
		Active:   true,
		Plan:     "max",
		Windows: []provider.Window{
			{Kind: provider.FiveHour, UsedPercent: 62, ResetsAt: now.Add(2*time.Hour + 10*time.Minute)},
			{Kind: provider.Weekly, UsedPercent: 34, ResetsAt: now.Add(51 * time.Hour)},
		},
		Limits: []provider.Limit{
			{Kind: "weekly", Scope: "Fable", UsedPercent: 71, ResetsAt: now.Add(12 * time.Hour), Active: true},
			{Kind: "weekly", Scope: "Other", UsedPercent: 90, ResetsAt: now.Add(time.Hour), Active: false},
		},
	}}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Plain: true, Width: 120, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	want := "CLAUDE\n" +
		"* work               5h [#####---]  62% regen 2h10m   wk [###-----]  34% regen 2d03h  (max)\n" +
		"  binding Fable        wk [######--]  71% regen 12h00m\n"
	if got := output.String(); got != want {
		t.Fatalf("snapshot mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableErrorRowSnapshotHasNextStep(t *testing.T) {
	t.Parallel()

	rows := []Row{{
		Provider: provider.Codex,
		Account:  "acct3",
		Problem: &Problem{
			Message: "Usage could not be loaded for codex account \"acct3\".",
			Action:  "Run 'hop login codex acct3', then retry.",
		},
	}}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Plain: true, Width: 120}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	want := "CODEX\n- acct3              ! Usage could not be loaded for codex account \"acct3\". Run 'hop login codex acct3', then retry.\n"
	if got := output.String(); got != want {
		t.Fatalf("snapshot mismatch\ngot:  %q\nwant: %q", got, want)
	}
}

func TestTableNarrowUsesOneMeterPerLine(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 8, 6, 0, 0, 0, time.UTC)
	rows := []Row{{
		Provider: provider.Codex,
		Account:  "personal",
		Windows: []provider.Window{
			{Kind: provider.FiveHour, UsedPercent: 3, ResetsAt: now.Add(20 * time.Minute)},
			{Kind: provider.Weekly, UsedPercent: 9, ResetsAt: now.Add(24 * time.Hour)},
		},
	}}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Plain: true, Width: 60, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	want := "CODEX\n- personal\n  5h [--------]   3% regen 20m\n  wk [#-------]   9% regen 1d00h\n"
	if got := output.String(); got != want {
		t.Fatalf("snapshot mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableNarrowErrorKeepsRecoveryStepOnItsOwnLine(t *testing.T) {
	t.Parallel()

	rows := []Row{{
		Provider: provider.Claude,
		Account:  "broken",
		Problem:  &Problem{Message: "Usage is unavailable.", Action: "Run 'hop login claude broken', then retry."},
	}}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Plain: true, Width: 50}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	want := "CLAUDE\n- broken ! Usage is unavailable.\n  Run 'hop login claude broken', then retry.\n"
	if got := output.String(); got != want {
		t.Fatalf("snapshot mismatch\ngot:  %q\nwant: %q", got, want)
	}
}

func TestScopeLabelExtractsClaudeModelDisplayName(t *testing.T) {
	t.Parallel()

	scope := `{"model":{"id":null,"display_name":"Fable"},"surface":null}`
	if got := scopeLabel(scope); got != "Fable" {
		t.Fatalf("scopeLabel() = %q, want Fable", got)
	}
}

func TestTableColorsNormalWarningAndCriticalMeters(t *testing.T) {
	t.Parallel()

	now := time.Now()
	rows := []Row{{
		Provider: provider.Claude,
		Account:  "colors",
		Windows: []provider.Window{
			{Kind: provider.FiveHour, UsedPercent: 10, ResetsAt: now.Add(time.Hour)},
			{Kind: provider.Weekly, UsedPercent: 75, ResetsAt: now.Add(time.Hour)},
		},
		Limits: []provider.Limit{{Kind: "weekly", Scope: "critical", UsedPercent: 95, ResetsAt: now.Add(time.Hour), Active: true}},
	}}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Color: true, Width: 120, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	for _, color := range []string{"\x1b[32m", "\x1b[33m", "\x1b[31m"} {
		if !strings.Contains(output.String(), color) {
			t.Errorf("output missing severity color %q: %q", color, output.String())
		}
	}
}
