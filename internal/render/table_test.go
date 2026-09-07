package render

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
)

// humanRows is the owner's own account set on the day issue 65 was filed,
// the dataset behind the mockup the glance is built to match.
func humanRows(now time.Time) []Row {
	// An idle Claude five-hour window has no reset yet; a Codex one always does.
	idle := time.Time{}
	claude := func(name string, active bool, fiveHourUsed float64, fiveHourReset time.Time, weeklyUsed, fableUsed float64, weeklyReset time.Duration) Row {
		return Row{Provider: provider.Claude, Account: name, Active: active,
			Windows: []provider.Window{
				{Kind: provider.FiveHour, UsedPercent: fiveHourUsed, ResetsAt: fiveHourReset},
				{Kind: provider.Weekly, UsedPercent: weeklyUsed, ResetsAt: now.Add(weeklyReset)},
			},
			Limits: []provider.Limit{{Kind: "weekly_scoped", Group: "weekly", Scope: fableScope, UsedPercent: fableUsed, ResetsAt: now.Add(weeklyReset), Active: true}},
		}
	}
	codex := func(name string, active bool, weeklyUsed float64, weeklyReset time.Duration, resetCredits int) Row {
		return Row{Provider: provider.Codex, Account: name, Active: active,
			Windows: []provider.Window{
				{Kind: provider.FiveHour, UsedPercent: 0, ResetsAt: now.Add(5 * time.Hour)},
				{Kind: provider.Weekly, UsedPercent: weeklyUsed, ResetsAt: now.Add(weeklyReset)},
			},
			Limits: []provider.Limit{
				{Kind: "model_five_hour", Scope: "GPT-5.3-Codex-Spark", UsedPercent: 0, ResetsAt: now.Add(5 * time.Hour), Active: true},
				{Kind: "model_weekly", Scope: "GPT-5.3-Codex-Spark", UsedPercent: 0, ResetsAt: now.Add(7 * 24 * time.Hour), Active: true},
			},
			ResetCredits: resetCredits,
		}
	}
	jvalle1 := claude("jvalle1", false, 0, idle, 17, 16, 6*24*time.Hour+18*time.Hour)
	jvalle1.RefreshTokenExpiry = &TokenExpiry{
		ExpiresAt: now.Add(37 * time.Hour),
		Severity:  "critical",
		Commands:  []string{"hop rm claude jvalle1", "hop login claude jvalle1"},
	}
	return []Row{
		jvalle1,
		claude("work4", true, 51, now.Add(3*time.Hour+7*time.Minute), 28, 49, 4*24*time.Hour+17*time.Hour),
		claude("work3", false, 0, idle, 47, 66, 3*24*time.Hour+2*time.Hour),
		claude("jvalle2", false, 0, idle, 58, 77, 3*24*time.Hour+11*time.Hour),
		claude("work1", false, 16, now.Add(17*time.Minute), 45, 79, 4*24*time.Hour+9*time.Hour),
		{Provider: provider.Claude, Account: "work2", Problem: &Problem{
			Fact:     "usage unavailable (HTTP 400)",
			Commands: []string{"hop login claude work2"},
		}},
		codex("jvalle1", true, 5, 6*24*time.Hour+20*time.Hour, 1),
		codex("work1", false, 7, 13*time.Hour+56*time.Minute, 3),
		codex("jvalle2", false, 12, 5*24*time.Hour+18*time.Hour, 2),
		codex("work2", false, 92, 11*time.Hour+7*time.Minute, 3),
	}
}

// fableScope is the Fable model cap's scope exactly as Claude's usage
// endpoint sends it and hop.ls/v1 carries it.
const fableScope = `{"model":{"id":null,"display_name":"Fable"},"surface":null}`

func fixedNow() time.Time {
	return time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
}

