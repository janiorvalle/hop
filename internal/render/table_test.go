package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
)

// lockedDesignRows mirrors the dataset in the owner-locked "Headroom
// traffic-light" mockup recorded on quest 328.
func lockedDesignRows(now time.Time) []Row {
	return []Row{
		{Provider: provider.Claude, Account: "jvalle1", Problem: &Problem{
			Message: `Usage could not be loaded for claude account "jvalle1".`,
			Action:  `decode Claude session limit; resets_at may be null only while percent is zero and the limit is inactive, update hop before retrying: claude usage request failed`,
		}},
		{Provider: provider.Claude, Account: "work1",
			Windows: []provider.Window{
				{Kind: provider.FiveHour, UsedPercent: 66, ResetsAt: now.Add(48 * time.Minute)},
				{Kind: provider.Weekly, UsedPercent: 66, ResetsAt: now.Add(107 * time.Hour)},
			},
			Limits: []provider.Limit{{Kind: "weekly", Scope: "Fable", UsedPercent: 97, ResetsAt: now.Add(107 * time.Hour), Active: true}},
		},
		{Provider: provider.Claude, Account: "work2", Active: true,
			Windows: []provider.Window{
				{Kind: provider.FiveHour, UsedPercent: 7, ResetsAt: now.Add(4*time.Hour + 38*time.Minute)},
				{Kind: provider.Weekly, UsedPercent: 49, ResetsAt: now.Add(83 * time.Hour)},
			},
			Limits: []provider.Limit{{Kind: "weekly", Scope: "Fable", UsedPercent: 68, ResetsAt: now.Add(83 * time.Hour), Active: true}},
		},
		{Provider: provider.Claude, Account: "work3",
			Windows: []provider.Window{
				{Kind: provider.FiveHour, UsedPercent: 1, ResetsAt: now.Add(4*time.Hour + 48*time.Minute)},
				{Kind: provider.Weekly, UsedPercent: 60, ResetsAt: now.Add(76 * time.Hour)},
			},
			Limits: []provider.Limit{{Kind: "weekly", Scope: "Fable", UsedPercent: 100, ResetsAt: now.Add(76 * time.Hour), Active: true}},
		},
		{Provider: provider.Codex, Account: "jvalle1", Plan: "pro",
			Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 0, ResetsAt: now.Add(168 * time.Hour)}},
			Limits:  []provider.Limit{{Kind: "weekly", Scope: "GPT-5.3-Codex", UsedPercent: 0, ResetsAt: now.Add(168 * time.Hour), Active: true}},
		},
		{Provider: provider.Codex, Account: "jvalle2", Plan: "pro",
			Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 57, ResetsAt: now.Add(141 * time.Hour)}},
		},
		{Provider: provider.Codex, Account: "work1", Plan: "pro",
			Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 4, ResetsAt: now.Add(141 * time.Hour)}},
		},
	}
}

