// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Width bounds. Below the floor a path does not fit one line; above the ceiling prose is hard
// to follow.
const (
	MinWidth = 60
	MaxWidth = 120
)

// Table layout. The flexible column holds prose, so its continuation lines indent; sized columns
// hold values and wrap flush. Sized columns shrink toward their minimum widths to keep the
// flexible column at flexibleFloor runes.
const (
	cellIndent    = 2
	flexibleFloor = 12
)

// Column is one table column. Width pins it; zero sizes it to its content. Flexible marks the
// one column that takes the width the others leave.
type Column struct {
	Title    string
	Width    int
	Flexible bool
}

// Cell is one cell's paragraphs. Each starts a line and wraps at the column width.
type Cell []string

// Table is columns and rows of cells, one cell per column.
type Table struct {
	Columns []Column
	Rows    [][]Cell
}

// ClampWidth bounds a terminal width to [MinWidth, MaxWidth].
func ClampWidth(width int) int {
	return min(max(width, MinWidth), MaxWidth)
}

// Render draws the table at width, clamped: a rule, the titles, a rule, then each row and a
// rule. A row is as tall as its tallest cell; shorter cells pad. ASCII only, no color.
func (table Table) Render(width int) string {
	widths := table.columnWidths(ClampWidth(width))
	var rule strings.Builder
	for _, columnWidth := range widths {
		rule.WriteString("+" + strings.Repeat("-", columnWidth+2))
	}
	rule.WriteString("+\n")

	titles := make([]Cell, len(table.Columns))
	for index, column := range table.Columns {
		titles[index] = Cell{column.Title}
	}
	var text strings.Builder
	text.WriteString(rule.String())
	text.WriteString(table.renderRow(titles, widths))
	text.WriteString(rule.String())
	for _, row := range table.Rows {
		text.WriteString(table.renderRow(row, widths))
		text.WriteString(rule.String())
	}
	return text.String()
}

// renderRow wraps each cell at its column width and pads the shorter cells to the tallest.
func (table Table) renderRow(cells []Cell, widths []int) string {
	lines := make([][]string, len(table.Columns))
	height := 1
	for index, column := range table.Columns {
		indent := 0
		if column.Flexible {
			indent = cellIndent
		}
		for _, paragraph := range cells[index] {
			lines[index] = append(lines[index], Wrap(paragraph, widths[index], indent, false)...)
		}
		height = max(height, len(lines[index]))
	}
	var text strings.Builder
	for lineIndex := range height {
		for index, columnWidth := range widths {
			fmt.Fprintf(&text, "| %-*s ", columnWidth, lineAt(lines[index], lineIndex))
		}
		text.WriteString("|\n")
	}
	return text.String()
}

// lineAt is lines[index], or "" past the end.
func lineAt(lines []string, index int) string {
	if index < len(lines) {
		return lines[index]
	}
	return ""
}

// columnWidths sizes the columns at a clamped width: a pinned column keeps its Width, a sized one
// its natural width, the flexible one takes the rest. While the rest is under flexibleFloor, the
// widest sized column gives up a rune, down to its minimum width, so content wraps instead of
// overflowing and a value never cuts. When the minimums alone do not fit, the sized columns give
// on down to their titles, so a value cuts rather than the frame.
func (table Table) columnWidths(width int) []int {
	widths := make([]int, len(table.Columns))
	minimums := make([]int, len(table.Columns))
	titles := make([]int, len(table.Columns))
	flexible := -1
	for index, column := range table.Columns {
		titles[index] = utf8.RuneCountInString(column.Title)
		switch {
		case column.Flexible:
			flexible = index
		case column.Width > 0:
			widths[index], minimums[index], titles[index] = column.Width, column.Width, column.Width
		default:
			widths[index], minimums[index] = table.contentWidths(index)
		}
	}
	if flexible < 0 {
		return widths
	}
	rest := width - (3*len(table.Columns) + 1) // "| " and " " per column, then the last "|"
	for _, columnWidth := range widths {
		rest -= columnWidth
	}
	for rest < flexibleFloor {
		widest := widestAbove(widths, minimums)
		if widest < 0 && rest < 1 {
			widest = widestAbove(widths, titles)
		}
		if widest < 0 {
			break
		}
		widths[widest]--
		rest++
	}
	widths[flexible] = max(rest, 1)
	return widths
}

// contentWidths sizes a column in runes. natural: the longer of its title and its longest
// paragraph. minimum: the longer of its title and its longest spaceless token, the width a sized
// column keeps when squeezed, so a serial or a number never cuts.
func (table Table) contentWidths(index int) (natural, minimum int) {
	natural = utf8.RuneCountInString(table.Columns[index].Title)
	minimum = natural
	for _, row := range table.Rows {
		for _, paragraph := range row[index] {
			natural = max(natural, utf8.RuneCountInString(paragraph))
			for _, token := range strings.Fields(paragraph) {
				minimum = max(minimum, utf8.RuneCountInString(token))
			}
		}
	}
	return natural, minimum
}

// widestAbove is the widest column still above its floor, the first on a tie, or -1 when none
// can shrink. The flexible column has no width yet, so it never qualifies.
func widestAbove(widths, floors []int) int {
	widest := -1
	for index, width := range widths {
		if width > floors[index] && (widest < 0 || width > widths[widest]) {
			widest = index
		}
	}
	return widest
}
