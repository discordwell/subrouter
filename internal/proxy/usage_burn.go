package proxy

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Burn (PATCHES §92.2): how fast each account spends each meter, read from
// its usage history, and the pool's figure across this daemon's accounts and
// its peers'. Shown in usage-status, at UsageBurnPath and by `srq burn`.
// Nothing ranks or routes on it.

// UsageBurnPath is the pool figure's route.
const UsageBurnPath = "/_subrouter/burn.json"

const (
	// usageMeterLength is the window of every recorded meter: Claude's week
	// and Fable meter, Codex's week and a model pool's own week.
	usageMeterLength = 7 * 24 * time.Hour
	// usageBurnMaxGap is the longest stretch between two points that counts
	// as covered: the heartbeat writes a point at least every 10 minutes
	// while readings arrive, so a longer one is a time with no readings.
	usageBurnMaxGap = 2 * usageHistoryHeartbeat
	// usageBurnMinCover is the share of a window the series must cover for
	// the window to have a figure.
	usageBurnMinCover = 0.5
	// usageBurnAtLimit is the used fraction at which an account counts as at
	// its limit: it could take no more of the demand sent to it.
	usageBurnAtLimit = 0.995
	// usageBurnPeerMaxAge is the oldest peer body whose figures are counted,
	// the age at which a peer's carried reading ends (remotePoolCarryTTL).
	usageBurnPeerMaxAge = remotePoolCarryTTL
)

// usageBurnWindow is one window a figure is read over: length long, ending
// before now by before.
type usageBurnWindow struct {
	name           string
	length, before time.Duration
}

// The windows, and the trends read from them: the 6 and 24 hours before the
// last 6 and 24.
var (
	usageBurnWindows = []usageBurnWindow{
		{"1h", time.Hour, 0}, {"6h", 6 * time.Hour, 0}, {"24h", 24 * time.Hour, 0},
		{"6h_prev", 6 * time.Hour, 6 * time.Hour}, {"24h_prev", 24 * time.Hour, 24 * time.Hour},
	}
	usageBurnPoolWindows = []string{"1h", "6h", "24h"}
	usageBurnTrends      = []struct{ name, last, before string }{{"6h", "6h", "6h_prev"}, {"24h", "24h", "24h_prev"}}
	// usageBurnSpan is how far back the oldest window reaches.
	usageBurnSpan = 48 * time.Hour
)

// UsageBurnFigure is one meter of one account over one window.
type UsageBurnFigure struct {
	// Burn is the rise in used inside the window, divided by the window's
	// share of the meter's week: 1 spends the budget exactly as fast as it
	// lasts. Absent when the series covers less than half the window.
	Burn *float64 `json:"burn,omitempty"`
	// Covered is the share of the window the series covers.
	Covered float64 `json:"covered"`
	// AtLimit is the share of the covered time the meter read at or above
	// 99.5 % used, published with Burn.
	AtLimit float64 `json:"at_limit,omitempty"`
}

// UsageMeterBurn is one meter's figures, keyed by window: 1h, 6h, 24h, and
// 6h_prev and 24h_prev, the 6 and 24 hours before those.
type UsageMeterBurn map[string]UsageBurnFigure

// UsageBurn is one account's figures, keyed by meter as the series names it:
// week, fable, week:<feature>.
type UsageBurn map[string]UsageMeterBurn

// accountBurn is account's figures at now, one per meter its series holds.
func (h *UsageHistory) accountBurn(provider, account string, now time.Time) UsageBurn {
	if h == nil {
		return nil
	}
	since := now.Add(-usageBurnSpan - usageBurnMaxGap).UnixMicro()
	type meterSeries struct {
		meter  string
		points []usageMeterReading
		latest usageMeterReading
	}
	var found []meterSeries
	h.mu.Lock()
	for key, series := range h.series {
		if key.provider != provider || key.account != account || len(series.points) == 0 {
			continue
		}
		// One point before the oldest window, so a stretch across its start
		// is read too.
		start := sort.Search(len(series.points), func(i int) bool { return series.points[i].At >= since })
		start = max(start-1, 0)
		found = append(found, meterSeries{key.meter, append([]usageMeterReading(nil), series.points[start:]...), series.latest})
	}
	h.mu.Unlock()
	if len(found) == 0 {
		return nil
	}
	out := UsageBurn{}
	for _, series := range found {
		// Claims only name a segment's cause; its bounds are the points'.
		out[series.meter] = meterBurn(usageSegments(series.points, nil), series.latest, now)
	}
	return out
}

