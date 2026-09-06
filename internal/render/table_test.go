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
		{Provider: provider.Claude, Account: "work1", Plan: "Max 5x",
			Windows: []provider.Window{
				{Kind: provider.FiveHour, UsedPercent: 66, ResetsAt: now.Add(48 * time.Minute)},
				{Kind: provider.Weekly, UsedPercent: 66, ResetsAt: now.Add(107 * time.Hour)},
			},
			Limits: []provider.Limit{{Kind: "weekly", Scope: "Fable", UsedPercent: 97, ResetsAt: now.Add(107 * time.Hour), Active: true}},
		},
		{Provider: provider.Claude, Account: "work2", Active: true, Plan: "Max 20x",
			Windows: []provider.Window{
				{Kind: provider.FiveHour, UsedPercent: 7, ResetsAt: now.Add(4*time.Hour + 38*time.Minute)},
				{Kind: provider.Weekly, UsedPercent: 49, ResetsAt: now.Add(83 * time.Hour)},
			},
			Limits: []provider.Limit{{Kind: "weekly", Scope: "Fable", UsedPercent: 68, ResetsAt: now.Add(83 * time.Hour), Active: true}},
		},
		{Provider: provider.Claude, Account: "work3", Plan: "Pro",
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
		"    ACCOUNT    HEADROOM   5 HOUR         WEEKLY         BINDING: Fable / WEEKLY   PLAN\n" +
		"> ~ work2      32% LEFT    93% . 4h38m    51% . 3d11h    32% . 3d11h              (Max 20x)\n" +
		"  o work1       3% LEFT    34% . 48m      34% . 4d11h     3% . 4d11h              (Max 5x)\n" +
		"  o work3       0% LEFT    99% . 4h48m    40% . 3d04h     0% . 3d04h              (Pro)\n" +
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
		"    5h 93%/4h38m . week 51%/3d11h . Fable* 32%/3d11h . Max 20x\n" +
		"  o work1      3% LEFT\n" +
		"    5h 34%/48m . week 34%/4d11h . Fable* 3%/4d11h . Max 5x\n" +
		"  o work3      0% LEFT\n" +
		"    5h 99%/4h48m . week 40%/3d04h . Fable* 0%/3d04h . Pro\n" +
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

func TestTableShowsCodexDurationAndCodeReviewLimits(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	rows := []Row{{
		Provider: provider.Codex,
		Account:  "work",
		Plan:     "pro",
		Windows:  []provider.Window{{Kind: provider.FiveHour, UsedPercent: 62, ResetsAt: now.Add(2*time.Hour + 10*time.Minute)}},
		Limits: []provider.Limit{
			{Kind: "account_30d", Group: "account", Scope: "30d", UsedPercent: 20, ResetsAt: now.Add(24 * time.Hour), Active: true},
			{Kind: "code_review_weekly", Group: "code_review", Scope: "code review", UsedPercent: 10, ResetsAt: now.Add(10 * time.Minute), Active: true},
		},
	}}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Plain: true, Width: 120, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	want := "HEADROOM = capacity left at the binding limit\n" +
		"+ 50-100 plenty   ~ 10-49 tight   o 0-9 nearly/full   ! error   > active\n" +
		"\n" +
		"CODEX  .  pro\n" +
		"    ACCOUNT    HEADROOM   5 HOUR         BINDING\n" +
		"  ~ work       38% LEFT    38% . 2h10m   30d  80% . 1d00h   code review 90% . 10m\n"
	if got := output.String(); got != want {
		t.Fatalf("snapshot mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}

	output.Reset()
	if err := Table(&output, rows, Options{Plain: true, Width: 60, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	for _, detail := range []string{"30d* 80%/1d00h", "code review* 90%/10m"} {
		if !strings.Contains(output.String(), detail) {
			t.Errorf("narrow detail missing %q: %q", detail, output.String())
		}
	}
}

func TestTableNamesUnknownDurationsOnce(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	testCases := []struct {
		name       string
		limit      provider.Limit
		wantHeader string
		wantNarrow string
	}{
		{
			name:       "account window is its own scope",
			limit:      provider.Limit{Kind: "account_30d", Scope: "30d", UsedPercent: 20, ResetsAt: now.Add(24 * time.Hour), Active: true},
			wantHeader: "BINDING: 30d\n",
			wantNarrow: "30d* 80%/1d00h",
		},
		{
			name:       "model cap carries its duration",
			limit:      provider.Limit{Kind: "model_30d", Scope: "GPT", UsedPercent: 20, ResetsAt: now.Add(24 * time.Hour), Active: true},
			wantHeader: "BINDING: GPT / 30D\n",
			wantNarrow: "GPT*(30d) 80%/1d00h",
		},
	}
	for _, testCase := range testCases {
		rows := []Row{{Provider: provider.Codex, Account: "work", Limits: []provider.Limit{testCase.limit}}}
		var output bytes.Buffer
		if err := Table(&output, rows, Options{Plain: true, Width: 120, Now: now}); err != nil {
			t.Fatalf("%s: Table() error = %v", testCase.name, err)
		}
		if !strings.Contains(output.String(), testCase.wantHeader) {
			t.Errorf("%s: wide header missing %q: %q", testCase.name, testCase.wantHeader, output.String())
		}
		output.Reset()
		if err := Table(&output, rows, Options{Plain: true, Width: 60, Now: now}); err != nil {
			t.Fatalf("%s: Table() error = %v", testCase.name, err)
		}
		if !strings.Contains(output.String(), testCase.wantNarrow) {
			t.Errorf("%s: narrow detail missing %q: %q", testCase.name, testCase.wantNarrow, output.String())
		}
	}
}

func TestTableShowsInactiveScopedLimitUsage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	rows := []Row{
		{Provider: provider.Claude, Account: "capped",
			Windows: []provider.Window{{Kind: provider.FiveHour, UsedPercent: 40, ResetsAt: now.Add(3 * time.Hour)}},
			Limits: []provider.Limit{
				{Kind: "session", UsedPercent: 40, Active: true},
				{Kind: "weekly", Scope: "Fable", UsedPercent: 98, ResetsAt: now.Add(76 * time.Hour), Active: false},
			},
		},
		{Provider: provider.Claude, Account: "bare",
			Windows: []provider.Window{{Kind: provider.FiveHour, UsedPercent: 40, ResetsAt: now.Add(3 * time.Hour)}},
		},
	}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Plain: true, Width: 120, Now: now}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	got := output.String()
	if !strings.Contains(got, "2% . 3d04h") {
		t.Errorf("inactive Fable weekly limit at 98%% used renders no usage: %q", got)
	}
	if strings.Contains(got, "2% LEFT") {
		t.Errorf("headroom must stay bound to active limits, not the inactive cap: %q", got)
	}
	bareLine := ""
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "bare") {
			bareLine = line
		}
	}
	if !strings.HasSuffix(bareLine, "-") {
		t.Errorf("account with no scoped limit data lost its dash: %q", bareLine)
	}
}

func TestTableHeadroomBindsOnScopelessActiveLimits(t *testing.T) {
	t.Parallel()

	rows := []Row{{
		Provider: provider.Claude,
		Account:  "session",
		Limits:   []provider.Limit{{Kind: "session", UsedPercent: 95, Active: true}},
	}}
	var output bytes.Buffer
	if err := Table(&output, rows, Options{Plain: true, Width: 120}); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	if !strings.Contains(output.String(), "  o session     5% LEFT") {
		t.Errorf("scope-less active limit did not bind the headline: %q", output.String())
	}
	if strings.Contains(output.String(), "100% LEFT") {
		t.Errorf("account reported fully available despite a 95%% used session limit: %q", output.String())
	}
}

func TestScopeLabelExtractsClaudeModelDisplayName(t *testing.T) {
	t.Parallel()

	scope := `{"model":{"id":null,"display_name":"Fable"},"surface":null}`
	if got := scopeLabel(scope); got != "Fable" {
		t.Fatalf("scopeLabel() = %q, want Fable", got)
	}
}
