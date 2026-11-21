package burnpace

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
)

// The SGR spans as the reference script writes them, so expected coloured
// text is spelled out here rather than taken from Render.
func bold(s string) string  { return "\x1b[1m" + s + "\x1b[0m" }
func dim(s string) string   { return "\x1b[2m" + s + "\x1b[0m" }
func blue(s string) string  { return "\x1b[38;5;39m" + s + "\x1b[0m" }
func deep(s string) string  { return "\x1b[38;5;24m" + s + "\x1b[0m" }
func amber(s string) string { return "\x1b[38;5;214m" + s + "\x1b[0m" }
func grey(s string) string  { return "\x1b[38;5;243m" + s + "\x1b[0m" }
func white(s string) string { return "\x1b[1;97m" + s + "\x1b[0m" }

func sp(n int) string { return strings.Repeat(" ", n) }

var sgrSpan = regexp.MustCompile("\x1b\\[[0-9;]*m")

func pageLines(t *testing.T, s Snapshot, p Page) []string {
	t.Helper()
	return strings.Split(render(t, Derive(s, thursday0701), p), "\n")
}

func lineWith(t *testing.T, lines []string, prefix string) (int, string) {
	t.Helper()
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return i, line
		}
	}
	t.Fatalf("no line starts with %q in\n%s", prefix, strings.Join(lines, "\n"))
	return 0, ""
}

// chartRows are the lines between the chart's header and its axis.
func chartRows(t *testing.T, lines []string) []string {
	t.Helper()
	header, _ := lineWith(t, lines, "  ceiling · next 168h")
	axis, _ := lineWith(t, lines, "  1.0×└")
	return lines[header+1 : axis]
}

func requireLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("--- want\n%s\n--- got\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

func TestColourSpans(t *testing.T) {
	coloured := Page{Cols: 100, Lines: 40, Colour: true}
	for _, p := range pages() {
		t.Run(p.golden+" without its spans is the captured page", func(t *testing.T) {
			requirePage(t, sgrSpan.ReplaceAllString(render(t, Derive(p.snapshot, thursday0701), coloured), ""), goldenText(t, p))
		})
	}

	p := pageNamed(t, "03-box-through-peer-view")
	got := pageLines(t, p.snapshot, coloured)
	plain := strings.Split(goldenText(t, p), "\n")
	want := []string{
		"",
		"  " + bold("subrouter · 7d burn pace") + " · local + box.example via peer link" + sp(25) + grey("Thu 07:01"),
		"  " + dim(strings.Repeat("─", 96)),
		"  " + amber("HOME NOT READ (No route to host) — box.example (peer link) is in the numbers; if that is home,"),
		"    " + amber("nothing is missing"),
		"",
		"  " + bold("claude  ") + bold(" 0.70× ") + " " + dim("▕") + strings.Repeat(blue("█"), 13) + blue("▍") + sp(5) + dim("╎") + sp(46) + dim("▏") + grey("  ceil 1.76×"),
		"  " + bold("codex   ") + bold(" 0.22× ") + " " + dim("▕") + strings.Repeat(blue("█"), 4) + sp(15) + dim("╎") + sp(46) + dim("▏") + grey("  ceil 2.15×"),
		sp(19) + dim("0×                 1×                                         3.5×"),
		sp(19) + dim("5 accounts across local + box.example (peer link) · 1 held on both, counted once"),
		"",
		"  " + bold("ceiling · next 168h") + grey("   claude, 4 accounts"),
	}
	// Each chart cell is its own span: the wash deep blue, the curve's cell
	// blue, a blank plain.
	for _, row := range plain[len(want) : len(want)+10] {
		runes := []rune(row)
		var cells strings.Builder
		for _, r := range runes[8:] {
			switch r {
			case ' ':
				cells.WriteByte(' ')
			case '░':
				cells.WriteString(deep("░"))
			default:
				cells.WriteString(blue(string(r)))
			}
		}
		want = append(want, dim(string(runes[:6]))+dim(string(runes[6]))+" "+cells.String())
	}
	want = append(want,
		dim("  1.0×")+dim("└"+strings.Repeat("─", 91)),
		sp(8)+dim("Thu                      Sat                      Mon                       Wed"),
		"  "+dim("(codex ceiling not charted)"),
		"",
		"  "+bold("unspent · expiring soonest"),
		"  "+grey("b-one"+sp(27))+bold(" 40%")+dim("  in  30.0h"),
		"  "+grey("l-one"+sp(27))+bold(" 50%")+dim("  in  40.0h"),
		"  "+grey("shared-codex"+sp(20))+bold(" 90%")+dim("  in  90.0h"),
		"  "+dim("home's servers.json address is down (No route to host); a peer link stood in, see above"),
		"",
		"",
	)
	requireLines(t, got, want)

	narrow := pageLines(t, p.snapshot, Page{Cols: 40, Lines: 40, Colour: true})
	requireLines(t, narrow[1:3], []string{
		"  " + bold("subrouter · 7d burn pace") + " " + grey("Thu 07:01"),
		"  local + box.example via peer link",
	})
}

// barOf is the cells between a page's bar ends, for one claude account at
// 84 h into its week, drawn at 104 columns: 70 cells, the notch at cell 20,
// and exact = 40 × used.
func barOf(t *testing.T, usedPercent float64) string {
	t.Helper()
	lines := pageLines(t, Snapshot{Local: Answered(claude("a", usedPercent, 84))}, Page{Cols: 104, Colour: true})
	_, row := lineWith(t, lines, "  "+bold("claude  "))
	start, end := strings.Index(row, dim("▕")), strings.Index(row, dim("▏"))
	if start < 0 || end < start {
		t.Fatalf("no bar in %q", row)
	}
	return row[start+len(dim("▕")) : end]
}

func TestBarEighths(t *testing.T) {
	for _, c := range []struct {
		name        string
		usedPercent float64
		want        string
	}{
		{"exact 5.1: a remainder under 1/8 is blank", 12.75,
			strings.Repeat(blue("█"), 5) + sp(15) + dim("╎") + sp(49)},
		{"exact 5.2: a remainder in [1/8, 2/8) draws two eighths", 13,
			strings.Repeat(blue("█"), 5) + blue("▎") + sp(14) + dim("╎") + sp(49)},
		{"exact 5.95: a remainder of 7/8 or more is a full block", 14.875,
			strings.Repeat(blue("█"), 5) + blue("█") + sp(14) + dim("╎") + sp(49)},
		{"pace exactly 1.00 is not over", 50,
			strings.Repeat(blue("█"), 20) + dim("╎") + sp(49)},
		{"the notch replaces the partial cell it lands on", 51.25,
			strings.Repeat(amber("█"), 20) + dim("╎") + sp(49)},
		{"the notch over the fill is white", 75,
			strings.Repeat(amber("█"), 20) + white("╎") + strings.Repeat(amber("█"), 9) + sp(40)},
		{"a pace past 3.5 fills the bar", 200,
			strings.Repeat(amber("█"), 20) + white("╎") + strings.Repeat(amber("█"), 49)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := barOf(t, c.usedPercent); got != c.want {
				t.Fatalf("bar\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestNoSignalRows(t *testing.T) {
	underResolution := Snapshot{Local: Answered(claude("a", 3, 100), claude("b", 2, 100))}
	for _, c := range []struct {
		name     string
		snapshot Snapshot
		page     Page
		want     string
	}{
		{"mean draw under the meter's step", underResolution, pagesFrame,
			"  claude      –    no signal · 5% across 2 accounts, under meter resolution"},
		{"too little of the window run", Snapshot{Local: Answered(claude("a", 90, 167.5))}, pagesFrame,
			"  claude      –    no signal · 90% drawn over 0.5h of window"},
		{"cut to the room left", underResolution, Page{Cols: 60},
			"  claude      –    no signal · 5% across 2 accounts, under…"},
		{"coloured", underResolution, Page{Cols: 100, Colour: true},
			"  " + bold("claude  ") + dim("    –") + "    " + amber("no signal") + dim(" · 5% across 2 accounts, under meter resolution")},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := pageLines(t, c.snapshot, c.page)
			i, row := lineWith(t, lines, c.want[:9])
			if row != c.want {
				t.Fatalf("row\n got %q\nwant %q", row, c.want)
			}
			if tick := sgrSpan.ReplaceAllString(lines[i+1], ""); !strings.HasPrefix(tick, sp(19)+"0×") {
				t.Fatalf("the tick line does not follow the row: %q", lines[i+1])
			}
			if strings.Contains(strings.Join(lines, "\n"), "ceiling") {
				t.Fatal("a pool with no signal is charted or named under the chart")
			}
		})
	}

	t.Run("only a provider with signal is charted, and no other is named", func(t *testing.T) {
		lines := pageLines(t, Snapshot{Local: Answered(claude("a", 3, 100), claude("b", 2, 100), codex("c", 50, 84))}, pagesFrame)
		if _, header := lineWith(t, lines, "  ceiling · next 168h"); header != "  ceiling · next 168h   codex, 1 accounts" {
			t.Fatalf("chart header = %q", header)
		}
		if strings.Contains(strings.Join(lines, "\n"), "not charted") {
			t.Fatal("a provider with no signal is named as not charted")
		}
	})
}

func TestChartRowCounts(t *testing.T) {
	evenFour := Snapshot{Local: Answered(claude("a", 50, 1), claude("b", 50, 43), claude("c", 50, 85), claude("d", 50, 127))}
	peer03 := pageNamed(t, "03-box-through-peer-view").snapshot
	for _, c := range []struct {
		name     string
		snapshot Snapshot
		lines    int
		labels   []string
	}{
		{"peak 8.2: past the cap at half steps, so taller than the cap", Snapshot{Local: Answered(claude("a", 50, 1), claude("b", 50, 41))}, 40,
			[]string{"  8.5×┤", "  8.0×┤", "  7.5×┤", "  7.0×┤", "  6.5×┤", "  6.0×┤", "  5.5×┤", "  5.0×┤", "  4.5×┤", "  4.0×┤", "  3.5×┤", "  3.0×┤", "  2.5×┤", "  2.0×┤", "  1.5×┤"}},
		{"peak 17.6: 34 rows, a five-rune label", Snapshot{Local: Answered(claude("a", 50, 7), claude("b", 50, 23))}, 40, nil},
		{"40 lines allow 10 rows", peer03, 40,
			[]string{"  3.5×┤", "      │", "  3.0×┤", "      │", "  2.5×┤", "      │", "  2.0×┤", "      │", "  1.5×┤", "      │"}},
		{"39 lines allow 8", peer03, 39, []string{"  3.5×┤", "  3.0×┤", "  2.5×┤", "  2.0×┤", "  1.5×┤"}},
		{"32 lines allow 8", evenFour, 32,
			[]string{"  3.0×┤", "      │", "  2.5×┤", "      │", "  2.0×┤", "      │", "  1.5×┤", "      │"}},
		{"31 lines allow 6", evenFour, 31, []string{"  3.0×┤", "  2.5×┤", "  2.0×┤", "  1.5×┤"}},
		{"unknown lines are 40", peer03, 0,
			[]string{"  3.5×┤", "      │", "  3.0×┤", "      │", "  2.5×┤", "      │", "  2.0×┤", "      │", "  1.5×┤", "      │"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows := chartRows(t, pageLines(t, c.snapshot, Page{Cols: 100, Lines: c.lines}))
			var labels []string
			for _, row := range rows {
				if n := len([]rune(row)); n != 98 {
					t.Fatalf("chart row %q is %d runes, want 98", row, n)
				}
				labels = append(labels, string([]rune(row)[:7]))
			}
			if c.labels == nil {
				if len(labels) != 34 || labels[0] != " 18.0×┤" || labels[33] != "  1.5×┤" {
					t.Fatalf("%d rows labelled %q .. %q; want 34, \" 18.0×┤\" .. \"  1.5×┤\"", len(labels), labels[0], labels[len(labels)-1])
				}
				return
			}
			requireLines(t, labels, c.labels)
		})
	}
}

func TestNarrowAndClampedColumns(t *testing.T) {
	s := pageNamed(t, "03-box-through-peer-view").snapshot
	m := Derive(s, thursday0701)

	t.Run("columns past 104 draw at 104", func(t *testing.T) {
		requirePage(t, render(t, m, Page{Cols: 150, Lines: 40}), render(t, m, Page{Cols: 104, Lines: 40}))
	})
	t.Run("unknown columns draw at 92", func(t *testing.T) {
		page := render(t, m, Page{Lines: 40})
		requirePage(t, page, render(t, m, Page{Cols: 92, Lines: 40}))
		lines := strings.Split(page, "\n")
		if lines[2] != "  "+strings.Repeat("─", 88) {
			t.Fatalf("rule = %q, want 88 rules", lines[2])
		}
	})
	t.Run("80 columns", func(t *testing.T) {
		lines := pageLines(t, s, Page{Cols: 80, Lines: 40})
		_, row := lineWith(t, lines, "  claude ")
		_, tick := lineWith(t, lines, sp(19)+"0×")
		rows := chartRows(t, lines)
		for what, c := range map[string]struct{ line, want int }{
			"headline": {len([]rune(lines[1])), 76},
			"rule":     {len([]rune(lines[2])), 78},
			"bar row":  {len([]rune(row)), 78},
			"tick":     {len([]rune(tick)), 65},
			"chart":    {len([]rune(rows[0])), 78},
		} {
			if c.line != c.want {
				t.Errorf("%s is %d runes, want %d", what, c.line, c.want)
			}
		}
	})
	t.Run("40 columns: labels wrap under the headline, lines shorten, bar and chart keep their minimum", func(t *testing.T) {
		lines := pageLines(t, s, Page{Cols: 40, Lines: 40})
		rows := chartRows(t, lines)
		header, _ := lineWith(t, lines, "  ceiling · next 168h")
		want := []string{
			"",
			"  subrouter · 7d burn pace Thu 07:01",
			"  local + box.example via peer link",
			"  " + strings.Repeat("─", 36),
			"  HOME NOT READ (No route to host) —",
			"    box.example (peer link) is in the",
			"    numbers; if that is home, nothing",
			"    is missing",
			"",
			"  claude   0.70×  ▕████  ╎             ▏  ceil 1.76×",
			"  codex    0.22×  ▕█▎    ╎             ▏  ceil 2.15×",
			"                   0×    1×        3.5×",
			"                   5 accounts, local +…",
			"",
			"  ceiling · next 168h   claude, 4 accounts",
		}
		requireLines(t, lines[:header+1], want)
		var labels []string
		for _, row := range rows {
			if n := len([]rune(row)); n != 38 {
				t.Fatalf("chart row %q is %d runes, want 38", row, n)
			}
			labels = append(labels, string([]rune(row)[:8]))
		}
		requireLines(t, labels, []string{"  3.5×┤ ", "      │ ", "  3.0×┤ ", "      │ ", "  2.5×┤ ", "      │ ", "  2.0×┤ ", "      │ ", "  1.5×┤ ", "      │ "})
		requireLines(t, lines[header+1+len(rows):], []string{
			"  1.0×└" + strings.Repeat("─", 31),
			"        Thu     Sat     Mon     Wed",
			"  (codex ceiling not charted)",
			"",
			"  unspent · expiring soonest",
			"  b-one                            40%  in  30.0h",
			"  l-one                            50%  in  40.0h",
			"  shared-codex                     90%  in  90.0h",
			"  home's servers.json address is",
			"    down (No route to host); a peer",
			"    link stood in, see above",
			"",
			"",
		})
	})
}

func TestAnEmptyAnsweredPoolPrintsZeroAccounts(t *testing.T) {
	requireLines(t, pageLines(t, Snapshot{Local: Answered()}, pagesFrame), []string{
		"",
		"  subrouter · 7d burn pace · local" + sp(53) + "Thu 07:01",
		"  " + strings.Repeat("─", 96),
		"",
		"                   0×                 1×                                         3.5×",
		"                   0 accounts from local",
		"",
		"",
	})
}

func TestStaleAndConflictLines(t *testing.T) {
	ids := []string{"conflicting-account-one", "conflicting-account-two", "conflicting-account-three", "conflicting-account-four"}
	var local, box []Row
	for _, id := range ids {
		cut := claude(id, 50, 40)
		cut.SweepCut = true
		local = append(local, cut)
		box = append(box, claude(id, 90, 40))
	}
	s := Snapshot{Local: Answered(local...), Boxes: home(Answered(box...))}

	lines := pageLines(t, s, Page{Cols: 40, Lines: 40})
	i, _ := lineWith(t, lines, sp(19)+"4 accounts")
	requireLines(t, lines[i+1:i+4], []string{
		"  4 in the numbers on a last-good",
		"    reading (sweep cut this read)",
		"  same account disagrees across scopes: conflicting-account-one, conflicting-account-two, conflicting-account-three",
	})

	lines = pageLines(t, s, Page{Cols: 100, Colour: true})
	i, _ = lineWith(t, lines, sp(19)+"\x1b[2m4 accounts across")
	requireLines(t, lines[i+1:i+3], []string{
		"  " + grey("4 in the numbers on a last-good reading (sweep cut this read)"),
		"  " + amber("same account disagrees across scopes: conflicting-account-one, conflicting-account-two, conflicting-account-three"),
	})
}

type writeRecorder struct {
	calls int
	err   error
	got   strings.Builder
}

func (w *writeRecorder) Write(p []byte) (int, error) {
	w.calls++
	if w.err != nil {
		return 0, w.err
	}
	return w.got.Write(p)
}

func TestRenderReturnsTheWritersError(t *testing.T) {
	p := pageNamed(t, "03-box-through-peer-view")
	m := Derive(p.snapshot, thursday0701)

	broken := &writeRecorder{err: errors.New("broken pipe")}
	if err := Render(broken, m, pagesFrame); !errors.Is(err, broken.err) || broken.calls != 1 {
		t.Fatalf("err = %v after %d writes; want the writer's error after one write", err, broken.calls)
	}

	ok := &writeRecorder{}
	if err := Render(ok, m, pagesFrame); err != nil || ok.calls != 1 {
		t.Fatalf("err = %v after %d writes; want nil after one write", err, ok.calls)
	}
	requirePage(t, ok.got.String(), goldenText(t, p))
}

func TestExpiringRows(t *testing.T) {
	lines := pageLines(t, Snapshot{Local: Answered(
		claude("an-account-id-that-runs-past-thirty-runes", 100, 10),
		claude("b", 101, 20),
		claude("c", 20, 30),
		claude("d", 10, 120),
	)}, pagesFrame)
	i, _ := lineWith(t, lines, "  unspent · expiring soonest")
	requireLines(t, lines[i+1:i+4], []string{
		"  an-account-id-that-runs-past-t    0%  in  10.0h",
		"  b                                -1%  in  20.0h",
		"  c                                80%  in  30.0h",
	})
}

// The reference adds hours to a naive local datetime, so across a DST change
// the day labels count wall-clock hours, not elapsed ones.
func TestDayLabelsCountWallClockHours(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	beforeSpringForward := time.Date(2026, 3, 7, 23, 30, 0, 0, chicago)
	m := Derive(pageNamed(t, "02-cold-local").snapshot, beforeSpringForward)
	lines := strings.Split(render(t, m, pagesFrame), "\n")
	if lines[1][len(lines[1])-len("Sat 23:30"):] != "Sat 23:30" {
		t.Fatalf("headline clock: %q", lines[1])
	}
	axis, _ := lineWith(t, lines, "  1.0×└")
	if got, want := lines[axis+1], "        Sat                      Mon                      Wed                       Fri"; got != want {
		t.Fatalf("day labels\n got %q\nwant %q", got, want)
	}
}