func render(t *testing.T, rows []Row, options Options) string {
	t.Helper()
	var output bytes.Buffer
	if err := Table(&output, rows, options); err != nil {
		t.Fatalf("Table() error = %v", err)
	}
	return output.String()
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func widestLine(output string) int {
	widest := 0
	for _, line := range strings.Split(output, "\n") {
		if width := len([]rune(ansiPattern.ReplaceAllString(line, ""))); width > widest {
			widest = width
		}
	}
	return widest
}

func TestTablePlainSnapshotMatchesTheMockup(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	got := render(t, humanRows(now), Options{Plain: true, Width: 100, Now: now})
	want := "CLAUDE\n" +
		"    ACCOUNT   HEADROOM                      WEEK            5 HOUR          FABLE\n" +
		"  + jvalle1   #################...    83%    83%   6d18h    100%             84%   6d18h\n" +
		"> ~ work4     ##########..........    49%    72%   4d17h     49%   3h07m     51%   4d17h\n" +
		"  ~ work3     #######.............    34%    53%   3d02h    100%             34%   3d02h\n" +
		"  ~ jvalle2   #####...............    23%    42%   3d11h    100%             23%   3d11h\n" +
		"  ~ work1     ####................    21%    55%   4d09h     84%   17m       21%   4d09h\n" +
		"  ! work2                           ERROR\n" +
		"\n" +
		"CODEX\n" +
		"    ACCOUNT   HEADROOM                      WEEK            5 HOUR          RESETS\n" +
		"> + jvalle1   ###################.    95%    95%   6d20h    100%   5h00m    1\n" +
		"  + work1     ###################.    93%    93%   13h56m   100%   5h00m    3\n" +
		"  + jvalle2   ##################..    88%    88%   5d18h    100%   5h00m    2\n" +
		"  o work2     ##..................     8%     8%   11h07m   100%   5h00m    3\n" +
		"\n" +
		"attention\n" +
		" 1. claude/jvalle1: token expires 1d13h; hop rm claude jvalle1; hop login claude jvalle1\n" +
		" 2. claude/work2: usage unavailable (HTTP 400); hop login claude work2\n" +
		"\n" +
		"+ 50-100 plenty   ~ 10-49 tight   o 0-9 nearly/full   ! error   > active\n"
	if got != want {
		t.Fatalf("Table() plain output mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableColorSnapshotUsesBlocksAndCircles(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	got := ansiPattern.ReplaceAllString(render(t, humanRows(now), Options{Color: true, Width: 100, Now: now}), "")
	want := "CLAUDE\n" +
		"    ACCOUNT   HEADROOM                      WEEK            5 HOUR          FABLE\n" +
		"  ● jvalle1   █████████████████░░░    83%    83%   6d18h    100%             84%   6d18h\n" +
		"> ◐ work4     ██████████░░░░░░░░░░    49%    72%   4d17h     49%   3h07m     51%   4d17h\n" +
		"  ◐ work3     ███████░░░░░░░░░░░░░    34%    53%   3d02h    100%             34%   3d02h\n" +
		"  ◐ jvalle2   █████░░░░░░░░░░░░░░░    23%    42%   3d11h    100%             23%   3d11h\n" +
		"  ◐ work1     ████░░░░░░░░░░░░░░░░    21%    55%   4d09h     84%   17m       21%   4d09h\n" +
		"  ! work2                           ERROR\n" +
		"\n" +
		"CODEX\n" +
		"    ACCOUNT   HEADROOM                      WEEK            5 HOUR          RESETS\n" +
		"> ● jvalle1   ███████████████████░    95%    95%   6d20h    100%   5h00m    1\n" +
		"  ● work1     ███████████████████░    93%    93%   13h56m   100%   5h00m    3\n" +
		"  ● jvalle2   ██████████████████░░    88%    88%   5d18h    100%   5h00m    2\n" +
		"  ○ work2     ██░░░░░░░░░░░░░░░░░░     8%     8%   11h07m   100%   5h00m    3\n" +
		"\n" +
		"attention\n" +
		" 1. claude/jvalle1: token expires 1d13h; hop rm claude jvalle1; hop login claude jvalle1\n" +
		" 2. claude/work2: usage unavailable (HTTP 400); hop login claude work2\n" +
		"\n" +
		"● 50-100 plenty   ◐ 10-49 tight   ○ 0-9 nearly/full   ! error   > active\n"
	if got != want {
		t.Fatalf("Table() color output (styles stripped) mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableColorsRowsByThresholdAndDimsTheRest(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	got := render(t, humanRows(now), Options{Color: true, Width: 100, Now: now})
	for _, want := range []string{
		styleGreen + "●" + styleReset + " jvalle1   " + styleGreen + "█████████████████" + styleReset + styleDim + "░░░" + styleReset + "   " + styleGreen + " 83%" + styleReset +
			"   " + styleGreen + " 83%" + styleReset + "   " + styleDim + "6d18h" + styleReset + "    " + styleGreen + "100%" + styleReset + "            " + styleGreen + " 84%" + styleReset + "   " + styleDim + "6d18h" + styleReset + "\n",
		styleAmber + "◐" + styleReset + " work4     " + styleAmber + "██████████" + styleReset + styleDim + "░░░░░░░░░░" + styleReset + "   " + styleAmber + " 49%" + styleReset +
			"   " + styleGreen + " 72%" + styleReset + "   " + styleDim + "4d17h" + styleReset + "    " + styleAmber + " 49%" + styleReset + "   " + styleDim + "3h07m" + styleReset + "    " + styleGreen + " 51%" + styleReset,
		styleDim + "5h00m" + styleReset + "    " + styleDim + "1" + styleReset + "\n",
		styleRed + "○" + styleReset + " work2     " + styleRed + "██" + styleReset + styleDim + "░░░░░░░░░░░░░░░░░░" + styleReset + "   " + styleRed + "  8%" + styleReset,
		styleRed + "!" + styleReset + " work2     " + styleRed + "                      ERROR" + styleReset,
		styleBold + "CLAUDE" + styleReset,
		styleDim + "    ACCOUNT   HEADROOM                      WEEK            5 HOUR          FABLE" + styleReset,
		styleBold + "attention" + styleReset,
		" 1. " + styleRed + "claude/jvalle1: token expires 1d13h; hop rm claude jvalle1; hop login claude jvalle1" + styleReset,
		" 2. " + styleRed + "claude/work2: usage unavailable (HTTP 400); hop login claude work2" + styleReset,
		styleDim + "● 50-100 plenty",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Table() color output missing %q:\n%s", want, got)
		}
	}
}

func TestTableColorsARefreshWarningAmber(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	rows := []Row{{Provider: provider.Codex, Account: "quiet",
		Windows:            []provider.Window{{Kind: provider.Weekly, UsedPercent: 12, ResetsAt: now.Add(time.Hour)}},
		RefreshTokenExpiry: &TokenExpiry{ExpiresAt: now.Add(6 * 24 * time.Hour), Severity: "warning", Commands: []string{"hop login codex quiet"}},
	}}
	got := render(t, rows, Options{Color: true, Width: 100, Now: now})
	want := " 1. " + styleAmber + "codex/quiet: token expires 6d00h; hop login codex quiet" + styleReset + "\n"
	if !strings.Contains(got, want) {
		t.Fatalf("Table() output missing amber warning %q:\n%s", want, got)
	}
}

func TestTableFitsTheWidthsTheDesignPromises(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	for _, scenario := range []struct {
		name    string
		rows    []Row
		options Options
	}{
		{name: "wide color", rows: longestRows(now), options: Options{Color: true, Width: 100, Now: now}},
		{name: "wide plain", rows: longestRows(now), options: Options{Plain: true, Width: 100, Now: now}},
		{name: "narrow color", rows: humanRows(now), options: Options{Color: true, Width: 88, Now: now}},
		{name: "narrow plain", rows: humanRows(now), options: Options{Plain: true, Width: 88, Now: now}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			output := render(t, scenario.rows, scenario.options)
			if widest := widestLine(output); widest > scenario.options.Width {
				t.Fatalf("Table() widest line = %d, want <= %d:\n%s", widest, scenario.options.Width, output)
			}
		})
	}
}

// longestRows is the mockup's accounts with the longest name the glance
// shows and the longest countdown a week can have.
func longestRows(now time.Time) []Row {
	rows := humanRows(now)
	for index := range rows {
		rows[index].Account = strings.Repeat("x", maxNameRunes)
	}
	rows[0].Windows[1].ResetsAt = now.Add(13*time.Hour + 56*time.Minute)
	rows[0].Limits[0].ResetsAt = now.Add(13*time.Hour + 56*time.Minute)
	return rows
}

func TestTableWideLayoutUsesExactlyAHundredColumns(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	if widest := widestLine(render(t, longestRows(now), Options{Plain: true, Width: 100, Now: now})); widest != 100 {
		t.Fatalf("Table() widest line = %d, want 100", widest)
	}
}

func TestTableWrapsAnAttentionLineAtItsSemicolons(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	got := render(t, humanRows(now), Options{Plain: true, Width: 80, Now: now})
	want := " 1. claude/jvalle1: token expires 1d13h; hop rm claude jvalle1\n" +
		"    hop login claude jvalle1\n" +
		" 2. claude/work2: usage unavailable (HTTP 400); hop login claude work2\n"
	if !strings.Contains(got, want) {
		t.Fatalf("Table() at 80 columns did not wrap at the joiner, want:\n%s\ngot:\n%s", want, got)
	}
}

func TestTableWrapsACommandlessFailureBetweenWords(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	rows := []Row{{Provider: provider.Codex, Account: "work", Problem: &Problem{
		Fact:     "usage unavailable",
		Commands: []string{`reach Codex usage endpoint; check the network and retry: Get "https://chatgpt.com/backend-api/wham/usage": dial tcp: connection refused: codex usage request failed`},
	}}}
	got := render(t, rows, Options{Plain: true, Width: 80, Now: now})
	want := " 1. codex/work: usage unavailable\n" +
		"    reach Codex usage endpoint; check the network and retry: Get\n" +
		"    \"https://chatgpt.com/backend-api/wham/usage\": dial tcp: connection refused:\n" +
		"    codex usage request failed\n"
	if !strings.Contains(got, want) {
		t.Fatalf("Table() did not wrap the long failure, want:\n%s\ngot:\n%s", want, got)
	}
}

func TestTableDropsTheLegendWhenItDoesNotFit(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	got := render(t, humanRows(now), Options{Plain: true, Width: 64, Now: now})
	if strings.Contains(got, "50-100 plenty") {
		t.Fatalf("Table() at 64 columns kept a legend wider than the terminal:\n%s", got)
	}
	if !strings.HasSuffix(got, "hop login claude work2\n") {
		t.Fatalf("Table() at 64 columns should end on the attention list:\n%s", got)
	}
}

func TestTableOmitsAttentionWhenNothingNeedsIt(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	rows := []Row{{Provider: provider.Codex, Account: "fine",
		Windows:            []provider.Window{{Kind: provider.Weekly, UsedPercent: 12, ResetsAt: now.Add(time.Hour)}},
		RefreshTokenExpiry: &TokenExpiry{ExpiresAt: now.Add(30 * 24 * time.Hour), Severity: "normal"},
	}}
	got := render(t, rows, Options{Plain: true, Width: 100, Now: now})
	want := "CODEX\n" +
		"    ACCOUNT   HEADROOM                      WEEK            5 HOUR          RESETS\n" +
		"  + fine   ##################..    88%    88%   1h00m\n" +
		"\n" +
		"+ 50-100 plenty   ~ 10-49 tight   o 0-9 nearly/full   ! error   > active\n"
	if got != want {
		t.Fatalf("Table() output mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableSaysExpiredOnceTheTokenIsGone(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	rows := []Row{{Provider: provider.Claude, Account: "dead",
		Windows:            []provider.Window{{Kind: provider.FiveHour, UsedPercent: 12, ResetsAt: now.Add(time.Hour)}},
		RefreshTokenExpiry: &TokenExpiry{ExpiresAt: now.Add(-time.Hour), Severity: "critical", Commands: []string{"hop rm claude dead", "hop login claude dead"}},
	}}
	got := render(t, rows, Options{Plain: true, Width: 100, Now: now})
	if !strings.Contains(got, " 1. claude/dead: token expired; hop rm claude dead; hop login claude dead\n") {
		t.Fatalf("Table() output missing the expired line:\n%s", got)
	}
}

func TestTableListsAnErrorRowsFailureAndItsTokenWarningSeparately(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	rows := []Row{
		{Provider: provider.Claude, Account: "work",
			Windows: []provider.Window{{Kind: provider.FiveHour, UsedPercent: 12, ResetsAt: now.Add(time.Hour)}},
		},
		{Provider: provider.Claude, Account: "stale",
			Problem:            &Problem{Fact: "usage unavailable (HTTP 502)", Commands: []string{"hop ls"}},
			RefreshTokenExpiry: &TokenExpiry{ExpiresAt: now.Add(6 * 24 * time.Hour), Severity: "warning", Commands: []string{"hop rm claude stale", "hop login claude stale"}},
		},
	}
	got := render(t, rows, Options{Plain: true, Width: 100, Now: now})
	want := "CLAUDE\n" +
		"    ACCOUNT   HEADROOM                      WEEK            5 HOUR          FABLE\n" +
		"  + work    ##################..    88%                    88%   1h00m\n" +
		"  ! stale                         ERROR\n" +
		"\n" +
		"attention\n" +
		" 1. claude/stale: usage unavailable (HTTP 502); hop ls\n" +
		" 2. claude/stale: token expires 6d00h; hop rm claude stale; hop login claude stale\n" +
		"\n" +
		"+ 50-100 plenty   ~ 10-49 tight   o 0-9 nearly/full   ! error   > active\n"
	if got != want {
		t.Fatalf("Table() output mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableNumbersTenOrMoreAttentionItemsInOneColumn(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	rows := make([]Row, 0, 10)
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		rows = append(rows, Row{Provider: provider.Codex, Account: name, Problem: &Problem{Fact: "usage unavailable", Commands: []string{"hop ls"}}})
	}
	got := render(t, rows, Options{Plain: true, Width: 100, Now: now})
	for _, want := range []string{"  1. codex/a: usage unavailable; hop ls\n", " 10. codex/j: usage unavailable; hop ls\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Table() output missing %q:\n%s", want, got)
		}
	}
}

func TestTableDisabledRowSortsLastAndIsDimmed(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	rows := []Row{
		{Provider: provider.Codex, Account: "old", Active: true, Disabled: true},
		{Provider: provider.Codex, Account: "broken", Problem: &Problem{Fact: "usage unavailable", Commands: []string{"hop login codex broken"}}},
		{Provider: provider.Codex, Account: "work", Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 96, ResetsAt: now.Add(time.Hour)}}},
	}
	plain := render(t, rows, Options{Plain: true, Width: 100, Now: now})
	want := "CODEX\n" +
		"    ACCOUNT   HEADROOM                      WEEK            5 HOUR          RESETS\n" +
		"  o work     #...................     4%     4%   1h00m\n" +
		"  ! broken                         ERROR\n" +
		">   old                         disabled\n"
	if !strings.HasPrefix(plain, want) {
		t.Fatalf("Table() plain output mismatch\n got:\n%s\nwant prefix:\n%s", plain, want)
	}
	colored := render(t, rows, Options{Color: true, Width: 100, Now: now})
	if !strings.Contains(colored, ">   "+styleDim+"old   "+styleReset+"   "+styleDim+"                   disabled"+styleReset+"\n") {
		t.Fatalf("Table() color output did not dim the disabled row:\n%s", colored)
	}
}

