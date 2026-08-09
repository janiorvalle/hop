// Package render formats the account glance for terminals.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/janiorvalle/hop/internal/provider"
)

const (
	barWidth     = 8
	narrowWidth  = 88
	maxNameRunes = 18
)

// Problem is the human-facing recovery step for an account row.
type Problem struct {
	Message string
	Action  string
}

// Row is one account in the rendered glance.
type Row struct {
	Provider provider.Name
	Account  string
	Active   bool
	Plan     string
	Windows  []provider.Window
	Limits   []provider.Limit
	Problem  *Problem
}

// Options controls terminal capabilities without tying rendering to os.Stdout.
type Options struct {
	Color bool
	Plain bool
	Width int
	Now   time.Time
}

// Table writes provider sections and usage rows.
func Table(writer io.Writer, rows []Row, options Options) error {
	if len(rows) == 0 {
		_, err := io.WriteString(writer, "No accounts enrolled. Run 'hop login claude work' or 'hop login codex work'.\n")
		return err
	}
	if options.Width <= 0 {
		options.Width = 120
	}
	if options.Now.IsZero() {
		options.Now = time.Now()
	}

	var previous provider.Name
	for _, row := range rows {
		if row.Provider != previous {
			if previous != "" {
				if _, err := io.WriteString(writer, "\n"); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(writer, "%s\n", strings.ToUpper(string(row.Provider))); err != nil {
				return err
			}
			previous = row.Provider
		}
		if err := renderRow(writer, row, options); err != nil {
			return err
		}
	}
	return nil
}

func renderRow(writer io.Writer, row Row, options Options) error {
	marker := "○"
	if row.Active {
		marker = "●"
	}
	if options.Plain {
		marker = "-"
		if row.Active {
			marker = "*"
		}
	}
	name := shorten(row.Account, maxNameRunes, options.Plain)
	if row.Problem != nil {
		if options.Width < narrowWidth {
			_, err := fmt.Fprintf(writer, "%s %s ! %s\n  %s\n", marker, name, row.Problem.Message, row.Problem.Action)
			return err
		}
		_, err := fmt.Fprintf(writer, "%s %-18s ! %s %s\n", marker, name, row.Problem.Message, row.Problem.Action)
		return err
	}

	fiveHour, hasFiveHour := findWindow(row.Windows, provider.FiveHour)
	weekly, hasWeekly := findWindow(row.Windows, provider.Weekly)
	if options.Width < narrowWidth {
		if _, err := fmt.Fprintf(writer, "%s %s%s\n", marker, name, planSuffix(row.Plan)); err != nil {
			return err
		}
		if hasFiveHour {
			if _, err := fmt.Fprintf(writer, "  %s\n", meter("5h", fiveHour.UsedPercent, fiveHour.ResetsAt, options)); err != nil {
				return err
			}
		}
		if hasWeekly {
			if _, err := fmt.Fprintf(writer, "  %s\n", meter("wk", weekly.UsedPercent, weekly.ResetsAt, options)); err != nil {
				return err
			}
		}
	} else {
		parts := make([]string, 0, 2)
		if hasFiveHour {
			parts = append(parts, meter("5h", fiveHour.UsedPercent, fiveHour.ResetsAt, options))
		}
		if hasWeekly {
			parts = append(parts, meter("wk", weekly.UsedPercent, weekly.ResetsAt, options))
		}
		if _, err := fmt.Fprintf(writer, "%s %-18s %s%s\n", marker, name, strings.Join(parts, "   "), planSuffix(row.Plan)); err != nil {
			return err
		}
	}

	for _, limit := range row.Limits {
		if !limit.Active || limit.Scope == "" {
			continue
		}
		label := "cap"
		if strings.Contains(limit.Kind, "five_hour") {
			label = "5h"
		} else if strings.Contains(limit.Kind, "weekly") {
			label = "wk"
		}
		if _, err := fmt.Fprintf(writer, "  binding %-12s %s\n", shorten(scopeLabel(limit.Scope), 12, options.Plain), meter(label, limit.UsedPercent, limit.ResetsAt, options)); err != nil {
			return err
		}
	}
	return nil
}

func scopeLabel(scope string) string {
	var structured struct {
		Model struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	}
	if json.Unmarshal([]byte(scope), &structured) == nil && structured.Model.DisplayName != "" {
		return structured.Model.DisplayName
	}
	return scope
}

func findWindow(windows []provider.Window, kind provider.WindowKind) (provider.Window, bool) {
	for _, window := range windows {
		if window.Kind == kind {
			return window, true
		}
	}
	return provider.Window{}, false
}

func meter(label string, percent float64, resetsAt time.Time, options Options) string {
	bar := usageBar(percent, options.Plain)
	value := fmt.Sprintf("%s [%s] %3.0f%%", label, bar, percent)
	if options.Color {
		value = severityColor(percent) + value + "\x1b[0m"
	}
	if resetsAt.IsZero() {
		return value
	}
	return fmt.Sprintf("%s regen %s", value, countdown(options.Now, resetsAt))
}

func usageBar(percent float64, plain bool) string {
	bounded := math.Max(0, math.Min(100, percent))
	filled := int(math.Round(bounded / 100 * barWidth))
	full, empty := "█", "░"
	if plain {
		full, empty = "#", "-"
	}
	return strings.Repeat(full, filled) + strings.Repeat(empty, barWidth-filled)
}

func severityColor(percent float64) string {
	switch {
	case percent >= 90:
		return "\x1b[31m"
	case percent >= 70:
		return "\x1b[33m"
	default:
		return "\x1b[32m"
	}
}

func countdown(now, resetsAt time.Time) string {
	remaining := resetsAt.Sub(now)
	if remaining <= 0 {
		return "now"
	}
	remaining = remaining.Round(time.Minute)
	if remaining < time.Hour {
		return fmt.Sprintf("%dm", int(remaining.Minutes()))
	}
	if remaining < 24*time.Hour {
		return fmt.Sprintf("%dh%02dm", int(remaining.Hours()), int(remaining.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(remaining.Hours())/24, int(remaining.Hours())%24)
}

func planSuffix(plan string) string {
	if plan == "" {
		return ""
	}
	return "  (" + plan + ")"
}

func shorten(value string, maxRunes int, plain bool) string {
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	if plain {
		return string(runes[:maxRunes-3]) + "..."
	}
	return string(runes[:maxRunes-1]) + "…"
}
