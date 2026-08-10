// Package render formats the account glance for terminals.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/janiorvalle/hop/internal/provider"
)

const (
	narrowWidth       = 88
	maxNameRunes      = 18
	headroomCellWidth = 9 // fits "100% LEFT"
	columnGap         = "   "
	guidanceIndent    = "    "
	maxGuidanceWidth  = 96
)

const (
	styleReset = "\x1b[0m"
	styleBold  = "\x1b[1m"
	styleDim   = "\x1b[2m"
	styleRed   = "\x1b[31m"
	styleGreen = "\x1b[32m"
	styleAmber = "\x1b[33m"
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

// Table writes provider sections with one headroom-first row per account.
//
// The glance answers "which account can I use right now": every percentage is
// capacity LEFT (100 - used), the headline is the headroom at the tightest
// binding limit, and accounts sort most-usable first with error rows last.
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

	sections := groupByProvider(rows)
	wide := options.Width >= narrowWidth
	body := renderSections(sections, wide, options)
	if wide && maxLineWidth(body) > options.Width {
		wide = false
		body = renderSections(sections, wide, options)
	}

	var out strings.Builder
	writeLegend(&out, wide, options)
	out.WriteString(body)
	_, err := io.WriteString(writer, out.String())
	return err
}

func renderSections(sections [][]Row, wide bool, options Options) string {
	var out strings.Builder
	for index, sectionRows := range sections {
		if index > 0 {
			out.WriteString("\n")
		}
		built := newSection(sectionRows)
		if wide {
			writeWideSection(&out, built, options)
		} else {
			writeNarrowSection(&out, built, options)
		}
	}
	return out.String()
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func maxLineWidth(body string) int {
	widest := 0
	for _, line := range strings.Split(body, "\n") {
		if width := utf8.RuneCountInString(ansiPattern.ReplaceAllString(line, "")); width > widest {
			widest = width
		}
	}
	return widest
}

func writeLegend(out *strings.Builder, wide bool, options Options) {
	definition := []string{"HEADROOM = capacity left at the binding limit"}
	if !wide {
		definition = []string{"HEADROOM = left at binding cap", "detail = left / resets in", "* binding"}
	}
	glyphs := []string{
		glyphFor(100, options.Plain) + " 50-100 plenty",
		glyphFor(49, options.Plain) + " 10-49 tight",
		glyphFor(9, options.Plain) + " 0-9 nearly/full",
		"! error",
		"> active",
	}
	for _, line := range flow(definition, "   ", options.Width) {
		out.WriteString(paint(line, styleDim, options) + "\n")
	}
	for _, line := range flow(glyphs, "   ", options.Width) {
		out.WriteString(paint(line, styleDim, options) + "\n")
	}
	out.WriteString("\n")
}

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
		sections = append(sections, grouped[name])
	}
	return sections
}

// section carries the per-provider layout decisions: which columns exist,
// which shared facts hoist into the title, and whether the binding column can
// name its scope once instead of per row.
type section struct {
	rows          []Row
	hoisted       []string
	showFiveHour  bool
	showWeekly    bool
	showBinding   bool
	bindingHeader string
	uniformScope  bool
	showPlan      bool
}

