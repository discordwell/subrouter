package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
)

// PATCHES §31, R3 fix 1: a token refresh that got no verdict on the box's own
// usage sweep is a blip, whatever the box's last-good cache holds.
//
// The box's sweep refreshes an expired access token before it reads usage. A
// refresh the token endpoint answered 503 (or that never connected) reached no
// verdict, and the request path already treats it so (errRefreshTransient: the
// session keeps its account). The published row said otherwise whenever the
// last-good backfill had nothing to paper over it with -- the first 15 minutes
// after every restart (every deploy), a newly added account, or a row whose
// usage reads had failed for 15 minutes -- and the laptop read the row as a
// checked dead login and ended every warm hold on the account as a forced
// switch. P8 lists "a refresh with no verdict" as transient.

// e2eBoxTokenBlip makes the box's token endpoint answer 503 while blip is set
// and a working exchange (the same token back) otherwise. Installed before the
// box serves anything: its sweep and its request path share the one client.
func e2eBoxTokenBlip(box *e2eBox) *atomic.Bool {
	blip := &atomic.Bool{}
	base := box.server.AccountRef.client.Transport
	box.server.AccountRef.client = &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.Contains(req.URL.Path, "/oauth/token") {
			return base.RoundTrip(req)
		}
		respond := func(status int, body string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
				Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		}
		if blip.Load() {
			return respond(http.StatusServiceUnavailable, `{"error":"upstream connect error"}`)
		}
		var exchange struct {
			RefreshToken string `json:"refresh_token"`
		}
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &exchange)
		name := strings.TrimPrefix(exchange.RefreshToken, "refresh-")
		return respond(http.StatusOK, `{"access_token":"tok-`+name+`","refresh_token":"refresh-`+name+`","expires_in":7200}`)
	})}
	return blip
}

// e2eBoxRestartsWithAnExpiredToken leaves the box as a restart does -- no
// sweep cached, no last-good rows -- with account's access token due, so its
// next sweep has to refresh it before it can measure anything.
func e2eBoxRestartsWithAnExpiredToken(t *testing.T, box *e2eBox, account string) {
	t.Helper()
	credential := agentclaude.CredentialInfo{
		AccessToken: "tok-" + account, RefreshToken: "refresh-" + account,
		ExpiresAt: time.Now().Add(-time.Minute).UnixMilli(),
	}
	if err := box.server.AccountRef.claudeStore.ImportProfileCredential(account, credential); err != nil {
		t.Fatal(err)
	}
	ref := box.server.AccountRef
	ref.usageStatusMu.Lock()
	ref.lastGoodUsage = nil
	ref.usageStatusMu.Unlock()
	ref.InvalidateUsageStatusCache()
}

func TestE2EABoxRefreshBlipWithNoLastGoodRowKeepsTheHold(t *testing.T) {
	box := e2eIncidentBox(t)
	blip := e2eBoxTokenBlip(box)
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool())
	e2ePrime(t, laptop.server.RemotePool)
	if got := laptop.turn(t, "s1"); got.ServedBy != "chip-5" {
		t.Fatalf("turn 1 served by %q; want chip-5 (burndown)", got.ServedBy)
	}

	blip.Store(true)
	e2eBoxRestartsWithAnExpiredToken(t, box, "chip-5")
	e2eRefetch(t, laptop.server.RemotePool)

	// What the box published: nobody got an answer, so the row is a cut.
	var rows []remotePoolAccountUsage
	if err := json.Unmarshal(laptop.server.RemotePool.cachedUsage().Body, &rows); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.ID != "chip-5" {
			continue
		}
		found = true
		if !strings.Contains(row.Error, "503") {
			t.Fatalf("calibration: chip-5's row error %q; the sweep must have tried the refresh and got the 503", row.Error)
		}
		if row.knownInvalid() || !row.unmeasured() {
			t.Fatalf("chip-5 row sweep_cut=%v auth_checked=%v auth_valid=%v: a 503 from the token endpoint was published as a dead credential",
				row.SweepCut, row.AuthChecked != nil && *row.AuthChecked, row.AuthValid != nil && *row.AuthValid)
		}
		if v := row.Normalized.NewSession; v != nil && v.Reason == targetRefusedAuthInvalid {
			t.Fatalf("chip-5 verdict %+v: a refresh blip is not auth_invalid", *v)
		}
	}
	if !found {
		t.Fatalf("no chip-5 row in the box's body")
	}

	// The held turn: the hold is kept and the box is asked for chip-5, whose
	// request-time refresh blips too -- a transient answer, so this one turn is
	// served locally and the session stays tied to chip-5.
	if got := laptop.turn(t, "s1"); got.Status != http.StatusOK {
		t.Fatalf("turn 2 status %d; the blip turn is served locally", got.Status)
	}
	if account, held := e2eHold(laptop.server.RemotePool, "s1"); !held || account != "chip-5" {
		t.Fatalf("hold after the blip = %q/%v; a refresh with no verdict must keep chip-5", account, held)
	}
	if ended := e2eLogLines(laptop.logs, "remote pool hold ended"); len(ended) != 0 {
		t.Fatalf("hold ended on a blip: %v", ended)
	}
	if turns := box.seen(); len(turns) != 2 || turns[1].Target != "chip-5" {
		t.Fatalf("box turns %+v; turn 2 must be the held turn, targeted at chip-5", turns)
	}

	blip.Store(false)
	if got := laptop.turn(t, "s1"); got.ServedBy != "chip-5" {
		t.Fatalf("turn 3 served by %q; with the token endpoint back the session is on chip-5", got.ServedBy)
	}
}
