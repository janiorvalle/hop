// Package render formats the account glance for terminals.
package render

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/janiorvalle/hop/internal/provider"
)

const (
	maxNameRunes = 18
	barCells     = 20 // one cell per five percent of headroom
	numberWidth  = 4  // fits "100%"
	columnGap    = "   "
	rowPrefixLen = 4 // marker, space, glyph, space
)

// statusWidth is the bar, the gap, and the number together, so ERROR and
// disabled end on the same column the number does.
const statusWidth = barCells + len(columnGap) + numberWidth

const (
	styleReset = "\x1b[0m"
	styleBold  = "\x1b[1m"
	styleDim   = "\x1b[2m"
	styleRed   = "\x1b[31m"
	styleGreen = "\x1b[32m"
	styleAmber = "\x1b[33m"
)

// Problem is why an account has no usage, in the words the attention list
// prints: the fact, then the commands that fix it.
type Problem struct {
	Fact     string
	Commands []string
}

// TokenExpiry warns that the refresh token behind a row is near the end of
// its life. Commands renew it.
type TokenExpiry struct {
	ExpiresAt time.Time
	Severity  string
	Commands  []string
}

var expiryStyles = map[string]string{"warning": styleAmber, "critical": styleRed}

// Row is one account in the rendered glance.
type Row struct {
	Provider           provider.Name
	Account            string
	Active             bool
	Disabled           bool
	Windows            []provider.Window
	Limits             []provider.Limit
	Problem            *Problem
	RefreshTokenExpiry *TokenExpiry
}

// Options controls terminal capabilities without tying rendering to os.Stdout.
type Options struct {
	Color bool
	Plain bool
	Width int
	Now   time.Time
}

// NoAccountsEnrolled is the whole output of any account listing before the first login.
const NoAccountsEnrolled = "No accounts enrolled. Run 'hop login claude work' or 'hop login codex work'.\n"

// Table writes one provider section per provider, one line per account.
//
// The glance answers "which account can I use right now": every line is the
// headroom left at the binding limit, as a bar and a number, with that
// limit's reset. Accounts sort most room first, errors after them, parked
// accounts last. Whatever needs a hand goes in the attention list below the
// tables, one numbered line each, so the rows stay clean.
func Table(writer io.Writer, rows []Row, options Options) error {
	if len(rows) == 0 {
		_, err := io.WriteString(writer, NoAccountsEnrolled)
		return err
	}
	if options.Width <= 0 {
		options.Width = 120
	}
	if options.Now.IsZero() {
		options.Now = time.Now()
	}

	sections := groupByProvider(rows)
	nameWidth := widestName(rows, options)
	var out strings.Builder
	for index, section := range sections {
		if index > 0 {
			out.WriteString("\n")
		}
		writeSection(&out, section, nameWidth, options)
	}
	writeAttention(&out, attentionItems(sections, options), options)
	writeLegend(&out, options)
	_, err := io.WriteString(writer, out.String())
	return err
}

// groupByProvider keeps the providers in first-seen order and sorts each
// section most room first, then errors, then parked accounts.
func groupByProvider(rows []Row) [][]Row {
	var order []provider.Name
	grouped := map[provider.Name][]Row{}
	for _, row := range rows {
		if _, seen := grouped[row.Provider]; !seen {
			order = append(order, row.Provider)
		}
		grouped[row.Provider] = append(grouped[row.Provider], row)
	}
	sections := make([][]Row, 0, len(order))
	for _, name := range order {
		section := grouped[name]
		sort.SliceStable(section, func(i, j int) bool {
			left, right := section[i], section[j]
			if rowRank(left) != rowRank(right) {
				return rowRank(left) < rowRank(right)
			}
			return headroomPercent(left) > headroomPercent(right)
		})
		sections = append(sections, section)
	}
	return sections
}

// rowRank orders usable accounts first, then errors that need a hand, then
// accounts parked on purpose.
func rowRank(row Row) int {
	switch {
	case row.Disabled:
		return 2
	case row.Problem != nil:
		return 1
	default:
		return 0
	}
}

