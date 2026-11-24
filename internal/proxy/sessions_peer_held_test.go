package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/tenant"
	"github.com/discordwell/subrouter/selectacct"
	"github.com/discordwell/subrouter/session"
)

// PATCHES §49: a daemon with ZERO local accounts whose turns are all served by
// a peer. Selection has no account to pin, so the session store never gets a
// row; the only record of where the session lives is the peer hold. Before
// §49, /_subrouter/sessions listed only the store, so the session had no row
// at all and `srq serving` printed "unassigned" and exited 1 on a setup that
// was working.

// e2eSessionRows reads the daemon's /_subrouter/sessions as JSON, the way srq
// and the cutover canary read it: as untyped rows.
func e2eSessionRows(t *testing.T, server Server) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/_subrouter/sessions", nil)
	req.Header.Set("Authorization", "Bearer admin")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /_subrouter/sessions = %d: %s", recorder.Code, recorder.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode /_subrouter/sessions %q: %v", recorder.Body.String(), err)
	}
	return rows
}

func e2eSessionRowsFor(rows []map[string]any, agent, sessionID string) []map[string]any {
	var out []map[string]any
	for _, row := range rows {
		if row["agent_type"] == agent && row["session_id"] == sessionID {
			out = append(out, row)
		}
	}
	return out
}

// A zero-account laptop: every turn goes to the peer, and the session it
// serves is listed as served there -- which peer, which account, no local
// account -- not left out.
func TestSessionsListsAPeerHeldSessionWithNoLocalAccount(t *testing.T) {
	now := time.Now()
	box := newE2EBox(t, map[string]string{"b1": e2eUsage(now, 5, 3*time.Hour, 10, 100*time.Hour)})
	laptop := newE2ELaptop(t, map[string]string{}, box.newPool().WithName("box"))
	e2ePrime(t, laptop.server.RemotePool)

	got := laptop.turn(t, "s1")
	if got.Status != http.StatusOK || got.ServedBy != "b1" || got.Pool != "remote" || got.Peer != "box" {
		t.Fatalf("calibration: turn status %d served by %q pool %q peer %q; want b1 on box", got.Status, got.ServedBy, got.Pool, got.Peer)
	}
	if _, ok := laptop.store.Get("claude", "s1"); ok {
		t.Fatal("calibration: a zero-account daemon wrote a local assignment")
	}
	if _, hold, ok := laptop.server.peerHold(session.ScopedSessionKey("claude", "s1")); !ok || hold.Account != "b1" {
		t.Fatalf("calibration: peer hold %+v %v; want b1", hold, ok)
	}

	rows := e2eSessionRowsFor(e2eSessionRows(t, laptop.server), "claude", "s1")
	if len(rows) != 1 {
		t.Fatalf("/_subrouter/sessions rows for s1 = %v; want exactly one", rows)
	}
	row := rows[0]
	if row["served_by"] != "remote" || row["served_peer"] != "box" || row["served_account"] != "b1" {
		t.Fatalf("row %v; want served_by remote on box, account b1", row)
	}
	if account, _ := row["account_id"].(string); account != "" {
		t.Fatalf("row %v names local account %q; this daemon has none", row, account)
	}
	updated, _ := row["updated_at"].(string)
	if at, err := time.Parse(time.RFC3339Nano, updated); err != nil || at.Before(now.Add(-time.Minute)) || at.After(time.Now().Add(time.Minute)) {
		t.Fatalf("row updated_at %q; want the hold's last activity, about now", updated)
	}
}

// The untargeted last resort (Deviation X2) -- an old peer that publishes no
// verdicts, so nothing is eligible and the turn is forwarded untargeted -- also
// leaves a hold, so it is listed the same way.
func TestSessionsListsAnUntargetedPeerServeWithNoLocalAccount(t *testing.T) {
	box := newE2EBox(t, map[string]string{"b1": e2eUsage(time.Now(), 5, 3*time.Hour, 10, 100*time.Hour)})
	box.legacy = true
	laptop := newE2ELaptop(t, map[string]string{}, box.newPool().WithName("box"))
	e2ePrime(t, laptop.server.RemotePool)

	got := laptop.turn(t, "s1")
	turns := box.seen()
	if got.Status != http.StatusOK || got.Pool != "remote" || len(turns) != 1 || turns[0].Target != "" {
		t.Fatalf("calibration: status %d pool %q, box turns %+v; want one untargeted forward", got.Status, got.Pool, turns)
	}
	rows := e2eSessionRowsFor(e2eSessionRows(t, laptop.server), "claude", "s1")
	if len(rows) != 1 || rows[0]["served_by"] != "remote" || rows[0]["served_peer"] != "box" || rows[0]["served_account"] != got.ServedBy {
		t.Fatalf("/_subrouter/sessions rows for s1 = %v; want one, served by %s on box", rows, got.ServedBy)
	}
}

