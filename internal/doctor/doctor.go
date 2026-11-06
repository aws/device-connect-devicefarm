// Package doctor runs readiness checks: one table, run in order, each check under its own
// timeout, skipped when a check it needs did not pass. Report: a table with one row per
// category and a What to do list, one text row per check, or one JSON object; the first two end
// with a count line.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Status is a row's verdict, lowercase. Only FAIL fails the run.
type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Tag is the bracket form the text report prints: "[PASS]".
func (status Status) Tag() string {
	return "[" + strings.ToUpper(string(status)) + "]"
}

// Category groups checks into one row of the table report.
type Category string

const (
	CategoryLocal        Category = "local"
	CategoryAWS          Category = "aws"
	CategoryNetwork      Category = "network"
	CategoryDependencies Category = "dependencies"
)

// categoryOrder is the table's row order.
var categoryOrder = []Category{CategoryLocal, CategoryAWS, CategoryNetwork, CategoryDependencies}

var categoryTitles = map[Category]string{
	CategoryLocal:        "Local machine",
	CategoryAWS:          "AWS",
	CategoryNetwork:      "Network",
	CategoryDependencies: "Dependencies",
}

// title is the row name. A category outside the table shows its raw id.
func (category Category) title() string {
	if title, known := categoryTitles[category]; known {
		return title
	}
	return string(category)
}

// Check is one readiness check. Category names its table row. Needs lists ids of earlier checks
// that must be PASS or WARN for this one to run. Timeout bounds Run through its context; zero
// means unbounded.
type Check struct {
	ID       string
	Title    string
	Category Category
	Needs    []string
	Timeout  time.Duration
	Run      func(ctx context.Context) Result
}

// Result is what a check returns. Hint is the fix on a WARN or FAIL row: cause, quoted command
// with the row's values, what it changes. The runner drops it on PASS.
type Result struct {
	Status  Status
	Summary string
	Hint    string
}

// Row is one check's outcome in a report.
type Row struct {
	ID       string
	Title    string
	Category Category
	Needs    []string
	Result
	Elapsed time.Duration
}

// Report is one run's rows, in run order.
type Report struct {
	Rows    []Row
	Elapsed time.Duration
}

// Summary counts the rows by status.
type Summary struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

// Summary counts the rows by status.
func (report Report) Summary() Summary {
	var summary Summary
	for _, row := range report.Rows {
		switch row.Status {
		case Pass:
			summary.Pass++
		case Warn:
			summary.Warn++
		case Fail:
			summary.Fail++
		case Skip:
			summary.Skip++
		}
	}
	return summary
}

// Failed counts FAIL rows.
func (report Report) Failed() int {
	return report.Summary().Fail
}

// Run runs checks in order. A check whose need is not PASS or WARN is skipped; its summary
// names the first blocking need. A check that outlives its timeout fails.
func Run(ctx context.Context, checks []Check) Report {
	started := time.Now()
	report := Report{Rows: make([]Row, 0, len(checks))}
	statuses := make(map[string]Status, len(checks))
	for _, check := range checks {
		row := Row{ID: check.ID, Title: check.Title, Category: check.Category, Needs: check.Needs}
		if blocker := firstBlockingNeed(check.Needs, statuses); blocker != "" {
			row.Result = Result{Status: Skip, Summary: "needs " + blocker}
		} else {
			checkStarted := time.Now()
			row.Result = runBounded(ctx, check)
			row.Elapsed = time.Since(checkStarted)
		}
		if row.Status == Pass {
			row.Hint = ""
		}
		statuses[check.ID] = row.Status
		report.Rows = append(report.Rows, row)
	}
	report.Elapsed = time.Since(started)
	return report
}

// firstBlockingNeed returns the first need whose row is neither PASS nor WARN, or "" when
// none blocks. A need that never ran blocks too.
func firstBlockingNeed(needs []string, statuses map[string]Status) string {
	for _, need := range needs {
		if status := statuses[need]; status != Pass && status != Warn {
			return need
		}
	}
	return ""
}

// runBounded runs one check on its own goroutine under its timeout, so a check that ignores
// its context cannot hang the run. Once the context ends the verdict is the context's: a
// result that lands with or after the deadline is dropped. A dead run context starts nothing.
func runBounded(ctx context.Context, check Check) Result {
	if ctx.Err() != nil {
		return Result{Status: Fail, Summary: ctx.Err().Error()}
	}
	runCtx := ctx
	if check.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, check.Timeout)
		defer cancel()
	}

	results := make(chan Result, 1)
	go func() { results <- check.Run(runCtx) }()
	select {
	case result := <-results:
		if runCtx.Err() == nil {
			return result
		}
	case <-runCtx.Done():
	}
	if ctx.Err() != nil {
		return Result{Status: Fail, Summary: ctx.Err().Error()}
	}
	return Result{Status: Fail, Summary: fmt.Sprintf("timed out after %s", check.Timeout)}
}

