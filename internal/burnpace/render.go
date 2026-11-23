package burnpace

import (
	"cmp"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// Page is the terminal the read-out is drawn for, as raw facts. Defaults and
// clamps are Render's.
type Page struct {
	Cols   int  // terminal columns; 0 is unknown and draws at 92. More than 104 draws at 104.
	Lines  int  // terminal lines; 0 is unknown and counts as 40. Only the chart's height reads it.
	Colour bool // ANSI colour
}

// Every length is a count of runes, never of bytes or terminal cells: the
// reference script counts code points.
const (
	defaultCols  = 92
	defaultLines = 40
	maxCols      = 104
	nameCells    = 8
	paceCells    = 7
	cell0        = 2 + nameCells + paceCells + 2 // the first bar cell, and the indent of the tick and accounts lines
	barMinCells  = 20
	barTrim      = 15
	whyTrim      = 12 // a no-signal row's reason fits in cols - cell0 - whyTrim
	chartGutter  = 8
	chartMinCols = 24
	paceScale    = 3.5 // the bar's right end
	headRoomTrim = 15
	wrapTrim     = 6
	wrapMin      = 20
	accountCells = 30
)

const (
	sgrBold  = "1"
	sgrDim   = "2"
	sgrBlue  = "38;5;39"
	sgrDeep  = "38;5;24"
	sgrAmber = "38;5;214"
	sgrGrey  = "38;5;243"
	sgrWhite = "1;97"
)

var toneCodes = [...]string{Amber: sgrAmber, Dim: sgrDim, Grey: sgrGrey}

// Index k draws k+1 eighths. The reference indexes them with the floor of
// eighths, so a partial cell draws one eighth more than it holds; kept.
var (
	barEighths   = [...]string{"▏", "▎", "▍", "▌", "▋", "▊", "▉", "█"}
	chartEighths = [...]string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}
)

// Render writes the whole read-out for m with one Write, so a writer never
// sees half a page. The only error is the writer's.
func Render(w io.Writer, m Model, p Page) error {
	l := layoutFor(p)
	c := paint{on: p.Colour}
	var out []string
	add := func(lines ...string) { out = append(out, lines...) }
	sayAll := func(notes ...Note) {
		for _, n := range notes {
			add(say(n, l, c)...)
		}
	}

	add("")
	add(headline(m, l, c)...)
	add("  " + c.span(sgrDim, repeat("─", l.cols-4)))
	sayAll(m.Top()...)
	add("")

	switch m.Outcome() {
	case NothingRead:
		sayAll(Note{Tone: Amber, Text: "no scope could be read, so there are no numbers to show"})
	case NothingMeasured:
		sayAll(Note{Tone: Amber, Text: "no scope that answered has a measured account, so there are no numbers to show"})
		sayAll(leftOutNotes(m)...)
		sayAll(m.Bottom()...)
	default:
		for _, prov := range m.Providers {
			add(providerRow(prov, l, c))
		}
		add(tickLine(l, c), accountsLine(m, l, c))
		sayAll(leftOutNotes(m)...)
		if m.Stale > 0 {
			sayAll(Note{Tone: Grey, Text: fmt.Sprintf("%d in the numbers on a last-good reading (sweep cut this read)", m.Stale)})
		}
		if len(m.Conflicts) > 0 {
			add("  " + c.span(sgrAmber, "same account disagrees across scopes: "+strings.Join(m.Conflicts[:min(3, len(m.Conflicts))], ", ")))
		}
		if charted, ok := m.Charted(); ok {
			add(chart(charted, m.Now, l, c)...)
			for _, other := range m.Providers {
				if other.Name != charted.Name && other.Signal() {
					add("  " + c.span(sgrDim, "("+other.Name+" ceiling not charted)"))
				}
			}
		}
		add(expiring(m.Expiring(), c)...)
		sayAll(m.Bottom()...)
	}
	add("")

	_, err := io.WriteString(w, strings.Join(out, "\n")+"\n")
	return err
}

type layout struct {
	cols    int
	bar     int // cells of the pace bar
	chart   int // columns of the ceiling chart
	wrap    int // wrap limit of a status line
	maxRows int // a soft cap on the chart's rows
}

func layoutFor(p Page) layout {
	cols := p.Cols
	if cols <= 0 {
		cols = defaultCols
	}
	cols = min(cols, maxCols)
	lines := p.Lines
	if lines <= 0 {
		lines = defaultLines
	}
	maxRows := 6
	switch {
	case lines >= 40:
		maxRows = 10
	case lines >= 32:
		maxRows = 8
	}
	return layout{
		cols:    cols,
		bar:     max(barMinCells, cols-cell0-barTrim),
		chart:   max(chartMinCols, cols-chartGutter-2),
		wrap:    max(wrapMin, cols-wrapTrim),
		maxRows: maxRows,
	}
}

// paint wraps text in one SGR span when colour is on, an empty text too, as
// the reference does. Each bar and chart cell is its own span.
type paint struct{ on bool }

