// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Wrap breaks text into lines of at most width runes; continuation lines indent, by less when
// the indent would fill the line. keepWhole (a list) never cuts a token: one longer than the line
// stands alone and overflows, so a quoted command copies whole. Without it (a cell) a quoted span
// longer than a continuation line wraps as prose and a spaceless token longer than the line is cut
// at the edge and continues on the next line.
func Wrap(text string, width, indent int, keepWhole bool) []string {
	indent = min(indent, max(width-1, 0))
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