// WriteText writes one row per check, `[STATUS] <id>: <summary>[; fix: <hint>]`, then the
// count line. ASCII only, no color.
func WriteText(writer io.Writer, report Report) error {
	var text strings.Builder
	for _, row := range report.Rows {
		fmt.Fprintf(&text, "%s %s: %s", row.Status.Tag(), row.ID, row.Summary)
		if row.Hint != "" {
			fmt.Fprintf(&text, "; fix: %s", row.Hint)
		}
		text.WriteByte('\n')
	}
	summary := report.Summary()
	fmt.Fprintf(&text, "doctor: %d passed, %d warnings, %d failed, %d skipped\n",
		summary.Pass, summary.Warn, summary.Fail, summary.Skip)
	_, err := io.WriteString(writer, text.String())
	return err
}

// CategoryReport is one table row: a category's checks in run order and their roll-up.
type CategoryReport struct {
	ID     Category
	Title  string
	Status Status
	Rows   []Row
}

// Categories groups the rows by category in table order, then one row per category outside
// the table, so no check vanishes. Roll-up: FAIL if any check is FAIL, else WARN if any is
// WARN, else PASS if any is PASS, else SKIP.
func (report Report) Categories() []CategoryReport {
	grouped := make(map[Category][]Row, len(categoryOrder))
	var extra []Category
	for _, row := range report.Rows {
		if _, known := categoryTitles[row.Category]; !known && grouped[row.Category] == nil {
			extra = append(extra, row.Category)
		}
		grouped[row.Category] = append(grouped[row.Category], row)
	}
	categories := make([]CategoryReport, 0, len(categoryOrder)+len(extra))
	for _, id := range slices.Concat(categoryOrder, extra) {
		categories = append(categories, CategoryReport{ID: id, Title: id.title(), Status: rollUp(grouped[id]), Rows: grouped[id]})
	}
	return categories
}

func rollUp(rows []Row) Status {
	status := Skip
	for _, row := range rows {
		switch {
		case row.Status == Fail:
			return Fail
		case row.Status == Warn:
			status = Warn
		case row.Status == Pass && status == Skip:
			status = Pass
		}
	}
	return status
}

// Table layout. Category and Status are fixed columns; Details takes the rest. Width clamp:
// below the floor a path no longer fits a Details line, above the ceiling prose is hard to follow.
const (
	tableMinWidth       = 60
	tableMaxWidth       = 120
	categoryColumnWidth = 13
	statusColumnWidth   = 6
	tableFrameWidth     = 10 // four borders, six padding spaces
	cellIndent          = 2
	itemIndent          = 3
)

// WriteTable writes one ASCII table row per category (Category, Status, Details), a numbered
// What to do list with the hint of every WARN or FAIL check, then the count line. width is the
// terminal width, clamped to [tableMinWidth, tableMaxWidth]. ASCII only, no color.
func WriteTable(writer io.Writer, report Report, width int) error {
	width = min(max(width, tableMinWidth), tableMaxWidth)
	detailsWidth := width - categoryColumnWidth - statusColumnWidth - tableFrameWidth
	rule := "+" + strings.Repeat("-", categoryColumnWidth+2) + "+" + strings.Repeat("-", statusColumnWidth+2) +
		"+" + strings.Repeat("-", detailsWidth+2) + "+\n"
	formatRow := func(category, status, details string) string {
		return fmt.Sprintf("| %-*s | %-*s | %-*s |\n", categoryColumnWidth, category, statusColumnWidth, status, detailsWidth, details)
	}

	var text strings.Builder
	text.WriteString(rule)
	text.WriteString(formatRow("Category", "Status", "Details"))
	text.WriteString(rule)
	categories := report.Categories()
	reasons := skipReasons(report)
	for _, category := range categories {
		label := wrap(category.Title, categoryColumnWidth, 0, false)
		var cell []string
		for _, paragraph := range detailsParagraphs(category.Rows, reasons) {
			cell = append(cell, wrap(paragraph, detailsWidth, cellIndent, false)...)
		}
		status := strings.ToUpper(string(category.Status))
		for index := range max(len(label), len(cell)) {
			text.WriteString(formatRow(lineAt(label, index), status, lineAt(cell, index)))
			status = ""
		}
		text.WriteString(rule)
	}
	if items := whatToDo(report, width); len(items) > 0 {
		text.WriteString("\nWhat to do\n")
		for _, item := range items {
			text.WriteString(item + "\n")
		}
	}
	text.WriteString(countLine(categories) + "\n")
	_, err := io.WriteString(writer, text.String())
	return err
}

