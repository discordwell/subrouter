package burnpace

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// wrap is Python's textwrap.wrap(text, limit) with that module's default
// options, ported line for line: the read-out has always been wrapped that
// way, and a status line that breaks one word differently is a different
// page. Lengths are rune counts, as Python's len counts code points.
func wrap(text string, limit int) []string {
	pending := chunks(text)
	var lines []string
	for len(pending) > 0 {
		var line []string
		used := 0
		if len(lines) > 0 && blank(pending[0]) {
			pending = pending[1:]
		}
		for len(pending) > 0 && used+runeLen(pending[0]) <= limit {
			line = append(line, pending[0])
			used += runeLen(pending[0])
			pending = pending[1:]
		}
		if len(pending) > 0 && runeLen(pending[0]) > limit {
			head, tail := breakLong(pending[0], limit-used)
			line = append(line, head)
			pending[0] = tail
		}
		// Only the last piece is dropped when blank. When the line was full,
		// that piece is breakLong's empty head, so a blank piece before it
		// stays and the line keeps a trailing space, as textwrap's does.
		if len(line) > 0 && blank(line[len(line)-1]) {
			line = line[:len(line)-1]
		}
		if len(line) > 0 {
			lines = append(lines, strings.Join(line, ""))
		}
	}
	return lines
}

// chunks splits text as textwrap's word-separator pattern does, after its
// tab expansion and whitespace replacement. Go's regexp has no lookaround,
// so each alternative of the pattern is tried by hand, in the pattern's
// order, at each position; a word is lazy and ends at the first place one of
// its three endings holds.
func chunks(text string) []string {
	s := []rune(mungeWhitespace(text))
	var out []string
	for i := 0; i < len(s); {
		n := chunkAt(s, i)
		out = append(out, string(s[i:i+n]))
		i += n
	}
	return out
}

func chunkAt(s []rune, i int) int {
	if s[i] == ' ' {
		j := i
		for j < len(s) && s[j] == ' ' {
			j++
		}
		return j - i
	}
	if i > 0 && wordPunct(s[i-1]) {
		if n := dashesBeforeWord(s, i); n > 0 {
			return n
		}
	}
	for end := i + 1; ; end++ {
		if end < len(s) && s[end] == '-' && hyphenBreak(s, end) {
			return end + 1 - i
		}
		if end == len(s) || s[end] == ' ' {
			return end - i
		}
		if wordPunct(s[end-1]) && dashesBeforeWord(s, end) > 0 {
			return end - i
		}
	}
}

// hyphenBreak reports whether a word may end after the hyphen at s[h]: two
// letters, or letter-hyphen-letter, before it, and letter, optional hyphen,
// letter after it.
func hyphenBreak(s []rune, h int) bool {
	at := func(k int) rune {
		if k < 0 || k >= len(s) {
			return 0
		}
		return s[k]
	}
	before := (letter(at(h-1)) && letter(at(h-2))) ||
		(letter(at(h-1)) && at(h-2) == '-' && letter(at(h-3)))
	after := letter(at(h+1)) && (letter(at(h+2)) || (at(h+2) == '-' && letter(at(h+3))))
	return before && after
}

// dashesBeforeWord is the length of a run of two or more hyphens at s[i]
// that a word character follows, or 0.
func dashesBeforeWord(s []rune, i int) int {
	j := i
	for j < len(s) && s[j] == '-' {
		j++
	}
	if j-i >= 2 && j < len(s) && wordRune(s[j]) {
		return j - i
	}
	return 0
}

// breakLong splits a chunk longer than a whole line, with room runes left on
// the current line: at the last hyphen that fits, when something other than
// hyphens comes before it, else hard at room.
func breakLong(chunk string, room int) (head, tail string) {
	r := []rune(chunk)
	end := room
	h := room - 1
	for h >= 0 && r[h] != '-' {
		h--
	}
	if h > 0 && strings.Trim(string(r[:h]), "-") != "" {
		end = h + 1
	}
	return string(r[:end]), string(r[end:])
}

// mungeWhitespace is str.expandtabs(8), whose columns restart after "\n" and
// "\r", followed by every one of "\t\n\v\f\r" becoming a space.
func mungeWhitespace(text string) string {
	var b strings.Builder
	col := 0
	for _, r := range text {
		switch r {
		case '\t':
			pad := 8 - col%8
			b.WriteString(strings.Repeat(" ", pad))
			col += pad
			continue
		case '\n', '\v', '\f', '\r':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
		col++
		if r == '\n' || r == '\r' {
			col = 0
		}
	}
	return b.String()
}

// wordRune is Python's \w on str: a letter, any number, or '_'.
func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

// letter is textwrap's [^\d\W]: \w that is not a decimal digit, so '_' and
// '²' are letters.
func letter(r rune) bool { return wordRune(r) && !unicode.IsDigit(r) }

func wordPunct(r rune) bool { return wordRune(r) || strings.ContainsRune(`!"'&.,?`, r) }

// blank is Python's s.strip() == "".
func blank(s string) bool { return strings.TrimFunc(s, pySpace) == "" }

// pySpace is Python's str.isspace for one rune. It also holds for the ASCII
// separators U+001C..U+001F, which unicode.IsSpace leaves out.
func pySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func runeLen(s string) int { return utf8.RuneCountInString(s) }