// meterBurn reads each window from one meter's segments. Burn sums the rise
// between consecutive points of one segment, the part of each stretch inside
// the window, so a fall or a jump where a segment starts adds nothing.
// Coverage is the time inside the window between consecutive points no more
// than usageBurnMaxGap apart, up to the newest reading.
func meterBurn(segments []usageSegment, latest usageMeterReading, now time.Time) UsageMeterBurn {
	out := UsageMeterBurn{}
	end := now.UnixMicro()
	for _, window := range usageBurnWindows {
		to := end - window.before.Microseconds()
		from := to - window.length.Microseconds()
		var rise, covered, atLimit float64
		cover := func(left usageMeterReading, at int64) {
			if at-left.At > usageBurnMaxGap.Microseconds() {
				return
			}
			inside := overlapMicros(left.At, at, from, to)
			covered += inside
			if left.Used >= usageBurnAtLimit {
				atLimit += inside
			}
		}
		var prev *usageMeterReading
		for _, segment := range segments {
			for i := range segment.Points {
				point := segment.Points[i]
				if prev != nil && point.At > prev.At {
					if i > 0 {
						rise += (point.Used - prev.Used) * overlapMicros(prev.At, point.At, from, to) / float64(point.At-prev.At)
					}
					cover(*prev, point.At)
				}
				prev = &segment.Points[i]
			}
		}
		if prev != nil && latest.At > prev.At {
			cover(*prev, latest.At)
		}
		length := float64(window.length.Microseconds())
		figure := UsageBurnFigure{Covered: roundBurn(covered / length)}
		if covered >= usageBurnMinCover*length {
			burn := roundBurn(math.Max(0, rise) / (length / float64(usageMeterLength.Microseconds())))
			figure.Burn = &burn
			figure.AtLimit = roundBurn(atLimit / covered)
		}
		out[window.name] = figure
	}
	return out
}

// overlapMicros is how much of [a, b] lies inside [from, to].
func overlapMicros(a, b, from, to int64) float64 {
	return float64(max(min(b, to)-max(a, from), 0))
}

func roundBurn(v float64) float64 { return math.Round(v*1000) / 1000 }

// withUsageBurn adds each row's figures to usage-status, for this daemon's
// own accounts, so the peer link carries them. None with the history off.
func (r *AccountRef) withUsageBurn(statuses []AccountUsageStatus) []AccountUsageStatus {
	h := r.history()
	if h == nil {
		return statuses
	}
	now := h.clock()
	out := append([]AccountUsageStatus(nil), statuses...)
	for i := range out {
		if provider := usageHistoryProvider(accounts.Account{ID: out[i].ID, Provider: out[i].Provider}); provider != "" {
			out[i].Burn = h.accountBurn(provider, out[i].ID, now)
		}
	}
	return out
}

// UsageBurnReport is UsageBurnPath's body.
type UsageBurnReport struct {
	GeneratedAt time.Time `json:"generated_at"`
	// Meters is the pool figure per meter: Claude's week and Fable meter and
	// Codex's week always, then each Codex model pool's own week.
	Meters []UsagePoolMeter `json:"meters"`
	// Peers is every configured peer and whether its accounts are counted.
	Peers []UsageBurnPeer `json:"peers"`
}

// UsagePoolMeter is one meter across the pool.
type UsagePoolMeter struct {
	Provider string `json:"provider"`
	Meter    string `json:"meter"`
	Label    string `json:"label"`
	// Windows is the pool figure over the last 1, 6 and 24 hours.
	Windows map[string]UsagePoolWindow `json:"windows"`
	// Trend is the change from the 6 and the 24 hours before, keyed 6h and
	// 24h.
	Trend map[string]UsagePoolTrend `json:"trend"`
	// Accounts is each account's own figures, this daemon's first.
	Accounts []UsageAccountBurn `json:"accounts"`
	// Need is whether the pool needs its saved resets on the meter, from its
	// pressure (PATCHES §94.1).
	Need *MeterNeed `json:"need,omitempty"`
}

// UsagePoolWindow is one window's pool figure.
type UsagePoolWindow struct {
	// Burn is the mean of the counted accounts' burn, each account weighing
	// the same; null when no account has enough history.
	Burn *float64 `json:"burn"`
	// Accounts is how many accounts are counted, NotEnoughHistory how many
	// have the meter with less than half the window covered.
	Accounts         int `json:"accounts"`
	NotEnoughHistory int `json:"not_enough_history"`
	// AtLimit is the share of the counted accounts' covered time spent at or
	// above 99.5 % used: demand a spent account could not serve, which burn
	// does not show.
	AtLimit *float64 `json:"at_limit"`
}