// lineAt is lines[index], or "" past the end.
func lineAt(lines []string, index int) string {
	if index < len(lines) {
		return lines[index]
	}
	return ""
}

// detailsParagraphs builds a category's Details cell: "<title>: <summary>" per WARN or FAIL
// check, the passed titles ("also passed: " prefixed after a WARN or FAIL), then "<title> not
// checked: <reason>" per skipped check. Every check SKIP with one shared reason: a single
// "not checked: <reason>".
func detailsParagraphs(rows []Row, reasons map[string]string) []string {
	var paragraphs, passed []string
	var skipped []Row
	for _, row := range rows {
		switch row.Status {
		case Pass:
			passed = append(passed, row.Title)
		case Skip:
			skipped = append(skipped, row)
		default:
			paragraphs = append(paragraphs, row.Title+": "+row.Summary)
		}
	}
	if len(paragraphs) == 0 && len(passed) == 0 {
		if reason, shared := sharedReason(skipped, reasons); shared {
			return []string{notChecked("", reason)}
		}
	}
	switch {
	case len(paragraphs) > 0 && len(passed) > 0:
		paragraphs = append(paragraphs, "also passed: "+strings.Join(passed, ", "))
	case len(passed) > 0:
		paragraphs = append(paragraphs, strings.Join(passed, ", "))
	}
	for _, row := range skipped {
		paragraphs = append(paragraphs, notChecked(row.Title, reasons[row.ID]))
	}
	return paragraphs
}

// sharedReason is the one reason every skipped row shares, "" for no rows.
func sharedReason(skipped []Row, reasons map[string]string) (string, bool) {
	var shared string
	for index, row := range skipped {
		if index == 0 {
			shared = reasons[row.ID]
		} else if reasons[row.ID] != shared {
			return "", false
		}
	}
	return shared, true
}

// notChecked is "<title> not checked[: <reason>]"; an empty title or reason drops its part.
func notChecked(title, reason string) string {
	text := strings.TrimSpace(title + " not checked")
	if reason != "" {
		text += ": " + reason
	}
	return text
}

// skipReasons resolves the reason of every SKIP row by id. An ordered scan recomputes each
// SKIP row's first blocking need as the runner saw it; "" means the check wrote the SKIP itself
// and its summary is the reason. A runner skip is walked along those blockers to its root:
// "needs <root title>" for a check-authored SKIP root, "<root title> failed" for a FAIL root, ""
// when the root has no row or the chain loops.
func skipReasons(report Report) map[string]string {
	rows := make(map[string]Row, len(report.Rows))
	statuses := make(map[string]Status, len(report.Rows))
	blockers := make(map[string]string)
	for _, row := range report.Rows {
		rows[row.ID] = row
		if row.Status == Skip {
			blockers[row.ID] = firstBlockingNeed(row.Needs, statuses)
		}
		statuses[row.ID] = row.Status
	}
	reasons := make(map[string]string, len(blockers))
	for id, blocker := range blockers {
		if blocker == "" {
			reasons[id] = rows[id].Summary
		} else {
			reasons[id] = rootReason(blocker, rows, blockers)
		}
	}
	return reasons
}

// rootReason follows a runner skip's blockers to their root. The walk is bounded by the row
// count, so a malformed needs cycle ends in "".
func rootReason(blocker string, rows map[string]Row, blockers map[string]string) string {
	for range len(rows) {
		root, found := rows[blocker]
		if !found {
			return ""
		}
		if root.Status == Fail {
			return root.Title + " failed"
		}
		next := blockers[root.ID]
		if next == "" {
			return "needs " + root.Title
		}
		blocker = next
	}
	return ""
}

// whatToDo renders one numbered item per WARN or FAIL check with a hint, in run order:
// "N. <category>, <title>: <hint>", wrapped at the full width with a hanging indent.
func whatToDo(report Report, width int) []string {
	var lines []string
	number := 0
	for _, row := range report.Rows {
		if row.Hint == "" || (row.Status != Warn && row.Status != Fail) {
			continue
		}
		number++
		item := fmt.Sprintf("%d. %s, %s: %s", number, row.Category.title(), row.Title, row.Hint)
		lines = append(lines, wrap(item, width, itemIndent, true)...)
	}
	return lines
}

// countLine is "doctor: N of M categories passed[, N warning(s)][, N failed][, N not checked]".
func countLine(categories []CategoryReport) string {
	counts := make(map[Status]int)
	for _, category := range categories {
		counts[category.Status]++
	}
	parts := []string{fmt.Sprintf("doctor: %d of %d categories passed", counts[Pass], len(categories))}
	switch warnings := counts[Warn]; {
	case warnings == 1:
		parts = append(parts, "1 warning")
	case warnings > 1:
		parts = append(parts, fmt.Sprintf("%d warnings", warnings))
	}
	if counts[Fail] > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", counts[Fail]))
	}
	if counts[Skip] > 0 {
		parts = append(parts, fmt.Sprintf("%d not checked", counts[Skip]))
	}
	return strings.Join(parts, ", ")
}