func (c paint) span(code, text string) string {
	if !c.on {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func headline(m Model, l layout, c paint) []string {
	const head = "subrouter · 7d burn pace"
	room := l.cols - headRoomTrim
	clock := c.span(sgrGrey, m.Now.Format("Mon 15:04"))
	var labels []string
	for _, s := range m.Scopes() {
		label := m.Label(s)
		if s.Unmeasured > 0 {
			label += fmt.Sprintf(" (%d of %d measured)", s.Measured, s.Measured+s.Unmeasured)
		}
		labels = append(labels, label)
	}
	text := cmp.Or(strings.Join(labels, " + "), "nothing read")

	if tail := " · " + text; runeLen(head)+runeLen(tail) <= room {
		return []string{"  " + c.span(sgrBold, head) + tail + spaces(max(1, room-runeLen(head)-runeLen(tail))) + clock}
	}
	lines := []string{"  " + c.span(sgrBold, head) + spaces(max(1, room-runeLen(head))) + clock}
	for i, line := range wrap(text, l.wrap) {
		lines = append(lines, indent(i)+line)
	}
	return lines
}

// say is one status sentence, wrapped, with an indent that is never coloured.
func say(n Note, l layout, c paint) []string {
	var lines []string
	for i, line := range wrap(n.Text, l.wrap) {
		lines = append(lines, indent(i)+c.span(toneCodes[n.Tone], line))
	}
	return lines
}

func indent(line int) string {
	if line == 0 {
		return "  "
	}
	return "    "
}

func providerRow(p Provider, l layout, c paint) string {
	stem := "  " + c.span(sgrBold, padRight(p.Name, nameCells))
	if !p.Signal() {
		why := fmt.Sprintf("%.0f%% across %d accounts, under meter resolution", p.Drawn()*100, len(p.Accounts))
		if p.Share() < minShare {
			why = fmt.Sprintf("%.0f%% drawn over %.1fh of window", p.Drawn()*100, p.Share()*weekHours)
		}
		if avail := l.cols - cell0 - whyTrim; runeLen(why) > avail {
			why = strings.TrimRightFunc(firstRunes(why, max(0, avail-1)), pySpace) + "…"
		}
		return stem + c.span(sgrDim, "    –") + "    " + c.span(sgrAmber, "no signal") + c.span(sgrDim, " · "+why)
	}
	pace := p.Pace()
	ceiling, _ := p.CeilingAt(0)
	return stem + c.span(sgrBold, fmt.Sprintf("%5.2f× ", pace)) + " " + bar(pace, l.bar, pace > 1.0, c) +
		c.span(sgrGrey, fmt.Sprintf("  ceil %.2f×", ceiling))
}

// The 1× notch is drawn last, so the fill never hides the mark the pace is
// read against.
func bar(pace float64, cells int, over bool, c paint) string {
	notch := barNotch(cells)
	exact := max(0, min(float64(cells), pace/paceScale*float64(cells)))
	full := int(exact)
	rem := exact - float64(full)
	hue := sgrBlue
	if over {
		hue = sgrAmber
	}
	glyphs := make([]string, cells)
	for i := range glyphs {
		switch {
		case i < full:
			glyphs[i] = c.span(hue, "█")
		case i == full && rem >= 0.125:
			glyphs[i] = c.span(hue, barEighths[min(7, int(rem*8))])
		default:
			glyphs[i] = " "
		}
	}
	code := sgrDim
	if notch < full {
		code = sgrWhite
	}
	glyphs[notch] = c.span(code, "╎")
	return c.span(sgrDim, "▕") + strings.Join(glyphs, "") + c.span(sgrDim, "▏")
}

func barNotch(cells int) int { return int(math.RoundToEven(float64(cells) / paceScale)) }

func tickLine(l layout, c paint) string {
	return spaces(cell0) + c.span(sgrDim, place(l.bar,
		placed{"0×", 0}, placed{"1×", barNotch(l.bar)}, placed{"3.5×", l.bar - 4}))
}

// accountsLine names the counted scopes; one account reads "1 accounts", and
// Folded counts copies, as the reference prints them.
func accountsLine(m Model, l layout, c paint) string {
	var names []string
	for _, s := range m.Sources {
		if s.Standing == Counted {
			names = append(names, s.Scope())
		}
	}
	where := strings.Join(names, " + ")
	room := l.cols - cell0
	across, both := "from", "more than one scope"
	if len(names) > 1 {
		across = "across"
	}
	if len(names) == 2 {
		both = "both"
	}
	note := fmt.Sprintf("%d accounts %s %s", m.Accounts(), across, where)
	if m.Folded > 0 {
		note += fmt.Sprintf(" · %d held on %s, counted once", m.Folded, both)
	}
	if runeLen(note) > room {
		note = fmt.Sprintf("%d accounts, %s", m.Accounts(), where)
		if m.Folded > 0 {
			note += fmt.Sprintf(" · %d folded", m.Folded)
		}
	}
	if runeLen(note) > room {
		note = strings.TrimRightFunc(firstRunes(note, max(0, room-1)), pySpace) + "…"
	}
	return spaces(cell0) + c.span(sgrDim, note)
}

// The kinds are counted in LeftOutReason's order.
func leftOutNotes(m Model) []Note {
	if len(m.LeftOut) == 0 {
		return nil
	}
	var count [LeftRolled + 1]int
	names := make([]string, len(m.LeftOut))
	for i, lo := range m.LeftOut {
		count[lo.Why]++
		names[i] = lo.Name()
	}
	var parts []string
	for why := LeftUnmeasured; why <= LeftRolled; why++ {
		if count[why] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count[why], why))
		}
	}
	return []Note{{Tone: Amber, Text: strings.Join(parts, " · ") + " — not in the numbers: " + strings.Join(names, ", ")}}
}