func TestTableHeadroomIsTheBindingMeters(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	for _, scenario := range []struct {
		name string
		row  Row
		want string
	}{
		{
			name: "the tighter window wins over a scoped limit at zero",
			row: Row{Provider: provider.Codex, Account: "work",
				Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 5, ResetsAt: now.Add(48 * time.Hour)}},
				Limits:  []provider.Limit{{Kind: "model_five_hour", Scope: "Spark", UsedPercent: 0, ResetsAt: now.Add(5 * time.Hour), Active: true}},
			},
			want: "  + work   ###################.    95%    95%   2d00h\n",
		},
		{
			name: "the Fable cap binds and shows on its own",
			row: Row{Provider: provider.Claude, Account: "work",
				Windows: []provider.Window{
					{Kind: provider.FiveHour, UsedPercent: 30, ResetsAt: now.Add(3 * time.Hour)},
					{Kind: provider.Weekly, UsedPercent: 28, ResetsAt: now.Add(48 * time.Hour)},
				},
				Limits: []provider.Limit{{Kind: "weekly_scoped", Scope: fableScope, UsedPercent: 51, ResetsAt: now.Add(48 * time.Hour), Active: true}},
			},
			want: "  ~ work   ##########..........    49%    72%   2d00h     70%   3h00m     49%   2d00h\n",
		},
		{
			name: "a scope-less active limit binds",
			row: Row{Provider: provider.Codex, Account: "work",
				Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 10, ResetsAt: now.Add(48 * time.Hour)}},
				Limits:  []provider.Limit{{Kind: "account_30d", UsedPercent: 80, ResetsAt: now.Add(400 * time.Hour), Active: true}},
			},
			want: "  ~ work   ####................    20%    90%   2d00h\n",
		},
		{
			name: "an inactive limit never binds",
			row: Row{Provider: provider.Codex, Account: "work",
				Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 10, ResetsAt: now.Add(48 * time.Hour)}},
				Limits:  []provider.Limit{{Kind: "model_weekly", Scope: "Spark", UsedPercent: 98, ResetsAt: now.Add(400 * time.Hour), Active: false}},
			},
			want: "  + work   ##################..    90%    90%   2d00h\n",
		},
		{
			name: "no reset known leaves the countdown empty",
			row: Row{Provider: provider.Codex, Account: "work",
				Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 10}},
			},
			want: "  + work   ##################..    90%    90%\n",
		},
		{
			name: "no meters at all is full headroom",
			row:  Row{Provider: provider.Codex, Account: "work"},
			want: "  + work   ####################   100%\n",
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			got := render(t, []Row{scenario.row}, Options{Plain: true, Width: 100, Now: now})
			if !strings.Contains(got, scenario.want) {
				t.Fatalf("Table() output missing %q:\n%s", scenario.want, got)
			}
		})
	}
}