func newSection(rows []Row) section {
	sorted := make([]Row, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool {
		left, right := sorted[i], sorted[j]
		if (left.Problem == nil) != (right.Problem == nil) {
			return left.Problem == nil
		}
		return headroomPercent(left) > headroomPercent(right)
	})

	built := section{rows: sorted, bindingHeader: "BINDING"}
	plans := map[string]bool{}
	scopes := map[string]bool{}
	dataRows := 0
	fiveHourCap := false
	for _, row := range sorted {
		if row.Problem != nil {
			continue
		}
		dataRows++
		plans[row.Plan] = true
		if _, ok := findWindow(row.Windows, provider.FiveHour); ok {
			built.showFiveHour = true
		}
		if _, ok := findWindow(row.Windows, provider.Weekly); ok {
			built.showWeekly = true
		}
		for _, limit := range activeLimits(row) {
			built.showBinding = true
			scopes[scopeLabel(limit.Scope)+" / "+limitKindLabel(limit.Kind)] = true
			if strings.Contains(limit.Kind, "five_hour") {
				fiveHourCap = true
			}
		}
	}
	if dataRows > 0 && len(plans) == 1 && !plans[""] {
		for plan := range plans {
			built.hoisted = append(built.hoisted, plan)
		}
	} else {
		for plan := range plans {
			if plan != "" {
				built.showPlan = true
			}
		}
	}
	if dataRows > 0 && !built.showFiveHour && !fiveHourCap {
		built.hoisted = append(built.hoisted, "no 5-hour window")
	}
	if len(scopes) == 1 {
		for scope := range scopes {
			built.bindingHeader = "BINDING: " + scope
		}
		built.uniformScope = true
	}
	return built
}

func writeSectionTitle(out *strings.Builder, built section, options Options) {
	title := paint(strings.ToUpper(string(built.rows[0].Provider)), styleBold, options)
	if len(built.hoisted) > 0 {
		joiner := "  " + midDot(options) + "  "
		title += paint(joiner+strings.Join(built.hoisted, joiner), styleDim, options)
	}
	out.WriteString(title + "\n")
}

func writeWideSection(out *strings.Builder, built section, options Options) {
	writeSectionTitle(out, built, options)

	headers := []string{"ACCOUNT", "HEADROOM"}
	if built.showFiveHour {
		headers = append(headers, "5 HOUR")
	}
	if built.showWeekly {
		headers = append(headers, "WEEKLY")
	}
	if built.showBinding {
		headers = append(headers, built.bindingHeader)
	}
	if built.showPlan {
		headers = append(headers, "PLAN")
	}

	cellsByRow := make([][]string, len(built.rows))
	for index, row := range built.rows {
		cellsByRow[index] = wideCells(row, built, options)
	}
	widths := make([]int, len(headers))
	for column, header := range headers {
		widths[column] = utf8.RuneCountInString(header)
		for _, cells := range cellsByRow {
			if column < len(cells) && utf8.RuneCountInString(cells[column]) > widths[column] {
				widths[column] = utf8.RuneCountInString(cells[column])
			}
		}
	}

	headerLine := guidanceIndent
	for column, header := range headers {
		if column > 0 {
			headerLine += columnGap
		}
		headerLine += padCell(header, widths[column], column == 1)
	}
	out.WriteString(paint(strings.TrimRight(headerLine, " "), styleDim, options) + "\n")

	for index, row := range built.rows {
		out.WriteString(wideLine(row, cellsByRow[index], widths, options))
		if row.Problem != nil {
			writeGuidance(out, row.Problem, options)
		}
	}
}

// wideCells builds one row's column values in header order; error rows fill
// only the account and headroom columns.
func wideCells(row Row, built section, options Options) []string {
	name := shorten(row.Account, maxNameRunes, options.Plain)
	if row.Problem != nil {
		return []string{name, fmt.Sprintf("%*s", headroomCellWidth, "ERROR")}
	}
	cells := []string{name, fmt.Sprintf("%3d%% LEFT", headroomPercent(row))}
	if built.showFiveHour {
		cells = append(cells, windowCell(row, provider.FiveHour, options))
	}
	if built.showWeekly {
		cells = append(cells, windowCell(row, provider.Weekly, options))
	}
	if built.showBinding {
		cells = append(cells, bindingCell(row, built.uniformScope, options))
	}
	if built.showPlan {
		cells = append(cells, planCell(row.Plan, options))
	}
	return cells
}

