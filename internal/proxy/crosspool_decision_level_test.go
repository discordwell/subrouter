package proxy

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/selectacct"
)

// PATCHES §39: the `remote pool decision` line for a turn selection kept on its
// account without ranking it is Debug; every line a ranking produced stays
// Info. In the laptop's last 10 hours before §39 the kept lines were 99% of the
// decision lines and 55% of subrouter.err.log's bytes.

// At the daemon's default level (Info) a warm turn writes no decision line,
// and a cold turn ranked onto the box still writes one -- with the turn on the
// same accounts either way. Same laptop, same box, one logger at Info.
func TestKeptTurnDecisionLineIsDebugAndARankedOneStaysInfo(t *testing.T) {
	box := incidentBox(t, 0)
	laptop := newCrossPoolLaptop(t, []selectacct.Score{crossPoolClaudeScore("fifteen", 0.98, 159*time.Hour)}, box.pool)
	logs := &syncBuffer{}
	laptop.logs = logs
	laptop.server.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	for sessionID, idle := range map[string]time.Duration{"warm": 50 * time.Minute, "cold": 2 * time.Hour} {
		if _, err := laptop.store.Put("claude", sessionID, "fifteen", ""); err != nil {
			t.Fatal(err)
		}
		markIdle(laptop.active, sessionID, idle)
	}

	if status, servedBy := laptop.turn(t, "warm"); status != http.StatusOK || servedBy != "fifteen" {
		t.Fatalf("calibration: warm turn status %d served by %q; want fifteen", status, servedBy)
	}
	if decisions := laptop.logLines("remote pool decision"); len(decisions) != 0 {
		t.Fatalf("decision lines at Info after a warm turn: %v; a kept turn logs at Debug", decisions)
	}
	if !strings.Contains(logs.String(), `"msg":"proxy request"`) {
		t.Fatalf("calibration: the Info logger wrote no proxy request line:\n%s", logs.String())
	}

	if status, servedBy := laptop.turn(t, "cold"); status != http.StatusOK || servedBy != "chip-5" {
		t.Fatalf("calibration: cold turn status %d served by %q; want chip-5", status, servedBy)
	}
	decisions := laptop.logLines("remote pool decision")
	if len(decisions) != 1 || decisions[0]["session"] != "cold" || decisions[0]["target_account"] != "chip-5" || decisions[0]["level"] != "INFO" {
		t.Fatalf("decision lines %v; want one Info line for the cold turn, targeting chip-5", decisions)
	}
}

// The rule itself, over every shape of placement the decision line is written
// for. Only "kept, nothing ranked, nothing competing, first attempt" is Debug;
// a turn never ranked with no kept reason is a ranking that failed, and stays
// Info.
func TestCrossPoolDecisionLevel(t *testing.T) {
	peer := &crossPoolCandidate{Account: "acct-a"}
	ranked := &crossPoolRanking{ranked: true}
	kept := &crossPoolRanking{kept: crossPoolKeptWarm, sessionState: "warm"}
	for _, tc := range []struct {
		name      string
		ranking   *crossPoolRanking
		placement crossPoolPlacement
		recompute bool
		want      slog.Level
	}{
		{"warm: kept without ranking", kept, crossPoolPlacement{Local: true}, false, slog.LevelDebug},
		{"cold-retained: kept without ranking", &crossPoolRanking{kept: crossPoolKeptColdRetained, sessionState: "cold"}, crossPoolPlacement{Local: true}, false, slog.LevelDebug},
		{"selection did not rank this turn (a failed ranking)", &crossPoolRanking{}, crossPoolPlacement{Local: true}, false, slog.LevelInfo},
		{"no ranking at all", nil, crossPoolPlacement{Local: true}, false, slog.LevelInfo},
		{"a ranking ran and local won", ranked, crossPoolPlacement{Local: true}, false, slog.LevelInfo},
		{"local won against a peer", ranked, crossPoolPlacement{Local: true, BestPeer: peer}, false, slog.LevelInfo},
		{"a peer won", ranked, crossPoolPlacement{Peer: peer, BestPeer: peer}, false, slog.LevelInfo},
		{"a peer won but the body is too big to replay", kept, crossPoolPlacement{Local: true, BestPeer: peer}, false, slog.LevelInfo},
		{"recompute after a refusal lands local", ranked, crossPoolPlacement{Local: true}, true, slog.LevelInfo},
		{"recompute on an unranked turn", kept, crossPoolPlacement{Local: true}, true, slog.LevelInfo},
		{"local cannot serve", nil, crossPoolPlacement{}, false, slog.LevelInfo},
	} {
		if got := crossPoolDecisionLevel(tc.ranking, tc.placement, tc.recompute); got != tc.want {
			t.Errorf("%s: level %v, want %v", tc.name, got, tc.want)
		}
	}
}