func TestTableBlanksTheColumnsAnAccountDoesNotHave(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	rows := []Row{
		{Provider: provider.Claude, Account: "opus",
			Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 40, ResetsAt: now.Add(48 * time.Hour)}},
			Limits:  []provider.Limit{{Kind: "weekly_scoped", Scope: `{"model":{"id":null,"display_name":"Opus"},"surface":null}`, UsedPercent: 90, ResetsAt: now.Add(48 * time.Hour), Active: true}},
		},
		{Provider: provider.Codex, Account: "spent",
			Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 40, ResetsAt: now.Add(48 * time.Hour)}},
		},
	}
	got := render(t, rows, Options{Plain: true, Width: 100, Now: now})
	for _, want := range []string{
		"  ~ opus    ##..................    10%    60%   2d00h\n",
		"  + spent   ############........    60%    60%   2d00h\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Table() output missing %q:\n%s", want, got)
		}
	}
}

func TestTableSortsMostRoomFirstWithinEachProvider(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	rows := []Row{
		{Provider: provider.Claude, Account: "low", Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 90}}},
		{Provider: provider.Codex, Account: "mid", Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 50}}},
		{Provider: provider.Claude, Account: "high", Active: true, Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 10}}},
		{Provider: provider.Claude, Account: "mid", Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 50}}},
	}
	got := render(t, rows, Options{Plain: true, Width: 100, Now: now})
	want := "CLAUDE\n" +
		"    ACCOUNT   HEADROOM                      WEEK            5 HOUR          FABLE\n" +
		"> + high   ##################..    90%    90%\n" +
		"  + mid    ##########..........    50%    50%\n" +
		"  ~ low    ##..................    10%    10%\n" +
		"\n" +
		"CODEX\n" +
		"    ACCOUNT   HEADROOM                      WEEK            5 HOUR          RESETS\n" +
		"  + mid    ##########..........    50%    50%\n"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("Table() output mismatch\n got:\n%s\nwant prefix:\n%s", got, want)
	}
}