// chart draws the next 168 h of p's ceiling at this terminal's resolution.
// Its row cap is soft: a peak too high for the cap at half-steps still draws
// every half-step.
func chart(p Provider, now time.Time, l layout, c paint) []string {
	sw := l.chart
	values := make([]float64, sw)
	peak := 0.0
	for i := range values {
		if v, ok := p.CeilingAt(float64(i) * weekHours / float64(sw-1)); ok {
			values[i] = v
			peak = max(peak, v)
		}
	}
	hi := max(2.0, math.Ceil(peak/0.5)*0.5)
	rows := int(math.RoundToEven((hi - 1.0) / 0.25))
	if rows > l.maxRows {
		rows = int(math.RoundToEven((hi - 1.0) / 0.5))
	}
	step := (hi - 1.0) / float64(rows)

	heights := make([]float64, sw)
	for i, v := range values {
		heights[i] = max(0, min(float64(rows), (v-1.0)/step))
	}

	lines := []string{"", "  " + c.span(sgrBold, "ceiling · next 168h") + c.span(sgrGrey, fmt.Sprintf("   %s, %d accounts", p.Name, len(p.Accounts)))}
	for r := rows - 1; r >= 0; r-- {
		var cells strings.Builder
		for _, h := range heights {
			top := -1
			if h > 0 {
				top = max(0, int(math.Ceil(h))-1)
			}
			switch {
			case r < top:
				cells.WriteString(c.span(sgrDeep, "░"))
			case r == top:
				cells.WriteString(c.span(sgrBlue, chartEighths[min(7, int((h-float64(r))*8))]))
			default:
				cells.WriteByte(' ')
			}
		}
		val := 1.0 + step*float64(r+1)
		tick, edge := "", "│"
		if math.Abs(val*2-math.RoundToEven(val*2)) < 1e-9 {
			tick, edge = fmt.Sprintf("%.1f×", val), "┤"
		}
		lines = append(lines, c.span(sgrDim, padLeft(tick, 6))+c.span(sgrDim, edge)+" "+cells.String())
	}
	lines = append(lines, c.span(sgrDim, "  1.0×")+c.span(sgrDim, "└"+repeat("─", sw+1)))

	var days []placed
	for _, h := range []int{0, 48, 96, 144} {
		days = append(days, placed{weekdayAfter(now, h), int(float64(h) / weekHours * float64(sw-1))})
	}
	return append(lines, spaces(chartGutter)+c.span(sgrDim, place(sw, days...)))
}

// weekdayAfter adds hours to now's wall clock, not to the instant, as the
// reference's naive local datetime does, so a day label across a DST change
// names the same day.
func weekdayAfter(now time.Time, hours int) string {
	wall := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), time.UTC)
	return wall.Add(time.Duration(hours) * time.Hour).Format("Mon")
}

// A spent account lists too ("  0%"), as the reference lists it.
func expiring(accounts []Account, c paint) []string {
	if len(accounts) == 0 {
		return nil
	}
	lines := []string{"", "  " + c.span(sgrBold, "unspent · expiring soonest")}
	for _, a := range accounts {
		lines = append(lines, "  "+c.span(sgrGrey, padRight(firstRunes(a.Name(), accountCells), accountCells+2))+
			c.span(sgrBold, fmt.Sprintf("%3.0f%%", (1-a.Used)*100))+
			c.span(sgrDim, fmt.Sprintf("  in %5.1fh", a.ResetHours)))
	}
	return lines
}

type placed struct {
	text string
	at   int
}

func place(cells int, labels ...placed) string {
	line := []rune(spaces(cells))
	for _, label := range labels {
		for j, r := range []rune(label.text) {
			if k := label.at + j; k >= 0 && k < cells {
				line[k] = r
			}
		}
	}
	return strings.TrimRightFunc(string(line), pySpace)
}

func repeat(s string, n int) string   { return strings.Repeat(s, max(0, n)) }
func spaces(n int) string             { return repeat(" ", n) }
func padRight(s string, n int) string { return s + spaces(n-runeLen(s)) }
func padLeft(s string, n int) string  { return spaces(n-runeLen(s)) + s }

func firstRunes(s string, n int) string {
	r := []rune(s)
	return string(r[:min(n, len(r))])
}