// CHARACTERIZATION (the laptop: local accounts AND a peer): a session with a
// local assignment is listed exactly as before -- once, with its local
// account -- whether it is served locally or held on the peer. Shown red by
// mutation: listing every hold without skipping the assigned ones lists the
// held session twice.
func TestSessionsWithALocalAssignmentAreListedOnceAsBefore(t *testing.T) {
	box := e2eIncidentBox(t)
	laptop := newE2ELaptop(t, e2eIncidentLocal(), box.newPool().WithName("box"))
	e2ePrime(t, laptop.server.RemotePool)
	if got := laptop.turn(t, "held"); got.ServedBy != "chip-5" || got.Pool != "remote" {
		t.Fatalf("calibration: held turn served by %q pool %q; want chip-5 on the peer", got.ServedBy, got.Pool)
	}
	placeholder := laptop.pinnedTo("held")
	if placeholder == "" {
		t.Fatal("calibration: the peer-served turn left no local placeholder assignment")
	}
	if _, err := laptop.store.Put("claude", "local", "fifteen", ""); err != nil {
		t.Fatal(err)
	}

	rows := e2eSessionRows(t, laptop.server)
	if len(rows) != 2 {
		t.Fatalf("/_subrouter/sessions = %v; want the two assigned sessions, once each", rows)
	}
	held := e2eSessionRowsFor(rows, "claude", "held")
	if len(held) != 1 || held[0]["account_id"] != placeholder || held[0]["served_by"] != "remote" ||
		held[0]["served_account"] != "chip-5" || held[0]["served_peer"] != "box" {
		t.Fatalf("held rows %v; want one, account_id %s, served by chip-5 on box", held, placeholder)
	}
	local := e2eSessionRowsFor(rows, "claude", "local")
	if len(local) != 1 || local[0]["account_id"] != "fifteen" || local[0]["served_by"] != "local" {
		t.Fatalf("local rows %v; want one, fifteen, served locally", local)
	}
}

// A hold that has lapsed is not listed: once the idle window has passed, the
// session is no longer held anywhere, and srq's "unassigned" for it is true.
// The live hold beside it calibrates that this peer's holds are read at all.
func TestSessionsDoNotListALapsedPeerHold(t *testing.T) {
	server, _, _, _ := e2eMeasuredServer(t, map[string]string{})
	peer := NewRemotePool("http://127.0.0.1:1", "", "").WithName("box")
	server.RemotePool = peer
	now := time.Now()
	peer.delegations.set(session.ScopedSessionKey("claude", "live"), now, time.Hour, "b1", "box")
	peer.delegations.set(session.ScopedSessionKey("claude", "lapsed"), now.Add(-3*time.Hour), time.Hour, "b1", "box")

	rows := e2eSessionRows(t, server)
	if live := e2eSessionRowsFor(rows, "claude", "live"); len(live) != 1 || live[0]["served_peer"] != "box" {
		t.Fatalf("calibration: live hold rows %v; want one on box", live)
	}
	if lapsed := e2eSessionRowsFor(rows, "claude", "lapsed"); len(lapsed) != 0 {
		t.Fatalf("lapsed hold rows %v; want none: the hold expired", lapsed)
	}
}

// Two peers holding one session (only a hold loaded from disk that no claim has
// superseded): the row names the first peer in configuration order, the one
// peerHold -- and so routing -- follows.
func TestSessionsNameTheFirstPeerHoldingASession(t *testing.T) {
	server, _, _, _ := e2eMeasuredServer(t, map[string]string{})
	first := NewRemotePool("http://127.0.0.1:1", "", "").WithName("first")
	second := NewRemotePool("http://127.0.0.1:2", "", "").WithName("second")
	server.RemotePeers = []*RemotePool{first, second}
	key := session.ScopedSessionKey("claude", "s1")
	first.delegations.set(key, time.Now(), time.Hour, "a1", "first")
	second.delegations.set(key, time.Now(), time.Hour, "b1", "second")
	if p, _, ok := server.peerHold(key); !ok || p != first {
		t.Fatal("calibration: peerHold does not answer with the first peer")
	}

	rows := e2eSessionRowsFor(e2eSessionRows(t, server), "claude", "s1")
	if len(rows) != 1 || rows[0]["served_peer"] != "first" || rows[0]["served_account"] != "a1" {
		t.Fatalf("rows for s1 = %v; want one, a1 on first", rows)
	}
}

// A tenant server copies the base daemon's peers, and with them every peer
// hold: holds are keyed by session alone, not by tenant. Listing held-only
// rows there would hand a tenant other tenants' and the operator's session IDs
// and peer account names, so a tenant lists its own assignments only, as
// before §49.
func TestTenantSessionsDoNotListPeerHolds(t *testing.T) {
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	peer := NewRemotePool("http://127.0.0.1:1", "", "").WithName("box")
	base := Server{
		Upstream:     mustURL(t, "http://127.0.0.1:1"),
		Sessions:     sessions,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
		AdminToken:   "admin",
		RemotePool:   peer,
	}
	registry := tenant.NewRegistry(t.TempDir())
	handler := (&MultiTenant{Base: base, Registry: registry}).Handler(base.Handler())
	_, key, err := registry.Create("alpha")
	if err != nil {
		t.Fatal(err)
	}
	peer.delegations.set(session.ScopedSessionKey("claude", "operator-private-session"), time.Now(), time.Hour, "b1", "box")

	get := func(path, token string) []map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, recorder.Code, recorder.Body.String())
		}
		var rows []map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode %s %q: %v", path, recorder.Body.String(), err)
		}
		return rows
	}
	if rows := e2eSessionRowsFor(get("/_subrouter/sessions", "admin"), "claude", "operator-private-session"); len(rows) != 1 {
		t.Fatalf("calibration: the operator's listing %v; want the held session", rows)
	}
	if rows := get("/t/"+key+"/_subrouter/sessions", ""); len(rows) != 0 {
		t.Fatalf("tenant listing %v; want no rows: the hold is not the tenant's", rows)
	}
}