// wrap breaks text into lines of at most width runes; continuation lines indent. keepWhole
// (the list) never cuts a token: one longer than the line stands alone and overflows, so a
// quoted command copies whole. Without it (a cell) a quoted span longer than a continuation
// line wraps as prose and a spaceless token longer than the line is cut at the edge and
// continues on the next line.
func wrap(text string, width, indent int, keepWhole bool) []string {
	continuation := max(width-indent, 1)
	var lines []string
	line, capacity := "", width
	flush := func() {
		lines = append(lines, line)
		line, capacity = "", continuation
	}
	for _, token := range tokens(text, continuation, keepWhole) {
		if line != "" && utf8.RuneCountInString(line)+1+utf8.RuneCountInString(token) > capacity {
			flush()
		}
		for !keepWhole && utf8.RuneCountInString(token) > capacity {
			line, token = cutRunes(token, capacity)
			flush()
		}
		switch {
		case token == "":
		case line == "":
			line = token
		default:
			line += " " + token
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return []string{""}
	}
	for index := 1; index < len(lines); index++ {
		lines[index] = strings.Repeat(" ", indent) + lines[index]
	}
	return lines
}

// tokens splits text at whitespace. A quoted span is one token, its inner spaces kept, when
// keepWhole is set or it fits limit; else its words are tokens.
func tokens(text string, limit int, keepWhole bool) []string {
	var out []string
	rest := strings.TrimLeftFunc(text, unicode.IsSpace)
	for rest != "" {
		end := quotedSpanEnd(rest)
		if end == 0 {
			if end = strings.IndexFunc(rest, unicode.IsSpace); end < 0 {
				end = len(rest)
			}
		}
		token := rest[:end]
		if !keepWhole && utf8.RuneCountInString(token) > limit && strings.ContainsFunc(token, unicode.IsSpace) {
			out = append(out, strings.Fields(token)...)
		} else {
			out = append(out, token)
		}
		rest = strings.TrimLeftFunc(rest[end:], unicode.IsSpace)
	}
	return out
}

// quotedSpanEnd is the length of the quoted span that opens text, or 0 when text does not open
// one: a quote, no quote inside, a closing quote with an optional . , ; or : attached, then
// whitespace or the end. "server's" opens none.
func quotedSpanEnd(text string) int {
	if text == "" || text[0] != '\'' {
		return 0
	}
	closing := strings.IndexByte(text[1:], '\'')
	if closing < 0 {
		return 0
	}
	end := closing + 2
	if end < len(text) && strings.IndexByte(".,;:", text[end]) >= 0 {
		end++
	}
	next, _ := utf8.DecodeRuneInString(text[end:])
	if end == len(text) || unicode.IsSpace(next) {
		return end
	}
	return 0
}

// cutRunes splits token after count runes; a shorter token comes back whole.
func cutRunes(token string, count int) (head, tail string) {
	offset := 0
	for range count {
		_, size := utf8.DecodeRuneInString(token[offset:])
		offset += size
	}
	return token[:offset], token[offset:]
}

// jsonReport is the --json shape.
type jsonReport struct {
	OK         bool        `json:"ok"`
	SessionARN string      `json:"sessionArn"`
	Checks     []jsonCheck `json:"checks"`
	Summary    Summary     `json:"summary"`
}

// jsonCheck is one row. Hint is absent when the row has none: every PASS, and a FAIL from a
// timeout or a cancel.
type jsonCheck struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Category  Category `json:"category"`
	Status    Status   `json:"status"`
	Summary   string   `json:"summary"`
	Hint      string   `json:"hint,omitempty"`
	Needs     []string `json:"needs"`
	ElapsedMs int64    `json:"elapsedMs"`
}

// WriteJSON writes the report as one compact JSON line. ok is Failed() == 0.
func WriteJSON(writer io.Writer, report Report, sessionARN string) error {
	out := jsonReport{
		OK:         report.Failed() == 0,
		SessionARN: sessionARN,
		Checks:     make([]jsonCheck, 0, len(report.Rows)),
		Summary:    report.Summary(),
	}
	for _, row := range report.Rows {
		out.Checks = append(out.Checks, jsonCheck{ID: row.ID, Title: row.Title, Category: row.Category, Status: row.Status,
			Summary: row.Summary, Hint: row.Hint, Needs: nonNil(row.Needs), ElapsedMs: row.Elapsed.Milliseconds()})
	}
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false) // not HTML: keep && in hints literal
	return encoder.Encode(out)
}

// nonNil makes a nil list encode as [], not null.
func nonNil(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}
