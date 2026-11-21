package burnpace

import (
	"slices"
	"testing"
)

// The expected values are the Python standard library's, pasted in. To
// regenerate a row, feed its inputs to
//
//	python3 -I -c 'import json,sys,textwrap; print(json.dumps([textwrap.wrap(t, w) for t, w in json.load(sys.stdin)], ensure_ascii=False))'
//	python3 -I -c 'import json,sys,textwrap; print(json.dumps([textwrap.TextWrapper()._split_chunks(t) for t in json.load(sys.stdin)], ensure_ascii=False))'
//
// wrapCases holds every status sentence of the captured pages at 94, 74 and
// 20 runes, then the edge cases.

func TestWrapMatchesTextwrap(t *testing.T) {
	for _, c := range wrapCases {
		if got := wrap(c.text, c.limit); !slices.Equal(got, c.want) {
			t.Errorf("wrap(%q, %d)\n got %q\nwant %q", c.text, c.limit, got, c.want)
		}
	}
}

func TestChunksMatchTextwrap(t *testing.T) {
	for _, c := range chunkCases {
		if got := chunks(c.text); !slices.Equal(got, c.want) {
			t.Errorf("chunks(%q)\n got %q\nwant %q", c.text, got, c.want)
		}
	}
}

var wrapCases = []struct {
	text  string
	limit int
	want  []string
}{
	{"LOCAL MISSING — this machine's accounts are not in the numbers below (timeout after 65s)", 94, []string{"LOCAL MISSING — this machine's accounts are not in the numbers below (timeout after 65s)"}},
	{"LOCAL MISSING — this machine's accounts are not in the numbers below (timeout after 65s)", 74, []string{"LOCAL MISSING — this machine's accounts are not in the numbers below", "(timeout after 65s)"}},
	{"LOCAL MISSING — this machine's accounts are not in the numbers below (timeout after 65s)", 20, []string{"LOCAL MISSING — this", "machine's accounts", "are not in the", "numbers below", "(timeout after 65s)"}},
	{"local unreachable — timeout after 65s", 94, []string{"local unreachable — timeout after 65s"}},
	{"local unreachable — timeout after 65s", 74, []string{"local unreachable — timeout after 65s"}},
	{"local unreachable — timeout after 65s", 20, []string{"local unreachable —", "timeout after 65s"}},
	{"HOME NOT READ (No route to host) — box.example (peer link) is in the numbers; if that is home, nothing is missing", 94, []string{"HOME NOT READ (No route to host) — box.example (peer link) is in the numbers; if that is home,", "nothing is missing"}},
	{"HOME NOT READ (No route to host) — box.example (peer link) is in the numbers; if that is home, nothing is missing", 74, []string{"HOME NOT READ (No route to host) — box.example (peer link) is in the", "numbers; if that is home, nothing is missing"}},
	{"HOME NOT READ (No route to host) — box.example (peer link) is in the numbers; if that is home, nothing is missing", 20, []string{"HOME NOT READ (No", "route to host) —", "box.example (peer", "link) is in the", "numbers; if that is", "home, nothing is", "missing"}},
	{"home's servers.json address is down (No route to host); a peer link stood in, see above", 94, []string{"home's servers.json address is down (No route to host); a peer link stood in, see above"}},
	{"home's servers.json address is down (No route to host); a peer link stood in, see above", 74, []string{"home's servers.json address is down (No route to host); a peer link stood", "in, see above"}},
	{"home's servers.json address is down (No route to host); a peer link stood in, see above", 20, []string{"home's servers.json", "address is down (No", "route to host); a", "peer link stood in,", "see above"}},
	{"box.example is a last-good reading — this machine's daemon cannot reach it now (dial tcp 203.0.113.7:31416: i/o timeout)", 94, []string{"box.example is a last-good reading — this machine's daemon cannot reach it now (dial tcp", "203.0.113.7:31416: i/o timeout)"}},
	{"box.example is a last-good reading — this machine's daemon cannot reach it now (dial tcp 203.0.113.7:31416: i/o timeout)", 74, []string{"box.example is a last-good reading — this machine's daemon cannot reach it", "now (dial tcp 203.0.113.7:31416: i/o timeout)"}},
	{"box.example is a last-good reading — this machine's daemon cannot reach it now (dial tcp 203.0.113.7:31416: i/o timeout)", 20, []string{"box.example is a", "last-good reading —", "this machine's", "daemon cannot reach", "it now (dial tcp", "203.0.113.7:31416:", "i/o timeout)"}},
	{"BOX.EXAMPLE MISSING — this machine's peer link last read it 6h ago, too old to count (dial tcp 203.0.113.7:31416: i/o timeout)", 94, []string{"BOX.EXAMPLE MISSING — this machine's peer link last read it 6h ago, too old to count (dial tcp", "203.0.113.7:31416: i/o timeout)"}},
	{"BOX.EXAMPLE MISSING — this machine's peer link last read it 6h ago, too old to count (dial tcp 203.0.113.7:31416: i/o timeout)", 74, []string{"BOX.EXAMPLE MISSING — this machine's peer link last read it 6h ago, too", "old to count (dial tcp 203.0.113.7:31416: i/o timeout)"}},
	{"BOX.EXAMPLE MISSING — this machine's peer link last read it 6h ago, too old to count (dial tcp 203.0.113.7:31416: i/o timeout)", 20, []string{"BOX.EXAMPLE MISSING", "— this machine's", "peer link last read", "it 6h ago, too old", "to count (dial tcp", "203.0.113.7:31416:", "i/o timeout)"}},
	{"HOME MISSING — home's accounts are not in the numbers below (No route to host)", 94, []string{"HOME MISSING — home's accounts are not in the numbers below (No route to host)"}},
	{"HOME MISSING — home's accounts are not in the numbers below (No route to host)", 74, []string{"HOME MISSING — home's accounts are not in the numbers below (No route to", "host)"}},
	{"HOME MISSING — home's accounts are not in the numbers below (No route to host)", 20, []string{"HOME MISSING —", "home's accounts are", "not in the numbers", "below (No route to", "host)"}},
	{"home unreachable — No route to host", 94, []string{"home unreachable — No route to host"}},
	{"home unreachable — No route to host", 74, []string{"home unreachable — No route to host"}},
	{"home unreachable — No route to host", 20, []string{"home unreachable —", "No route to host"}},
	{"1 rolled since the reading — not in the numbers: b-one", 94, []string{"1 rolled since the reading — not in the numbers: b-one"}},
	{"1 rolled since the reading — not in the numbers: b-one", 74, []string{"1 rolled since the reading — not in the numbers: b-one"}},
	{"1 rolled since the reading — not in the numbers: b-one", 20, []string{"1 rolled since the", "reading — not in the", "numbers: b-one"}},
	{"peer link to home has no reading (dial tcp: connection refused) — unless it is home, its accounts are not in the numbers below", 94, []string{"peer link to home has no reading (dial tcp: connection refused) — unless it is home, its", "accounts are not in the numbers below"}},
	{"peer link to home has no reading (dial tcp: connection refused) — unless it is home, its accounts are not in the numbers below", 74, []string{"peer link to home has no reading (dial tcp: connection refused) — unless", "it is home, its accounts are not in the numbers below"}},
	{"peer link to home has no reading (dial tcp: connection refused) — unless it is home, its accounts are not in the numbers below", 20, []string{"peer link to home", "has no reading (dial", "tcp: connection", "refused) — unless it", "is home, its", "accounts are not in", "the numbers below"}},
	{"AT LEAST 1 OF HOME, WORK MISSING — none was read at its servers.json address (home: No route to host; work: No route to host); boxpeer (peer link) is in the numbers and may be one of them", 94, []string{"AT LEAST 1 OF HOME, WORK MISSING — none was read at its servers.json address (home: No route", "to host; work: No route to host); boxpeer (peer link) is in the numbers and may be one of them"}},
	{"AT LEAST 1 OF HOME, WORK MISSING — none was read at its servers.json address (home: No route to host; work: No route to host); boxpeer (peer link) is in the numbers and may be one of them", 74, []string{"AT LEAST 1 OF HOME, WORK MISSING — none was read at its servers.json", "address (home: No route to host; work: No route to host); boxpeer (peer", "link) is in the numbers and may be one of them"}},
	{"AT LEAST 1 OF HOME, WORK MISSING — none was read at its servers.json address (home: No route to host; work: No route to host); boxpeer (peer link) is in the numbers and may be one of them", 20, []string{"AT LEAST 1 OF HOME,", "WORK MISSING — none", "was read at its", "servers.json address", "(home: No route to", "host; work: No route", "to host); boxpeer", "(peer link) is in", "the numbers and may", "be one of them"}},
	{"work unreachable — No route to host", 94, []string{"work unreachable — No route to host"}},
	{"work unreachable — No route to host", 74, []string{"work unreachable — No route to host"}},
	{"work unreachable — No route to host", 20, []string{"work unreachable —", "No route to host"}},
	{"2 unmeasured · 1 auth-invalid — not in the numbers: cut-new, cut-old, dead-one", 94, []string{"2 unmeasured · 1 auth-invalid — not in the numbers: cut-new, cut-old, dead-one"}},
	{"2 unmeasured · 1 auth-invalid — not in the numbers: cut-new, cut-old, dead-one", 74, []string{"2 unmeasured · 1 auth-invalid — not in the numbers: cut-new, cut-old,", "dead-one"}},
	{"2 unmeasured · 1 auth-invalid — not in the numbers: cut-new, cut-old, dead-one", 20, []string{"2 unmeasured · 1", "auth-invalid — not", "in the numbers: cut-", "new, cut-old, dead-", "one"}},
	{"1 no weekly window — not in the numbers: codex-5h", 94, []string{"1 no weekly window — not in the numbers: codex-5h"}},
	{"1 no weekly window — not in the numbers: codex-5h", 74, []string{"1 no weekly window — not in the numbers: codex-5h"}},
	{"1 no weekly window — not in the numbers: codex-5h", 20, []string{"1 no weekly window —", "not in the numbers:", "codex-5h"}},
	{"LOCAL UNMEASURED — none of this machine's 12 accounts has a weekly reading this read (sweep cut, or not yet checked), so they are not in the numbers below", 94, []string{"LOCAL UNMEASURED — none of this machine's 12 accounts has a weekly reading this read (sweep", "cut, or not yet checked), so they are not in the numbers below"}},
	{"LOCAL UNMEASURED — none of this machine's 12 accounts has a weekly reading this read (sweep cut, or not yet checked), so they are not in the numbers below", 74, []string{"LOCAL UNMEASURED — none of this machine's 12 accounts has a weekly reading", "this read (sweep cut, or not yet checked), so they are not in the numbers", "below"}},
	{"LOCAL UNMEASURED — none of this machine's 12 accounts has a weekly reading this read (sweep cut, or not yet checked), so they are not in the numbers below", 20, []string{"LOCAL UNMEASURED —", "none of this", "machine's 12", "accounts has a", "weekly reading this", "read (sweep cut, or", "not yet checked), so", "they are not in the", "numbers below"}},
	{"12 unmeasured — not in the numbers: cut-0, cut-1, cut-10, cut-11, cut-2, cut-3, cut-4, cut-5, cut-6, cut-7, cut-8, cut-9", 94, []string{"12 unmeasured — not in the numbers: cut-0, cut-1, cut-10, cut-11, cut-2, cut-3, cut-4, cut-5,", "cut-6, cut-7, cut-8, cut-9"}},
	{"12 unmeasured — not in the numbers: cut-0, cut-1, cut-10, cut-11, cut-2, cut-3, cut-4, cut-5, cut-6, cut-7, cut-8, cut-9", 74, []string{"12 unmeasured — not in the numbers: cut-0, cut-1, cut-10, cut-11, cut-2,", "cut-3, cut-4, cut-5, cut-6, cut-7, cut-8, cut-9"}},
	{"12 unmeasured — not in the numbers: cut-0, cut-1, cut-10, cut-11, cut-2, cut-3, cut-4, cut-5, cut-6, cut-7, cut-8, cut-9", 20, []string{"12 unmeasured — not", "in the numbers:", "cut-0, cut-1,", "cut-10, cut-11,", "cut-2, cut-3, cut-4,", "cut-5, cut-6, cut-7,", "cut-8, cut-9"}},
	{"no scope that answered has a measured account, so there are no numbers to show", 94, []string{"no scope that answered has a measured account, so there are no numbers to show"}},
	{"no scope that answered has a measured account, so there are no numbers to show", 74, []string{"no scope that answered has a measured account, so there are no numbers to", "show"}},
	{"no scope that answered has a measured account, so there are no numbers to show", 20, []string{"no scope that", "answered has a", "measured account, so", "there are no numbers", "to show"}},
	{"HOME MISSING — home's accounts are not in the numbers below (Connection refused)", 94, []string{"HOME MISSING — home's accounts are not in the numbers below (Connection refused)"}},
	{"HOME MISSING — home's accounts are not in the numbers below (Connection refused)", 74, []string{"HOME MISSING — home's accounts are not in the numbers below (Connection", "refused)"}},
	{"HOME MISSING — home's accounts are not in the numbers below (Connection refused)", 20, []string{"HOME MISSING —", "home's accounts are", "not in the numbers", "below (Connection", "refused)"}},
	{"home unreachable — Connection refused", 94, []string{"home unreachable — Connection refused"}},
	{"home unreachable — Connection refused", 74, []string{"home unreachable — Connection refused"}},
	{"home unreachable — Connection refused", 20, []string{"home unreachable —", "Connection refused"}},
	{"no fallback through this machine's daemon: HTTP 500", 94, []string{"no fallback through this machine's daemon: HTTP 500"}},
	{"no fallback through this machine's daemon: HTTP 500", 74, []string{"no fallback through this machine's daemon: HTTP 500"}},
	{"no fallback through this machine's daemon: HTTP 500", 20, []string{"no fallback through", "this machine's", "daemon: HTTP 500"}},
	{"no fallback through this machine's daemon: timeout after 15s", 94, []string{"no fallback through this machine's daemon: timeout after 15s"}},
	{"no fallback through this machine's daemon: timeout after 15s", 74, []string{"no fallback through this machine's daemon: timeout after 15s"}},
	{"no fallback through this machine's daemon: timeout after 15s", 20, []string{"no fallback through", "this machine's", "daemon: timeout", "after 15s"}},
	{"no scope could be read, so there are no numbers to show", 94, []string{"no scope could be read, so there are no numbers to show"}},
	{"no scope could be read, so there are no numbers to show", 74, []string{"no scope could be read, so there are no numbers to show"}},
	{"no scope could be read, so there are no numbers to show", 20, []string{"no scope could be", "read, so there are", "no numbers to show"}},
	{"HOME MISSING — home's accounts are not in the numbers below (ValueError)", 94, []string{"HOME MISSING — home's accounts are not in the numbers below (ValueError)"}},
	{"HOME MISSING — home's accounts are not in the numbers below (ValueError)", 74, []string{"HOME MISSING — home's accounts are not in the numbers below (ValueError)"}},
	{"HOME MISSING — home's accounts are not in the numbers below (ValueError)", 20, []string{"HOME MISSING —", "home's accounts are", "not in the numbers", "below (ValueError)"}},
	{"LAB MISSING — lab's accounts are not in the numbers below (InvalidURL)", 94, []string{"LAB MISSING — lab's accounts are not in the numbers below (InvalidURL)"}},
	{"LAB MISSING — lab's accounts are not in the numbers below (InvalidURL)", 74, []string{"LAB MISSING — lab's accounts are not in the numbers below (InvalidURL)"}},
	{"LAB MISSING — lab's accounts are not in the numbers below (InvalidURL)", 20, []string{"LAB MISSING — lab's", "accounts are not in", "the numbers below", "(InvalidURL)"}},
	{"home unreachable — ValueError", 94, []string{"home unreachable — ValueError"}},
	{"home unreachable — ValueError", 74, []string{"home unreachable — ValueError"}},
	{"home unreachable — ValueError", 20, []string{"home unreachable —", "ValueError"}},
	{"lab unreachable — InvalidURL", 94, []string{"lab unreachable — InvalidURL"}},
	{"lab unreachable — InvalidURL", 74, []string{"lab unreachable — InvalidURL"}},
	{"lab unreachable — InvalidURL", 20, []string{"lab unreachable —", "InvalidURL"}},
	{"HOME MISSING — home's accounts are not in the numbers below (redirected to another address)", 94, []string{"HOME MISSING — home's accounts are not in the numbers below (redirected to another address)"}},
	{"HOME MISSING — home's accounts are not in the numbers below (redirected to another address)", 74, []string{"HOME MISSING — home's accounts are not in the numbers below (redirected to", "another address)"}},
	{"HOME MISSING — home's accounts are not in the numbers below (redirected to another address)", 20, []string{"HOME MISSING —", "home's accounts are", "not in the numbers", "below (redirected to", "another address)"}},
	{"home unreachable — redirected to another address", 94, []string{"home unreachable — redirected to another address"}},
	{"home unreachable — redirected to another address", 74, []string{"home unreachable — redirected to another address"}},
	{"home unreachable — redirected to another address", 20, []string{"home unreachable —", "redirected to", "another address"}},
	{"servers.json entry #2 unreadable (KeyError); not read", 94, []string{"servers.json entry #2 unreadable (KeyError); not read"}},
	{"servers.json entry #2 unreadable (KeyError); not read", 74, []string{"servers.json entry #2 unreadable (KeyError); not read"}},
	{"servers.json entry #2 unreadable (KeyError); not read", 20, []string{"servers.json entry", "#2 unreadable", "(KeyError); not read"}},
	{"2 unmeasured · 1 auth-invalid row", 22, []string{"2 unmeasured · 1 auth-", "invalid row"}},
	{"held on a last-good reading", 20, []string{"held on a last-good", "reading"}},
	{"alpha-bravolabs is in the numbers", 10, []string{"alpha-", "bravolabs", "is in the", "numbers"}},
	{"names cut-10 and cut-11", 10, []string{"names", "cut-10 and", "cut-11"}},
	{"see aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa now", 20, []string{"see aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaa now"}},
	{"see aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa now", 94, []string{"see aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa now"}},
	{"x pre-defghijklmnopqrstuvwxyz-abcdefghij-klmnopq", 20, []string{"x pre-defghijklmnopq", "rstuvwxyz-", "abcdefghij-klmnopq"}},
	{"see abcdefgh-1234567890123456 x", 20, []string{"see abcdefgh-", "1234567890123456 x"}},
	{"----123456789012345678901234", 20, []string{"----1234567890123456", "78901234"}},
	{"-----abcdefghijklmnopqrstuvwxyz", 20, []string{"-----abcdefghijklmno", "pqrstuvwxyz"}},
	{"ab--------------------------cd", 20, []string{"ab------------------", "--------cd"}},
	{"a -- b and x--y", 6, []string{"a -- b", "and x", "--y"}},
	{"one\ttwo\tthree\tfour", 20, []string{"one     two", "three   four"}},
	{"ab\n\tc\r\td\u000be\u000cf", 20, []string{"ab         c", "d e f"}},
	{"a\u00a0b\u00a0c d", 3, []string{"a\u00a0b", "\u00a0c", "d"}},
	{"     ", 20, nil},
	{"", 20, nil},
	{"  leading spaces stay on the first line", 12, []string{"  leading", "spaces stay", "on the first", "line"}},
	{"abcd xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", 5, []string{"abcd ", "xxxxx", "xxxxx", "xxxxx", "xxxxx", "xxxxx", "xxxxx"}},
	{"     xxxxxxxxxxxx", 5, []string{"     ", "xxxxx", "xxxxx", "xx"}},
	{"x — y · z—w ·v", 4, []string{"x —", "y ·", "z—w", "·v"}},
	{"a \u001c b \u00a0 c", 3, []string{"a ", "b ", "c"}},
	{"naïve-café x²-y ab_-cd", 7, []string{"naïve-", "café", "x²-y", "ab_-cd"}},
	{"twentyrunesexactly20 next", 20, []string{"twentyrunesexactly20", "next"}},
	{"twentyonerunesexactly next", 20, []string{"twentyonerunesexactl", "y next"}},
}