func TestTableShortensLongAccountNamesDeliberately(t *testing.T) {
	t.Parallel()

	now := fixedNow()
	rows := []Row{{Provider: provider.Claude, Account: "a-very-long-account-name-indeed",
		Windows: []provider.Window{{Kind: provider.FiveHour, UsedPercent: 20, ResetsAt: now.Add(time.Hour)}},
	}}
	plain := render(t, rows, Options{Plain: true, Width: 100, Now: now})
	if !strings.Contains(plain, "  + a-very-long-acc...   ################....    80%                    80%   1h00m\n") {
		t.Fatalf("Table() plain output did not shorten the name:\n%s", plain)
	}
	colored := ansiPattern.ReplaceAllString(render(t, rows, Options{Color: true, Width: 100, Now: now}), "")
	if !strings.Contains(colored, "  ● a-very-long-accou…   ████████████████░░░░    80%                    80%   1h00m\n") {
		t.Fatalf("Table() color output did not shorten the name:\n%s", colored)
	}
}

func TestBarRoundsToTheNearestCell(t *testing.T) {
	t.Parallel()

	for _, scenario := range []struct {
		left int
		want string
	}{
		{left: 100, want: "####################"},
		{left: 83, want: "#################..."},
		{left: 49, want: "##########.........."},
		{left: 8, want: "##.................."},
		{left: 2, want: "...................."},
		{left: 0, want: "...................."},
	} {
		if got := bar(scenario.left, Options{Plain: true}); got != scenario.want {
			t.Fatalf("bar(%d) = %q, want %q", scenario.left, got, scenario.want)
		}
	}
}

func TestTableEmptyIsTheEnrollmentStep(t *testing.T) {
	t.Parallel()

	if got := render(t, nil, Options{Plain: true, Width: 100}); got != NoAccountsEnrolled {
		t.Fatalf("Table() with no rows = %q, want %q", got, NoAccountsEnrolled)
	}
}