// widestName is shared by every section so the bars line up across
// providers: the glance is read by comparing rows, whichever table they're in.
func widestName(rows []Row, options Options) int {
	widest := 0
	for _, row := range rows {
		if length := utf8.RuneCountInString(shorten(row.Account, maxNameRunes, options.Plain)); length > widest {
			widest = length
		}
	}
	return widest
}

func writeSection(out *strings.Builder, rows []Row, nameWidth int, options Options) {
	out.WriteString(paint(strings.ToUpper(string(rows[0].Provider)), styleBold, options) + "\n")

	header := strings.Repeat(" ", rowPrefixLen) + padCell("ACCOUNT", nameWidth, false) +
		columnGap + padCell("HEADROOM", statusWidth, false) + columnGap + "RESET"
	out.WriteString(paint(strings.TrimRight(header, " "), styleDim, options) + "\n")

	for _, row := range rows {
		out.WriteString(line(row, nameWidth, options))
	}
}

func line(row Row, nameWidth int, options Options) string {
	name := padCell(shorten(row.Account, maxNameRunes, options.Plain), nameWidth, false)
	text := rowPrefix(row, options) + paint(name, nameStyle(row), options) + columnGap + status(row, options)
	if reset, ok := bindingReset(row); ok {
		text += columnGap + paint(Countdown(options.Now, reset), styleDim, options)
	}
	return strings.TrimRight(text, " ") + "\n"
}

func rowPrefix(row Row, options Options) string {
	marker := " "
	if row.Active {
		marker = ">"
	}
	if row.Disabled {
		return marker + "   "
	}
	if row.Problem != nil {
		return marker + " " + paint("!", styleRed, options) + " "
	}
	left := headroomPercent(row)
	return marker + " " + paint(glyphFor(left, options.Plain), severityStyle(left), options) + " "
}

func nameStyle(row Row) string {
	if row.Disabled {
		return styleDim
	}
	return ""
}

// status is the middle of a line: the bar and the number, or the one word
// that replaces them when there is no usage to draw.
func status(row Row, options Options) string {
	if row.Disabled {
		return paint(padCell("disabled", statusWidth, true), styleDim, options)
	}
	if row.Problem != nil {
		return paint(padCell("ERROR", statusWidth, true), styleRed, options)
	}
	left := headroomPercent(row)
	return bar(left, options) + columnGap + paint(fmt.Sprintf("%3d%%", left), severityStyle(left), options)
}

func bar(left int, options Options) string {
	filled := (left*barCells + 50) / 100
	full, empty := "█", "░"
	if options.Plain {
		full, empty = "#", "."
	}
	return paint(strings.Repeat(full, filled), severityStyle(left), options) +
		paint(strings.Repeat(empty, barCells-filled), styleDim, options)
}

// attentionItem is one numbered line under the tables: the segments join
// with "; " and break there when the line is too wide.
type attentionItem struct {
	segments []string
	style    string
}

func attentionItems(sections [][]Row, options Options) []attentionItem {
	var items []attentionItem
	for _, section := range sections {
		for _, row := range section {
			who := string(row.Provider) + "/" + row.Account + ": "
			if row.Problem != nil {
				items = append(items, attentionItem{
					segments: append([]string{who + row.Problem.Fact}, row.Problem.Commands...),
					style:    styleRed,
				})
			}
			expiry := row.RefreshTokenExpiry
			if expiry == nil {
				continue
			}
			style, warned := expiryStyles[expiry.Severity]
			if !warned {
				continue
			}
			items = append(items, attentionItem{
				segments: append([]string{who + expiryFact(expiry, options.Now)}, expiry.Commands...),
				style:    style,
			})
		}
	}
	return items
}

func expiryFact(expiry *TokenExpiry, now time.Time) string {
	if expiry.ExpiresAt.After(now) {
		return "token expires " + Countdown(now, expiry.ExpiresAt)
	}
	return "token expired"
}

func writeAttention(out *strings.Builder, items []attentionItem, options Options) {
	if len(items) == 0 {
		return
	}
	out.WriteString("\n" + paint("attention", styleBold, options) + "\n")
	labelWidth := len(strconv.Itoa(len(items))) + len(". ") + 1
	for index, item := range items {
		label := padCell(strconv.Itoa(index+1)+". ", labelWidth, true)
		for position, text := range attentionLines(item.segments, options.Width-labelWidth) {
			if position > 0 {
				label = strings.Repeat(" ", labelWidth)
			}
			out.WriteString(label + paint(text, item.style, options) + "\n")
		}
	}
}

