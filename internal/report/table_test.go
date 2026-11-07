package report

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// deviceTable is a four-column table with one sized column per value kind and a flexible name.
func deviceTable(name, serial string) Table {
	return Table{
		Columns: []Column{{Title: "Device", Flexible: true}, {Title: "Serial"}, {Title: "Attached"}, {Title: "Session"}},
		Rows: [][]Cell{{
			{name},
			{serial},
			{"12 minutes ago"},
			{"started 25 minutes ago", "expires in 2 hours 5 minutes"},
		}},
	}
}

func TestGivenALongNameWhenRenderedThenItWrapsInsideItsRowAndTheOtherCellsPad(t *testing.T) {
	out := deviceTable("Google Pixel 9 Pro XL Extra Large Deluxe (OS 16)", "R3CN30ABCDE").Render(80)

	assert.Equal(t, `+----------------+-------------+----------------+------------------------------+
| Device         | Serial      | Attached       | Session                      |
+----------------+-------------+----------------+------------------------------+
| Google Pixel 9 | R3CN30ABCDE | 12 minutes ago | started 25 minutes ago       |
|   Pro XL Extra |             |                | expires in 2 hours 5 minutes |
|   Large Deluxe |             |                |                              |
|   (OS 16)      |             |                |                              |
+----------------+-------------+----------------+------------------------------+
`, out)
}

func TestGivenAWidthUnderTheFloorWhenRenderedThenTheWidthIsClampedAndTheWidestSizedColumnsShrinkToFit(t *testing.T) {
	out := deviceTable("Google Pixel 9 (OS 16)", "R3CN30ABCDE").Render(40)

	assert.Equal(t, `+--------------+-------------+--------------+--------------+
| Device       | Serial      | Attached     | Session      |
+--------------+-------------+--------------+--------------+
| Google Pixel | R3CN30ABCDE | 12 minutes   | started 25   |
|   9 (OS 16)  |             | ago          | minutes ago  |
|              |             |              | expires in 2 |
|              |             |              | hours 5      |
|              |             |              | minutes      |
+--------------+-------------+--------------+--------------+
`, out)
}

func TestGivenALongSerialAtTheMinimumWidthWhenRenderedThenTheOtherSizedColumnsShrinkAndTheSerialStaysWhole(t *testing.T) {
	out := deviceTable("Google Pixel 9 (OS 16)", "FAKE-ADB-SERIAL-0001").Render(60)

	assert.Equal(t, `+--------------+----------------------+----------+---------+
| Device       | Serial               | Attached | Session |
+--------------+----------------------+----------+---------+
| Google Pixel | FAKE-ADB-SERIAL-0001 | 12       | started |
|   9 (OS 16)  |                      | minutes  | 25      |
|              |                      | ago      | minutes |
|              |                      |          | ago     |
|              |                      |          | expires |
|              |                      |          | in 2    |
|              |                      |          | hours 5 |
|              |                      |          | minutes |
+--------------+----------------------+----------+---------+
`, out)
}

func TestGivenASerialWiderThanTheSizedBudgetWhenRenderedThenTheNameColumnShrinksUnderTheFloorAndTheFrameHolds(t *testing.T) {
	tests := []struct {
		name   string
		serial string
		want   string // the serial cell's first line
	}{
		{name: "the name column takes what is left and the serial stays whole",
			serial: "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234", want: "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234"},
		{name: "the minimums alone overflow and the serial cuts at the column edge",
			serial: "ABCDEFGHIJKLMNOPQRSTUVWXYZ123456", want: "ABCDEFGHIJKLMNOPQRSTUVWXYZ12345"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := deviceTable("Google Pixel 9 (OS 16)", tc.serial).Render(60)

			for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
				assert.Len(t, line, MinWidth, line)
			}
			assert.Contains(t, out, "| "+tc.want+" |")
		})
	}
}

func TestGivenAWidthOverTheCeilingWhenRenderedThenTheFlexibleColumnTakesTheRestOfTheCeiling(t *testing.T) {
	out := deviceTable("Google Pixel 9 (OS 16)", "R3CN30ABCDE").Render(200)

	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	assert.Len(t, lines, 6)
	for _, line := range lines {
		assert.Len(t, line, MaxWidth, line)
	}
	// The sized columns keep their natural widths; the name takes the rest: 120 - 13 - 53.
	assert.Contains(t, out, fmt.Sprintf("| %-54s | R3CN30ABCDE | 12 minutes ago | started 25 minutes ago       |\n", "Google Pixel 9 (OS 16)"))
}

func TestGivenAPinnedColumnAndEmptyCellsWhenRenderedThenTheColumnKeepsItsWidthAndEveryRowHasALine(t *testing.T) {
	table := Table{
		Columns: []Column{{Title: "Code", Width: 4}, {Title: "Note", Flexible: true}},
		Rows:    [][]Cell{{{"longer than four"}, nil}, {nil, nil}, {{"ok"}, {"a", "b"}}},
	}

	assert.Equal(t, `+------+---------------------------------------------------+
| Code | Note                                              |
+------+---------------------------------------------------+
| long |                                                   |
| er   |                                                   |
| than |                                                   |
| four |                                                   |
+------+---------------------------------------------------+
|      |                                                   |
+------+---------------------------------------------------+
| ok   | a                                                 |
|      | b                                                 |
+------+---------------------------------------------------+
`, table.Render(60))
}

func TestGivenNoFlexibleColumnWhenRenderedThenEveryColumnKeepsItsWidthWhateverTheWidth(t *testing.T) {
	table := Table{
		Columns: []Column{{Title: "Code", Width: 6}, {Title: "Name"}},
		Rows:    [][]Cell{{{"E1"}, {"first error"}}},
	}

	want := `+--------+-------------+
| Code   | Name        |
+--------+-------------+
| E1     | first error |
+--------+-------------+
`
	assert.Equal(t, want, table.Render(MinWidth))
	assert.Equal(t, want, table.Render(MaxWidth))
}