// UsagePoolTrend is the mean change in burn from the window before, over the
// accounts with enough history in both.
type UsagePoolTrend struct {
	Change   *float64 `json:"change"`
	Accounts int      `json:"accounts"`
}

// UsageAccountBurn is one account's figures for one meter.
type UsageAccountBurn struct {
	// Pool is local or the peer's name.
	Pool    string         `json:"pool"`
	ID      string         `json:"id"`
	Label   string         `json:"label,omitempty"`
	Windows UsageMeterBurn `json:"windows"`
}

// UsageBurnPeer says whether a peer's accounts are counted, and why not.
type UsageBurnPeer struct {
	Name     string `json:"name"`
	Counted  bool   `json:"counted"`
	Accounts int    `json:"accounts,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// usageBurnAccount is one account's figures from one pool.
type usageBurnAccount struct {
	pool, provider, id, label string
	burn                      UsageBurn
}

// handleUsageBurn serves the pool figure over this daemon's own accounts and
// every peer's, each read as a routing decision reads it.
func (s Server) handleUsageBurn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h := s.AccountRef.history()
	if h == nil {
		http.Error(w, "this daemon keeps no usage history (SUBROUTER_USAGE_HISTORY=0)", http.StatusNotFound)
		return
	}
	now := h.clock()
	var rows []usageBurnAccount
	for _, account := range s.accountListContext(r.Context()) {
		provider := usageHistoryProvider(account)
		// Left out as resetPoolOf leaves it out (§98), so burn.json, the
		// decision and the choice read one pool.
		if provider == "" || s.pinOnlyAccount(account.ID) {
			continue
		}
		if burn := h.accountBurn(provider, account.ID, now); len(burn) > 0 {
			rows = append(rows, usageBurnAccount{poolNameLocal, provider, account.ID, account.Label, burn})
		}
	}
	peers := s.peers()
	deadline := time.Now().Add(peerViewReadWait)
	reads := make([]usageBurnPeerRead, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reads[i] = readPeerBurn(p.name(), p.usageForDecision(deadline))
		}()
	}
	wg.Wait()
	report := UsageBurnReport{GeneratedAt: now.UTC(), Peers: []UsageBurnPeer{}}
	for _, read := range reads {
		report.Peers = append(report.Peers, read.peer)
		rows = append(rows, read.rows...)
	}
	report.Meters = usagePoolMeters(rows)
	s.AccountRef.sharedResetNeeds().hold(report.Meters, now)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, report)
}

type usageBurnPeerRead struct {
	peer UsageBurnPeer
	rows []usageBurnAccount
	// holders is the counted peer's accounts that hold saved resets, with or
	// without figures (§94.1's expiry window).
	holders []resetHolder
}

// readPeerBurn takes the figures out of a peer's usage-status body. A peer
// none of whose rows carries burn is not counted, and neither are its
// accounts: a build from before §92 and one with its history off look alike.
func readPeerBurn(name string, usage AdminSnapshot) usageBurnPeerRead {
	read := usageBurnPeerRead{peer: UsageBurnPeer{Name: name}}
	if len(usage.Body) == 0 {
		read.peer.Reason = "no usage reading from it yet"
		if usage.Err != "" {
			read.peer.Reason += ": " + peerViewError(usage.Err)
		}
		return read
	}
	if age := time.Since(usage.At); age > usageBurnPeerMaxAge {
		read.peer.Reason = "its last usage reading is " + age.Round(time.Minute).String() + " old"
		return read
	}
	var rows []struct {
		ID       string                 `json:"id"`
		Provider string                 `json:"provider"`
		Label    string                 `json:"label"`
		Burn     UsageBurn              `json:"burn"`
		Resets   []accounts.HeldReset   `json:"resets"`
		Windows  []accounts.UsageWindow `json:"windows"`
		// The peer's own verdict: pin_only is no part of its pool (§98).
		Normalized struct {
			NewSession *struct {
				Reason string `json:"reason"`
			} `json:"new_session"`
		} `json:"normalized"`
	}
	if err := json.Unmarshal(usage.Body, &rows); err != nil {
		read.peer.Reason = "its usage body did not decode"
		return read
	}
	var holders []resetHolder
	for _, row := range rows {
		if verdict := row.Normalized.NewSession; verdict != nil && verdict.Reason == selectacct.VerdictPinOnly {
			continue
		}
		provider := string(accountProviderOrCodex(accounts.Account{Provider: accounts.Provider(row.Provider)}))
		if holder, ok := resetHolderOf(provider, row.ID, row.Resets, row.Windows); ok {
			holders = append(holders, holder)
		}
		if len(row.Burn) == 0 {
			continue
		}
		read.rows = append(read.rows, usageBurnAccount{name, provider, row.ID, row.Label, row.Burn})
	}
	if len(read.rows) == 0 {
		read.peer.Reason = "it publishes no burn: a build from before §92, or its usage history is off"
		return read
	}
	read.peer.Counted, read.peer.Accounts, read.holders = true, len(read.rows), holders
	return read
}

// usagePoolMeters is the pool figure per meter. An account held by two
// pools is counted once, from the first: this daemon's own reading first.
func usagePoolMeters(rows []usageBurnAccount) []UsagePoolMeter {
	type meterKey struct{ provider, meter string }
	keys := []meterKey{
		{string(accounts.ProviderClaude), usageMeterWeek},
		{string(accounts.ProviderClaude), usageMeterFable},
		{string(accounts.ProviderCodex), usageMeterWeek},
	}
	byMeter := map[meterKey][]UsageAccountBurn{}
	for _, key := range keys {
		byMeter[key] = []UsageAccountBurn{}
	}
	var extra []meterKey
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.provider+"\x00"+row.id] {
			continue
		}
		seen[row.provider+"\x00"+row.id] = true
		for meter, figures := range row.burn {
			key := meterKey{row.provider, meter}
			if _, ok := byMeter[key]; !ok {
				extra = append(extra, key)
			}
			byMeter[key] = append(byMeter[key], UsageAccountBurn{Pool: row.pool, ID: row.id, Label: row.label, Windows: figures})
		}
	}
	sort.Slice(extra, func(i, j int) bool {
		return extra[i].provider+"\x00"+extra[i].meter < extra[j].provider+"\x00"+extra[j].meter
	})
	out := make([]UsagePoolMeter, 0, len(keys)+len(extra))
	for _, key := range append(keys, extra...) {
		held := byMeter[key]
		windows := poolBurnWindows(held)
		out = append(out, UsagePoolMeter{
			Provider: key.provider, Meter: key.meter, Label: usageMeterLabel(key.provider, key.meter),
			Windows: windows, Trend: poolBurnTrends(held), Accounts: held, Need: meterNeed(windows, held),
		})
	}
	return out
}

func poolBurnWindows(held []UsageAccountBurn) map[string]UsagePoolWindow {
	out := map[string]UsagePoolWindow{}
	for _, name := range usageBurnPoolWindows {
		out[name] = poolWindowOf(held, name)
	}
	return out
}

// poolWindowOf is the pool figure of held over one window.
func poolWindowOf(held []UsageAccountBurn, name string) UsagePoolWindow {
	var window UsagePoolWindow
	var sum, covered, atLimit float64
	for _, account := range held {
		figure := account.Windows[name]
		if figure.Burn == nil {
			window.NotEnoughHistory++
			continue
		}
		window.Accounts++
		sum += *figure.Burn
		covered += figure.Covered
		atLimit += figure.AtLimit * figure.Covered
	}
	if window.Accounts > 0 {
		burn := roundBurn(sum / float64(window.Accounts))
		window.Burn = &burn
		if covered > 0 {
			share := roundBurn(atLimit / covered)
			window.AtLimit = &share
		}
	}
	return window
}

func poolBurnTrends(held []UsageAccountBurn) map[string]UsagePoolTrend {
	out := map[string]UsagePoolTrend{}
	for _, trend := range usageBurnTrends {
		var change UsagePoolTrend
		var sum float64
		for _, account := range held {
			last, before := account.Windows[trend.last].Burn, account.Windows[trend.before].Burn
			if last == nil || before == nil {
				continue
			}
			change.Accounts++
			sum += *last - *before
		}
		if change.Accounts > 0 {
			mean := roundBurn(sum / float64(change.Accounts))
			change.Change = &mean
		}
		out[trend.name] = change
	}
	return out
}

// usageMeterLabel names a meter for a person: Claude week, Claude Fable,
// Codex week, Codex <feature> week.
func usageMeterLabel(provider, meter string) string {
	name := provider
	switch provider {
	case string(accounts.ProviderClaude):
		name = "Claude"
	case string(accounts.ProviderCodex):
		name = "Codex"
	}
	switch {
	case meter == usageMeterWeek:
		return name + " week"
	case meter == usageMeterFable:
		return name + " Fable"
	case strings.HasPrefix(meter, usageMeterPoolPrefix):
		return name + " " + strings.TrimPrefix(meter, usageMeterPoolPrefix) + " week"
	}
	return name + " " + meter
}