func wideLine(row Row, cells []string, widths []int, options Options) string {
	line := rowPrefix(row, options)
	for column, cell := range cells {
		if column > 0 {
			line += columnGap
		}
		padded := cell
		if column < len(cells)-1 {
			padded = padCell(cell, widths[column], column == 1)
		}
		line += paint(padded, cellStyle(row, column), options)
	}
	for _, extra := range extraLimitCells(row, options) {
		line += columnGap + paint(extra, styleDim, options)
	}
	return strings.TrimRight(line, " ") + "\n"
}

func writeNarrowSection(out *strings.Builder, built section, options Options) {
	writeSectionTitle(out, built, options)

	nameWidth := 0
	for _, row := range built.rows {
		if length := utf8.RuneCountInString(shorten(row.Account, maxNameRunes, options.Plain)); length > nameWidth {
			nameWidth = length
		}
	}

	for _, row := range built.rows {
		name := padCell(shorten(row.Account, maxNameRunes, options.Plain), nameWidth, false)
		if row.Problem != nil {
			line := rowPrefix(row, options) + name + "  " +
				paint(fmt.Sprintf("%*s", headroomCellWidth, "ERROR"), styleRed, options)
			out.WriteString(strings.TrimRight(line, " ") + "\n")
			writeGuidance(out, row.Problem, options)
			continue
		}
		headroom := fmt.Sprintf("%3d%% LEFT", headroomPercent(row))
		line := rowPrefix(row, options) + name + "  " +
			paint(headroom, severityStyle(headroomPercent(row)), options)
		if row.Active {
			line += paint("  ACTIVE", styleBold, options)
		}
		out.WriteString(line + "\n")
		parts := narrowDetailParts(row, built.showPlan, options)
		for _, detail := range flow(parts, " "+midDot(options)+" ", options.Width-len(guidanceIndent)) {
			out.WriteString(guidanceIndent + paint(detail, styleDim, options) + "\n")
		}
	}
}

func narrowDetailParts(row Row, showPlan bool, options Options) []string {
	parts := make([]string, 0, 4)
	if window, ok := findWindow(row.Windows, provider.FiveHour); ok {
		parts = append(parts, "5h "+narrowValue(window.UsedPercent, window.ResetsAt, options))
	}
	if window, ok := findWindow(row.Windows, provider.Weekly); ok {
		parts = append(parts, "week "+narrowValue(window.UsedPercent, window.ResetsAt, options))
	}
	for _, limit := range activeLimits(row) {
		parts = append(parts, scopeLabel(limit.Scope)+"*"+limitKindTag(limit.Kind)+" "+narrowValue(limit.UsedPercent, limit.ResetsAt, options))
	}
	if showPlan && row.Plan != "" {
		parts = append(parts, row.Plan)
	}
	return parts
}

func narrowValue(usedPercent float64, resetsAt time.Time, options Options) string {
	value := fmt.Sprintf("%d%%", leftPercent(usedPercent))
	if resetsAt.IsZero() {
		return value
	}
	return value + "/" + countdown(options.Now, resetsAt)
}

func writeGuidance(out *strings.Builder, problem *Problem, options Options) {
	text := strings.TrimSpace(problem.Message + " " + problem.Action)
	width := options.Width
	if width > maxGuidanceWidth {
		width = maxGuidanceWidth
	}
	for _, line := range wrap(text, width-len(guidanceIndent)) {
		out.WriteString(guidanceIndent + paint(line, styleRed, options) + "\n")
	}
}

func rowPrefix(row Row, options Options) string {
	marker := " "
	if row.Active {
		marker = ">"
	}
	if row.Problem != nil {
		return marker + " " + paint("!", styleRed, options) + " "
	}
	left := headroomPercent(row)
	return marker + " " + paint(glyphFor(left, options.Plain), severityStyle(left), options) + " "
}

func cellStyle(row Row, column int) string {
	if column == 0 {
		return ""
	}
	if column == 1 {
		if row.Problem != nil {
			return styleRed
		}
		return severityStyle(headroomPercent(row))
	}
	return styleDim
}

