package report

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGivenLongTokensAndSpansWhenWrappedThenTheCellCutsAndTheListKeepsThemWhole(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		width     int
		indent    int
		keepWhole bool
		want      []string
	}{
		{name: "a span longer than the cell line wraps as prose", text: "see 'adb -P 5037 kill-server' then rerun", width: 20, indent: 2,
			want: []string{"see 'adb -P 5037", "  kill-server' then", "  rerun"}},
		{name: "a span longer than the list line stands alone", text: "see 'adb -P 5037 kill-server' then rerun", width: 20, indent: 3, keepWhole: true,
			want: []string{"see", "   'adb -P 5037 kill-server'", "   then rerun"}},
		{name: "a span that fits a continuation line moves whole", text: "stop it with 'adb kill-server' now", width: 20, indent: 2,
			want: []string{"stop it with", "  'adb kill-server'", "  now"}},
		{name: "a spaceless token longer than the cell line is cut at the edge", text: "pid 123456789012345", width: 10, indent: 2,
			want: []string{"pid", "  12345678", "  9012345"}},
		{name: "a spaceless token on the first cell line uses the full width", text: "123456789012345", width: 10, indent: 2,
			want: []string{"1234567890", "  12345"}},
		{name: "a spaceless token longer than the list line overflows", text: "pid 123456789012345", width: 10, indent: 3, keepWhole: true,
			want: []string{"pid", "   123456789012345"}},
		{name: "runes count, not bytes", text: "José José José", width: 9, indent: 2,
			want: []string{"José José", "  José"}},
		{name: "empty text is one empty line", text: "", width: 10, indent: 2, want: []string{""}},
		{name: "an indent that would fill the line shrinks to keep the width", text: "Device", width: 2, indent: 2,
			want: []string{"De", " v", " i", " c", " e"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Wrap(tc.text, tc.width, tc.indent, tc.keepWhole))
		})
	}
}

func TestGivenQuotesInProseWhenTokenizedThenOnlySpansBetweenSpacesStayWhole(t *testing.T) {
	assert.Equal(t, []string{"the", "server's", "adb;", "run", "'adb kill-server',", "then", "'x'."},
		tokens("the  server's adb; run 'adb kill-server', then 'x'.", 80, false))
	assert.Equal(t, []string{"run", "'bridge start':", "it"}, tokens("run 'bridge start': it", 80, false), "a colon closes a span too")
	assert.Equal(t, []string{"'abc", "def", "'abc'd", "ef"}, tokens("'abc def 'abc'd ef", 80, false))
	assert.Equal(t, []string{"'abc", "def"}, tokens("'abc def", 80, false), "an unclosed quote opens no span")
	assert.Equal(t, []string{"run", "'--log \"/tmp/Build  Logs/b.log\"'", "now"}, tokens("run\t'--log \"/tmp/Build  Logs/b.log\"'\nnow", 80, false),
		"a span keeps its inner spaces; prose whitespace is a separator")
	assert.Equal(t, []string{"'a  b'", "c"}, tokens("'a  b'\u00a0c", 80, false), "a multibyte space closes a span too")
	assert.Equal(t, []string{"'a b c'"}, tokens("'a b c'", 7, false), "a span at the limit stays whole in a cell")
	assert.Equal(t, []string{"'a", "b", "c'"}, tokens("'a b c'", 6, false), "a span over the limit splits in a cell")
	assert.Equal(t, []string{"'a b c'"}, tokens("'a b c'", 6, true), "a span over the limit stays whole in the list")
}