func TestTableWidePlainSnapshotMatchesLockedDesign(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	var output bytes.Buffer
	if err := Table(&output, lockedDesignRows(now), Options{Plain: true, Width: 120, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	want := "HEADROOM = capacity left at the binding limit\n" +
		"+ 50-100 plenty   ~ 10-49 tight   o 0-9 nearly/full   ! error   > active\n" +
		"\n" +
		"CLAUDE\n" +
		"    ACCOUNT    HEADROOM   5 HOUR         WEEKLY         BINDING: Fable / WEEKLY\n" +
		"> ~ work2      32% LEFT    93% . 4h38m    51% . 3d11h    32% . 3d11h\n" +
		"  o work1       3% LEFT    34% . 48m      34% . 4d11h     3% . 4d11h\n" +
		"  o work3       0% LEFT    99% . 4h48m    40% . 3d04h     0% . 3d04h\n" +
		"  ! jvalle1       ERROR\n" +
		"    Usage could not be loaded for claude account \"jvalle1\". decode Claude session limit;\n" +
		"    resets_at may be null only while percent is zero and the limit is inactive, update hop\n" +
		"    before retrying: claude usage request failed\n" +
		"\n" +
		"CODEX  .  pro  .  no 5-hour window\n" +
		"    ACCOUNT    HEADROOM   WEEKLY         BINDING: GPT-5.3-Codex / WEEKLY\n" +
		"  + jvalle1   100% LEFT   100% . 7d00h   100% . 7d00h\n" +
		"  + work1      96% LEFT    96% . 5d21h   -\n" +
		"  ~ jvalle2    43% LEFT    43% . 5d21h   -\n"
	if got := output.String(); got != want {
		t.Fatalf("snapshot mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableNarrowPlainSnapshotMatchesLockedDesign(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	var output bytes.Buffer
	if err := Table(&output, lockedDesignRows(now), Options{Plain: true, Width: 80, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	want := "HEADROOM = left at binding cap   detail = left / resets in   * binding\n" +
		"+ 50-100 plenty   ~ 10-49 tight   o 0-9 nearly/full   ! error   > active\n" +
		"\n" +
		"CLAUDE\n" +
		"> ~ work2     32% LEFT  ACTIVE\n" +
		"    5h 93%/4h38m . week 51%/3d11h . Fable* 32%/3d11h\n" +
		"  o work1      3% LEFT\n" +
		"    5h 34%/48m . week 34%/4d11h . Fable* 3%/4d11h\n" +
		"  o work3      0% LEFT\n" +
		"    5h 99%/4h48m . week 40%/3d04h . Fable* 0%/3d04h\n" +
		"  ! jvalle1      ERROR\n" +
		"    Usage could not be loaded for claude account \"jvalle1\". decode Claude\n" +
		"    session limit; resets_at may be null only while percent is zero and the\n" +
		"    limit is inactive, update hop before retrying: claude usage request failed\n" +
		"\n" +
		"CODEX  .  pro  .  no 5-hour window\n" +
		"  + jvalle1  100% LEFT\n" +
		"    week 100%/7d00h . GPT-5.3-Codex* 100%/7d00h\n" +
		"  + work1     96% LEFT\n" +
		"    week 96%/5d21h\n" +
		"  ~ jvalle2   43% LEFT\n" +
		"    week 43%/5d21h\n"
	if got := output.String(); got != want {
		t.Fatalf("snapshot mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableWideShowsIdleWindowWithoutRegen(t *testing.T) {
	t.Parallel()

	rows := []Row{{
		Provider: provider.Claude,
		Account:  "idle",
		Windows:  []provider.Window{{Kind: provider.FiveHour, UsedPercent: 0}},
	}}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Plain: true, Width: 120}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	want := "HEADROOM = capacity left at the binding limit\n" +
		"+ 50-100 plenty   ~ 10-49 tight   o 0-9 nearly/full   ! error   > active\n" +
		"\n" +
		"CLAUDE\n" +
		"    ACCOUNT    HEADROOM   5 HOUR\n" +
		"  + idle      100% LEFT   100%\n"
	if got := output.String(); got != want {
		t.Fatalf("snapshot mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableWideShowsPlanColumnWhenPlansDiffer(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	rows := []Row{
		{Provider: provider.Codex, Account: "a", Plan: "pro",
			Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 20, ResetsAt: now.Add(24 * time.Hour)}}},
		{Provider: provider.Codex, Account: "b",
			Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 80, ResetsAt: now.Add(24 * time.Hour)}}},
	}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Plain: true, Width: 120, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	want := "HEADROOM = capacity left at the binding limit\n" +
		"+ 50-100 plenty   ~ 10-49 tight   o 0-9 nearly/full   ! error   > active\n" +
		"\n" +
		"CODEX  .  no 5-hour window\n" +
		"    ACCOUNT    HEADROOM   WEEKLY         PLAN\n" +
		"  + a          80% LEFT    80% . 1d00h   (pro)\n" +
		"  ~ b          20% LEFT    20% . 1d00h   -\n"
	if got := output.String(); got != want {
		t.Fatalf("snapshot mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableColorsSeverityAndDimsDetail(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	var output bytes.Buffer
	if err := Table(&output, lockedDesignRows(now), Options{Color: true, Width: 120, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	for name, code := range map[string]string{
		"green": styleGreen, "amber": styleAmber, "red": styleRed, "dim": styleDim, "bold": styleBold,
	} {
		if !strings.Contains(output.String(), code) {
			t.Errorf("output missing %s style %q", name, code)
		}
	}
	for _, barGlyph := range []string{"#", "█", "░"} {
		// Bars are gone in the locked design; their glyphs would be a regression.
		if strings.Contains(output.String(), barGlyph) {
			t.Errorf("output contains bar remnant %q: %q", barGlyph, output.String())
		}
	}
}

func TestTableShortensLongAccountNamesDeliberately(t *testing.T) {
	t.Parallel()

	rows := []Row{{
		Provider: provider.Claude,
		Account:  "averyveryverylongaccountname",
		Windows:  []provider.Window{{Kind: provider.Weekly, UsedPercent: 10}},
	}}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Plain: true, Width: 120}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	if !strings.Contains(output.String(), "averyveryverylo...") {
		t.Fatalf("long account name not shortened with ellipsis: %q", output.String())
	}
}

func TestTableKeepsFiveHourClaimHonestForFiveHourCaps(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	rows := []Row{{
		Provider: provider.Codex,
		Account:  "capped",
		Windows:  []provider.Window{{Kind: provider.Weekly, UsedPercent: 20, ResetsAt: now.Add(24 * time.Hour)}},
		Limits:   []provider.Limit{{Kind: "model_five_hour", Scope: "GPT", UsedPercent: 30, ResetsAt: now.Add(time.Hour), Active: true}},
	}}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Plain: true, Width: 120, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	if strings.Contains(output.String(), "no 5-hour window") {
		t.Errorf("section claims no 5-hour window despite a five-hour cap: %q", output.String())
	}
	if !strings.Contains(output.String(), "BINDING: GPT / 5 HOUR") {
		t.Errorf("binding header omitted the five-hour kind: %q", output.String())
	}

	output.Reset()
	if err := Table(&output, rows, Options{Plain: true, Width: 60, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	if !strings.Contains(output.String(), "GPT*(5h) 70%/1h00m") {
		t.Errorf("narrow detail omitted the five-hour tag: %q", output.String())
	}
}

func TestTableNarrowKeepsEveryLineWithinWidth(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	var output bytes.Buffer
	if err := Table(&output, lockedDesignRows(now), Options{Plain: true, Width: 64, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	for _, line := range strings.Split(output.String(), "\n") {
		if len([]rune(line)) > 64 {
			t.Errorf("line exceeds width 64: %q", line)
		}
	}
}

func TestTableFallsBackToNarrowWhenWideDoesNotFit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	rows := []Row{{
		Provider: provider.Codex,
		Account:  "averyveryverylongaccountname",
		Windows: []provider.Window{
			{Kind: provider.FiveHour, UsedPercent: 10, ResetsAt: now.Add(time.Hour)},
			{Kind: provider.Weekly, UsedPercent: 20, ResetsAt: now.Add(24 * time.Hour)},
		},
		Limits: []provider.Limit{{Kind: "weekly", Scope: "GPT-5.3-Codex-Spark", UsedPercent: 30, ResetsAt: now.Add(24 * time.Hour), Active: true}},
	}}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Plain: true, Width: 88, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	for _, line := range strings.Split(output.String(), "\n") {
		if len([]rune(line)) > 88 {
			t.Errorf("line exceeds width 88: %q", line)
		}
	}
	if !strings.Contains(output.String(), "GPT-5.3-Codex-Spark* 70%/1d00h") {
		t.Errorf("expected narrow fallback detail row: %q", output.String())
	}
}

func TestScopeLabelExtractsClaudeModelDisplayName(t *testing.T) {
	t.Parallel()

	scope := `{"model":{"id":null,"display_name":"Fable"},"surface":null}`
	if got := scopeLabel(scope); got != "Fable" {
		t.Fatalf("scopeLabel() = %q, want Fable", got)
	}
}