var chunkCases = []struct {
	text string
	want []string
}{
	{"auth-invalid", []string{"auth-", "invalid"}},
	{"last-good", []string{"last-", "good"}},
	{"alpha-bravolabs", []string{"alpha-", "bravolabs"}},
	{"cut-10", []string{"cut-10"}},
	{"a-b-cd", []string{"a-b-", "cd"}},
	{"ab-c-de", []string{"ab-", "c-", "de"}},
	{"a -- b", []string{"a", " ", "--", " ", "b"}},
	{"x--y", []string{"x", "--", "y"}},
	{"x---y", []string{"x", "---", "y"}},
	{"--x", []string{"--x"}},
	{"x-- y", []string{"x--", " ", "y"}},
	{"well,--said", []string{"well,", "--", "said"}},
	{"(sweep cut, or not yet checked)", []string{"(sweep", " ", "cut,", " ", "or", " ", "not", " ", "yet", " ", "checked)"}},
	{"home's servers.json address", []string{"home's", " ", "servers.json", " ", "address"}},
	{"dial tcp 203.0.113.7:31416: i/o timeout", []string{"dial", " ", "tcp", " ", "203.0.113.7:31416:", " ", "i/o", " ", "timeout"}},
	{"x — y · z—w ·v", []string{"x", " ", "—", " ", "y", " ", "·", " ", "z—w", " ", "·v"}},
	{"a\u00a0b c", []string{"a\u00a0b", " ", "c"}},
	{"one\ttwo", []string{"one", "     ", "two"}},
	{"a\n\r\u000b\u000cb", []string{"a", "    ", "b"}},
	{"naïve-café x²-y ab_-cd 9a-bc a9-bc", []string{"naïve-", "café", " ", "x²-y", " ", "ab_-", "cd", " ", "9a-bc", " ", "a9-bc"}},
	{"e-mail re-read co-op", []string{"e-mail", " ", "re-", "read", " ", "co-", "op"}},
	{"", nil},
	{"   ", []string{"   "}},
}