func windowCell(row Row, kind provider.WindowKind, options Options) string {
	window, ok := findWindow(row.Windows, kind)
	if !ok {
		return dash(options)
	}
	cell := fmt.Sprintf("%3d%%", leftPercent(window.UsedPercent))
	if window.ResetsAt.IsZero() {
		return cell
	}
	return cell + " " + midDot(options) + " " + countdown(options.Now, window.ResetsAt)
}

// bindingCell shows the tightest active model-scoped limit; the scope name is
// omitted when the section header already carries it.
func bindingCell(row Row, uniformScope bool, options Options) string {
	limits := activeLimits(row)
	if len(limits) == 0 {
		return dash(options)
	}
	tightest := limits[0]
	for _, limit := range limits[1:] {
		if limit.UsedPercent > tightest.UsedPercent {
			tightest = limit
		}
	}
	cell := fmt.Sprintf("%3d%%", leftPercent(tightest.UsedPercent))
	if !tightest.ResetsAt.IsZero() {
		cell += " " + midDot(options) + " " + countdown(options.Now, tightest.ResetsAt)
	}
	if uniformScope {
		return cell
	}
	return scopeLabel(tightest.Scope) + limitKindTag(tightest.Kind) + " " + cell
}

// extraLimitCells renders active limits beyond the tightest so a row with
// several model caps still loses nothing in the wide layout.
func extraLimitCells(row Row, options Options) []string {
	limits := activeLimits(row)
	if len(limits) < 2 {
		return nil
	}
	tightest := 0
	for index, limit := range limits {
		if limit.UsedPercent > limits[tightest].UsedPercent {
			tightest = index
		}
	}
	cells := make([]string, 0, len(limits)-1)
	for index, limit := range limits {
		if index == tightest {
			continue
		}
		cell := fmt.Sprintf("%s%s %d%%", scopeLabel(limit.Scope), limitKindTag(limit.Kind), leftPercent(limit.UsedPercent))
		if !limit.ResetsAt.IsZero() {
			cell += " " + midDot(options) + " " + countdown(options.Now, limit.ResetsAt)
		}
		cells = append(cells, cell)
	}
	return cells
}

func planCell(plan string, options Options) string {
	if plan == "" {
		return dash(options)
	}
	return "(" + plan + ")"
}

func activeLimits(row Row) []provider.Limit {
	limits := make([]provider.Limit, 0, len(row.Limits))
	for _, limit := range row.Limits {
		if limit.Active && limit.Scope != "" {
			limits = append(limits, limit)
		}
	}
	return limits
}

// headroomPercent binds on every active limit, scoped or not; a scope-less
// account-level cap must still pull the headline down even though only
// scoped caps get their own binding column.
func headroomPercent(row Row) int {
	tightest := 0.0
	for _, window := range row.Windows {
		tightest = math.Max(tightest, window.UsedPercent)
	}
	for _, limit := range row.Limits {
		if limit.Active {
			tightest = math.Max(tightest, limit.UsedPercent)
		}
	}
	return leftPercent(tightest)
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

func limitKindLabel(kind string) string {
	if strings.Contains(kind, "five_hour") {
		return "5 HOUR"
	}
	if strings.Contains(kind, "weekly") {
		return "WEEKLY"
	}
	return strings.ToUpper(kind)
}

// limitKindTag marks a cap's window kind wherever no column header carries it;
// weekly is the design's unmarked default.
func limitKindTag(kind string) string {
	if strings.Contains(kind, "five_hour") {
		return "(5h)"
	}
	return ""
}

func midDot(options Options) string {
	if options.Plain {
		return "."
	}
	return "·"
}

func dash(options Options) string {
	if options.Plain {
		return "-"
	}
	return "—"
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

func wrap(text string, width int) []string {
	return flow(strings.Fields(text), " ", width)
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