// attentionLines breaks an item at its semicolons first, and only a segment
// that is too wide on its own breaks between words.
func attentionLines(segments []string, width int) []string {
	var lines []string
	for _, text := range flow(segments, "; ", width) {
		lines = append(lines, flow(strings.Fields(text), " ", width)...)
	}
	return lines
}

// writeLegend is the last line, and it goes when the terminal is too narrow
// for it: the glyphs and numbers on the rows already carry the state.
func writeLegend(out *strings.Builder, options Options) {
	legend := strings.Join([]string{
		glyphFor(100, options.Plain) + " 50-100 plenty",
		glyphFor(49, options.Plain) + " 10-49 tight",
		glyphFor(9, options.Plain) + " 0-9 nearly/full",
		"! error",
		"> active",
	}, columnGap)
	if utf8.RuneCountInString(legend) > options.Width {
		return
	}
	out.WriteString("\n" + paint(legend, styleDim, options) + "\n")
}

// bindingMeter is the tightest meter on the row: every window, and every
// active limit whether scoped to a model or not, since a scope-less
// account-level cap still stops the account. Between meters equally tight,
// the one that resets last binds, because headroom doesn't move until it does.
func bindingMeter(row Row) (usedPercent float64, resetsAt time.Time) {
	usedPercent = -1
	consider := func(used float64, resets time.Time) {
		if used > usedPercent || (used == usedPercent && resets.After(resetsAt)) {
			usedPercent, resetsAt = used, resets
		}
	}
	for _, window := range row.Windows {
		consider(window.UsedPercent, window.ResetsAt)
	}
	for _, limit := range row.Limits {
		if limit.Active {
			consider(limit.UsedPercent, limit.ResetsAt)
		}
	}
	return usedPercent, resetsAt
}

func headroomPercent(row Row) int {
	usedPercent, _ := bindingMeter(row)
	return leftPercent(usedPercent)
}

func bindingReset(row Row) (time.Time, bool) {
	if row.Disabled || row.Problem != nil {
		return time.Time{}, false
	}
	_, resetsAt := bindingMeter(row)
	return resetsAt, !resetsAt.IsZero()
}

func leftPercent(usedPercent float64) int {
	left := math.Round(100 - usedPercent)
	return int(math.Max(0, math.Min(100, left)))
}

func glyphFor(left int, plain bool) string {
	switch {
	case left >= 50:
		if plain {
			return "+"
		}
		return "●"
	case left >= 10:
		if plain {
			return "~"
		}
		return "◐"
	default:
		if plain {
			return "o"
		}
		return "○"
	}
}

func severityStyle(left int) string {
	switch {
	case left >= 50:
		return styleGreen
	case left >= 10:
		return styleAmber
	default:
		return styleRed
	}
}

func paint(text, style string, options Options) string {
	if !options.Color || style == "" {
		return text
	}
	return style + text + styleReset
}

func padCell(text string, width int, rightAlign bool) string {
	padding := width - utf8.RuneCountInString(text)
	if padding <= 0 {
		return text
	}
	if rightAlign {
		return strings.Repeat(" ", padding) + text
	}
	return text + strings.Repeat(" ", padding)
}

// flow packs segments into lines no wider than width, dropping the joiner at
// each break; a single oversized segment still gets its own line.
func flow(segments []string, joiner string, width int) []string {
	if width < 16 {
		width = 16
	}
	var lines []string
	line := ""
	for _, segment := range segments {
		if line == "" {
			line = segment
			continue
		}
		if utf8.RuneCountInString(line)+utf8.RuneCountInString(joiner)+utf8.RuneCountInString(segment) > width {
			lines = append(lines, line)
			line = segment
			continue
		}
		line += joiner + segment
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// Countdown formats how long until resetsAt in the units the glance uses.
func Countdown(now, resetsAt time.Time) string {
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
