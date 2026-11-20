package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func deleteTestServer(t *testing.T) (admin http.Handler, public http.Handler, store agentclaude.Store) {
	t.Helper()
	store = agentclaude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("seven-alpha"); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	ref.claudeStore = store
	server := Server{AccountRef: ref, AdminToken: "admin-secret",
		LocalProxyToken: "proxy-secret", AccountImportToken: "import-secret"}
	// PublicHandler is a DIFFERENT mux from Handler. Asserting the public gate
	// against Handler() would pass no matter what the public listener does.
	return server.Handler(), server.PublicHandler(), store
}

func adminDelete(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, target, nil)
	req.RemoteAddr = "100.64.0.20:4321" // tailnet, not loopback
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Removal had no route on the single-tenant path at all, so dropping an account
// meant hand-editing claude.json over a root shell.
//
// The listing assertion below does NOT prove in-memory eviction: with the
// reloadAccounts call removed the test still passes, because reads reload on a
// disk-generation change. It proves the account is gone from both stores and
// from what callers see, which is the contract that matters here.
func TestAccountDeleteRemovesAccountFromBothStores(t *testing.T) {
	handler, _, store := deleteTestServer(t)

	rec := adminDelete(t, handler, "/_subrouter/accounts?account=seven-alpha&confirm=seven-alpha")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if _, ok := store.FindProfile("seven-alpha"); ok {
		t.Fatal("profile still in the registry after a 200 delete")
	}

	// The eviction half: a hand edit could never do this.
	read := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil)
	read.RemoteAddr = "100.64.0.20:4321"
	read.Header.Set("Authorization", "Bearer admin-secret")
	readRec := httptest.NewRecorder()
	handler.ServeHTTP(readRec, read)
	if body := readRec.Body.String(); strings.Contains(body, "seven-alpha") {
		t.Fatalf("deleted account still listed by /_subrouter/accounts: %s", body)
	}
}

func TestAccountDeleteRequiresMatchingConfirm(t *testing.T) {
	handler, _, store := deleteTestServer(t)

	for _, target := range []string{
		"/_subrouter/accounts?account=seven-alpha",
		"/_subrouter/accounts?account=seven-alpha&confirm=",
		"/_subrouter/accounts?account=seven-alpha&confirm=something-else",
	} {
		if rec := adminDelete(t, handler, target); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", target, rec.Code)
		}
	}
	if _, ok := store.FindProfile("seven-alpha"); !ok {
		t.Fatal("a refused delete still removed the profile")
	}
}

// The proxy token is held by every pooled caller and reaches this path on the
// public listener. It must never be able to destroy an account.
func TestPublicListenerCannotDeleteAccounts(t *testing.T) {
	_, public, store := deleteTestServer(t)

	req := httptest.NewRequest(http.MethodDelete,
		"/_subrouter/accounts?account=seven-alpha&confirm=seven-alpha", nil)
	req.RemoteAddr = "203.0.113.7:5555" // off-tailnet, like the public listener
	req.Header.Set("Authorization", "Bearer proxy-secret")
	rec := httptest.NewRecorder()
	public.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatal("the PROXY token deleted an account -- every pooled caller holds it")
	}
	if _, ok := store.FindProfile("seven-alpha"); !ok {
		t.Fatal("profile was removed via the public path")
	}
}

// The narrow key manages pool MEMBERSHIP: it can add (account-import) and now
// remove, but it still cannot enumerate accounts, and no route anywhere returns
// a credential. So a duplicate account is fixed by deleting the box's copy --
// there is no path that moves the secret back off the box.
func TestAccountImportTokenCanDeleteButNotEnumerate(t *testing.T) {
	admin, _, store := deleteTestServer(t)

	del := httptest.NewRequest(http.MethodDelete,
		"/_subrouter/accounts?account=seven-alpha&confirm=seven-alpha", nil)
	del.RemoteAddr = "100.64.0.20:4321"
	del.Header.Set("Authorization", "Bearer import-secret")
	delRec := httptest.NewRecorder()
	admin.ServeHTTP(delRec, del)
	if delRec.Code != http.StatusOK {
		t.Fatalf("import token delete = %d, want 200 (%s)", delRec.Code, delRec.Body.String())
	}
	if _, ok := store.FindProfile("seven-alpha"); ok {
		t.Fatal("profile survived a 200 delete")
	}

	// Reading the pool stays admin-only: the listing carries every account's
	// identity and spend.
	get := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil)
	get.RemoteAddr = "100.64.0.20:4321"
	get.Header.Set("Authorization", "Bearer import-secret")
	getRec := httptest.NewRecorder()
	admin.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusUnauthorized {
		t.Fatalf("import token enumerated accounts with status %d", getRec.Code)
	}
}
