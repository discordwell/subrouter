package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/internal/proxy"
)

// publishClaudeInventoryChange runs one local Claude profile-registry change
// under upstream's account-disk publication (7dbc4b1 and follow-ups): the
// generation marker advances under the same cross-process lock a running
// worker reloads behind, so the daemon stops serving a moved or renamed
// profile at once rather than whenever it next reloads for another reason.
// Upstream wraps every mutation site it owns; this is for the fork-only one
// (rename). claudeRunner.mutateProfileInventory is the one wrapper for a
// mutation that needs no lock of its own. The move's removal holds the
// profile's refresh lock, which must come after the account transaction, so
// it takes the transaction itself (moveClaudeProfileTo).
func publishClaudeInventoryChange(ctx context.Context, store agentclaude.Store, mutate func() (bool, error)) error {
	_, err := claudeRunner{store: store}.mutateProfileInventory(ctx, mutate)
	return err
}

// verifyServerHoldsAccount reports whether a server has a WORKING copy of one
// account.
//
// Deliberately strict: the account must appear in the server's usage-status and
// read auth_valid. Registry presence is not enough -- /_subrouter/accounts keeps
// listing accounts the server can no longer meter, and on 2026-08-28 the box
// listed seven-alpha and ten-alpha in its registry while omitting both from
// usage-status for a full day. A registry check would have green-lit deleting
// the last working copy of each.
//
// The asymmetry decides the strictness: a false negative leaves a duplicate,
// which is annoying. A false positive destroys a credential.
//
// Only a Claude row counts. The move is Claude-only, and usage-status lists
// Codex records too: a legacy Codex record is keyed by email, and `sr claude
// addr` names a profile by its email, so a usable Codex row with the same ID
// must not vouch for a Claude copy the server cannot use.
func (r srRunner) verifyServerHoldsAccount(ctx context.Context, server srServerConfig, name string) error {
	statuses, ok, err := r.fetchServerUsageStatuses(ctx, server)
	if err != nil {
		return fmt.Errorf("read %s usage-status: %w", server.Name, err)
	}
	if !ok {
		return fmt.Errorf("%s did not return a usage-status", server.Name)
	}
	for _, status := range statuses {
		if status.ID != name || status.Provider != accounts.ProviderClaude {
			continue
		}
		if !status.AuthValid {
			return fmt.Errorf("%s holds %q but it is not usable (auth_valid=false%s)",
				server.Name, name, errorSuffix(status.Error))
		}
		return nil
	}
	return fmt.Errorf("%s does not report %q in usage-status", server.Name, name)
}

func errorSuffix(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return ""
	}
	return ": " + msg
}

// resolveServer picks the named server. A blank name is refused, not read as
// the default: `--server ""` is what an unset shell variable gives. A push or
// move with no server named goes through claudeUploadTarget instead.
func (r srRunner) resolveServer(name string) (srServerConfig, error) {
	// selectedRemoteServer is gated and this was not: on a keyless machine that
	// still had a servers.json, a remove aimed at a server sent an
	// authenticated DELETE to the box. A server does not exist there
	// (PATCHES 17), so it is refused as an unknown flag, naming nothing.
	if !remoteUnlocked() {
		return srServerConfig{}, errors.New("unknown flag: --server")
	}
	if strings.TrimSpace(name) == "" {
		return srServerConfig{}, errors.New("--server needs a server name")
	}
	// A named server heals as the default one does (selectedRemoteServer,
	// namedRemoteServer): an entry recorded with a Tailscale node ID is
	// re-resolved when its stored URL went stale. Without a node ID the heal
	// is a no-op, so nothing here probes Tailscale by default.
	store := defaultSRServerStore(r.store)
	server, ok, err := store.find(name)
	if err != nil {
		return srServerConfig{}, err
	}
	if !ok {
		return srServerConfig{}, fmt.Errorf("no server named %q (see `sr remote -v`)", name)
	}
	// Every caller is an admin command (a remove, a move's import, a reset),
	// whose requests can fall back to the public admin path, so a heal that
	// fails off the tailnet does not stop an entry that has one (§43.2).
	return r.healAdminServer(context.Background(), store, server)
}

// claudeUploadTarget is where `sr claude push` and `sr claude move` send a
// profile: the server named; else the selected one (SUBROUTER_SERVER or the
// default); else the only configured server, healed as a named one is. With
// several and no default it guesses nothing: before any credential is read it
// names each server in the command to rerun with it. A SUBROUTER_SERVER that
// names this machine is a choice too, so the only server does not override it.
// flags are the typed flags the rerun must keep: dropping `--copy` would turn
// a refused copy into a move.
//
// 2026-11-26, the owner: "I should be able to push to a target remote!"
func (r srRunner) claudeUploadTarget(named, verb, profile string, flags ...string) (srServerConfig, error) {
	if strings.TrimSpace(named) != "" {
		return r.resolveServer(named)
	}
	server, ok, err := r.selectedRemoteServer()
	if err != nil || ok {
		return server, err
	}
	servers, err := r.configuredRemoteServers()
	if err != nil {
		return srServerConfig{}, err
	}
	pinnedLocal := explicitLocalServerTarget()
	if len(servers) == 1 && !pinnedLocal {
		return r.resolveServer(servers[0].Name)
	}
	command := fmt.Sprintf("%s claude %s %s", r.programOrSubrouter(), verb, profile)
	if len(servers) == 0 {
		return srServerConfig{}, fmt.Errorf("no server is configured on this machine, so `%s` has nowhere to send it", command)
	}
	why := "no server is named and no default is set"
	if pinnedLocal {
		why = "no server is named, and SUBROUTER_SERVER or SUBROUTER_CODEX_SERVER selects this machine"
	}
	var refusal strings.Builder
	fmt.Fprintf(&refusal, "%s; nothing was sent. Name a server:", why)
	for _, server := range servers {
		refusal.WriteString("\n  " + strings.Join(append([]string{command, server.Name}, flags...), " "))
	}
	return srServerConfig{}, errors.New(refusal.String())
}

// moveClaudeProfile is `sr claude move <name> [<server>]`.
func (r srRunner) moveClaudeProfile(ctx context.Context, name, serverName string) error {
	server, err := r.claudeUploadTarget(serverName, "move", name)
	if err != nil {
		return err
	}
	return r.moveClaudeProfileTo(ctx, name, server)
}

// moveClaudeProfileTo MOVES a profile to a server the caller already resolved:
// upload, confirm the server can actually use it, then remove the local copy so
// exactly one holder of its credential remains. It is the only way a
// refresh-token profile leaves this machine (`sr claude move`, and `sr claude
// push` on a refresh-token profile).
//
// The confirmation is not ceremony. The uploader returning nil is the
// uploader's account of itself; deleting the only other copy of a credential on
// that basis is how you lose one. Every failure path below keeps the local
// profile and says so.
//
// The chain must not rotate between the read that feeds the upload and the
// removal. It used to: a refresh that finished in between was saved here and
// then removed, and one still in flight when the removal committed dropped its
// rotation as "no longer current". Either way the server was left holding a
// spent refresh token, and the move printed success. So the whole move runs
// under the profile's refresh lock (HoldProfileRefresh), which every refresh on
// this machine holds from its read to its write, and the removal goes ahead
// only while the profile still serves exactly what was uploaded
// (RemoveProfileIfCredentialContext). A credential replaced anyway -- Claude
// Code refreshing the keychain item itself, or a new login -- is kept here,
// and the server's copy is deleted (dropReplacedServerCopy).
//
// Lock order: the account-disk transaction, then the refresh lock. `sr` status
// refreshes in that order, so taking the transaction only at the removal, under
// the refresh lock, could deadlock the two. The removal is published under that
// transaction, so the local daemon drops the profile at once.
//
// This was `sr shove` (and before that `sr export`, which never dispatched).
// The 2026-09 grammar pass re-homed it provider-first.
func (r srRunner) moveClaudeProfileTo(ctx context.Context, name string, server srServerConfig) error {
	store := agentclaude.DefaultStore()
	if _, ok := store.FindProfile(name); !ok {
		return fmt.Errorf("no local Claude profile named %q", name)
	}
	// An unpinned plaintext entry hears the whole move over its public admin
	// path only, the check below that decides the local copy's fate included
	// (PATCHES §51). r is this call's copy.
	r.credentialUploadPublicOnly = unpinnedPlaintextServer(server)
	held := false
	err := proxy.WithAccountDiskMutationPublication(ctx, store.Dir, func(publish func() error) error {
		return store.HoldProfileRefresh(ctx, name, func() error {
			held = true
			return r.moveHeldClaudeProfile(ctx, store, name, server, publish)
		})
	})
	if errors.Is(err, agentclaude.ErrProfileCredentialReplaced) {
		return r.dropReplacedServerCopy(ctx, name, server, err)
	}
	if err != nil && !held {
		return fmt.Errorf("move %s: nothing was uploaded and the local copy is untouched: %w", name, err)
	}
	return err
}

// moveHeldClaudeProfile is the move itself, run while the account transaction
// and the profile's refresh lock are held.
func (r srRunner) moveHeldClaudeProfile(ctx context.Context, store agentclaude.Store, name string, server srServerConfig, publish func() error) error {
	// The RESOLVED server, not a second resolution inside the uploader. When the
	// upload re-resolved its own default, `--server box2` sent the credential to
	// box1 and then verified box2.
	uploaded, err := r.pushClaudeProfileTo(ctx, name, server, claudeUploadForMove)
	if err != nil {
		return fmt.Errorf("upload %s to %s (local copy KEPT): %w", name, server.Name, err)
	}
	if err := r.verifyServerHoldsAccount(ctx, server, name); err != nil {
		return fmt.Errorf("%s was uploaded but %s cannot use it -- local copy KEPT: %w",
			name, server.Name, err)
	}
	// The `removed` bool is load-bearing. RemoveProfile returns (false, nil) for
	// a name that is no longer in the registry, so throwing it away would print
	// "is now its only owner" in precisely the case where both copies were
	// still live.
	//
	// The removal runs on its own clock: the server already holds a verified
	// copy, so a Ctrl-C here must not leave two live holders of one refresh
	// token (PATCHES 18's rule that a caller's cancellation never aborts a
	// rotation).
	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeProfileReconcileTimeout)
	defer cancel()
	removed := false
	err = publish()
	if err == nil {
		removed, err = store.RemoveProfileIfCredentialContext(removeCtx, name, uploaded)
		// As publishAccountDiskMutation reads it: a registry rename that is
		// visible but not yet durable is a removal.
		if removed && errors.Is(err, agentclaude.ErrProfileRegistryWriteCommitted) {
			err = nil
		}
	}
	if errors.Is(err, agentclaude.ErrProfileCredentialReplaced) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%s is on %s but the local copy could not be removed, so it "+
			"now exists in BOTH places -- run `%s claude remove %s`: %w",
			name, server.Name, r.programOrSubrouter(), name, err)
	}
	if !removed {
		return fmt.Errorf("%s is on %s but no local profile named %q was there to remove; "+
			"if a copy still exists under another name it is now racing the server "+
			"for the same refresh token", name, server.Name, name)
	}
	fmt.Fprintf(r.out, "Moved %q to %s. %s is now its only owner.\n", name, server.Name, server.Name)
	return nil
}

// dropReplacedServerCopy finishes a move whose local credential was replaced
// after the upload read it. The local profile was kept: it holds the newer
// credential. The server's copy is what the upload sent, and it is deleted,
// because either its refresh token is spent (Claude Code rotated the chain
// here) or it is a second chain the move never meant to leave behind (a new
// login). A spent token left there would be presented at the server's next
// refresh. The delete runs on its own clock, for the reason the removal does.
func (r srRunner) dropReplacedServerCopy(ctx context.Context, name string, server srServerConfig, cause error) error {
	dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	kept := fmt.Sprintf("%s's local credential was replaced while it was being moved, so the local copy was KEPT", name)
	if err := r.deleteServerClaudeCopy(dropCtx, server, name); err != nil {
		return fmt.Errorf("%s, but %s still holds the copy it was sent, whose refresh token may be spent: "+
			"remove that Claude account from %s before it refreshes (not deleted: %v): %w",
			kept, server.Name, server.Name, err, cause)
	}
	return fmt.Errorf("%s, and the copy %s was sent was deleted from it; run the move again: %w",
		kept, server.Name, cause)
}

// deleteServerClaudeCopy deletes the server's Claude account named name, and
// only that. The server's DELETE /_subrouter/accounts also removes a stored
// Codex account its selector matches, and that match is loose (an exact key,
// "apikey:"+name, a display name, or a unique substring of an email), so the
// delete is refused when any non-Claude account the server lists names this
// one in its ID, email or label, or when the list cannot be read.
func (r srRunner) deleteServerClaudeCopy(ctx context.Context, server srServerConfig, name string) error {
	res, _, err := r.doServerAdmin(ctx, server, serverAdminCall{
		method:     http.MethodGet,
		path:       "/_subrouter/accounts",
		timeout:    15 * time.Second,
		quiet:      true,
		publicOnly: r.credentialUploadPublicOnly,
	})
	if err != nil {
		return fmt.Errorf("list %s's accounts before the delete: %w", server.Name, redactServerRequestError(err, server))
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		res.Body.Close()
		return fmt.Errorf("list %s's accounts before the delete: HTTP %d", server.Name, res.StatusCode)
	}
	var listed []remoteServerAccount
	err = json.NewDecoder(res.Body).Decode(&listed)
	res.Body.Close()
	if err != nil {
		return fmt.Errorf("list %s's accounts before the delete: %w", server.Name, err)
	}
	needle := strings.ToLower(name)
	for _, account := range listed {
		if account.Provider == accounts.ProviderClaude {
			continue
		}
		for _, field := range []string{account.ID, account.Email, account.Label} {
			if strings.Contains(strings.ToLower(field), needle) {
				return fmt.Errorf("%s's account delete would also match its %s account %q", server.Name, account.Provider, account.ID)
			}
		}
	}
	res, _, err = r.doServerAdmin(ctx, server, serverAdminCall{
		method:     http.MethodDelete,
		path:       fmt.Sprintf("/_subrouter/accounts?account=%s&confirm=%s", url.QueryEscape(name), url.QueryEscape(name)),
		timeout:    15 * time.Second,
		quiet:      true,
		publicOnly: r.credentialUploadPublicOnly,
	})
	if err != nil {
		return fmt.Errorf("delete %s on %s: %w", name, server.Name, redactServerRequestError(err, server))
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("delete %s on %s: HTTP %d", name, server.Name, res.StatusCode)
	}
	return nil
}

// renameClaudeProfile is `sr claude rename <old> <new>`: it re-labels a local
// profile and CHECKS every configured server.
//
// It does not rename on a server, because a server cannot: its admin surface
// is account-import, account-status, accounts (GET/DELETE) and the migration
// trio, with no rename route and no route that returns a credential. A
// server-only account therefore cannot be re-labelled from here at all.
//
// The two failure modes are treated asymmetrically on purpose. A server that
// already holds the NEW name is a refusal: two different accounts answering to
// one label across one pool is the confusion this verb exists to remove. A
// server that cannot be reached is a warning and the local rename proceeds --
// the tailnet drops often enough that blocking a local relabel on it would make
// the verb useless exactly when someone is cleaning up after an outage. A
// keyless machine lists no servers (PATCHES 17), so it renames locally and
// asks nobody.
func (r srRunner) renameClaudeProfile(ctx context.Context, oldName, newName string) error {
	store := agentclaude.DefaultStore()
	if _, ok := store.FindProfile(oldName); !ok {
		return fmt.Errorf("no local Claude profile named %q", oldName)
	}
	for _, server := range r.renameCheckTargets() {
		statuses, ok, err := r.fetchServerUsageStatuses(ctx, server)
		if err != nil || !ok {
			fmt.Fprintf(r.errOut, "Warning: could not read %s, so this rename was not "+
				"checked against it.\n", server.Name)
			continue
		}
		for _, status := range statuses {
			if status.ID == newName {
				return fmt.Errorf("%s already holds an account named %q; renaming this one "+
					"would put two different accounts behind one label", server.Name, newName)
			}
		}
		for _, status := range statuses {
			if status.ID == oldName {
				fmt.Fprintf(r.out, "Note: %s also holds %q and keeps that name -- a server "+
					"cannot rename it.\n", server.Name, oldName)
			}
		}
	}
	if err := publishClaudeInventoryChange(ctx, store, func() (bool, error) {
		if err := store.RenameProfile(oldName, newName); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Renamed %q to %q.\n", oldName, newName)
	return nil
}

// renameCheckTargets is every configured server. A locked machine has none,
// so the check is skipped without ever naming a remote.
func (r srRunner) renameCheckTargets() []srServerConfig {
	servers, err := r.configuredRemoteServers()
	if err != nil {
		return nil
	}
	return servers
}

// deleteRemoteAccount is `sr claude remove <name> --server <server>`: it removes
// one account from a server and leaves this machine's profile alone. Uses the account-import
// token when configured: that key can add and remove pool members but cannot
// enumerate them, and no route anywhere returns a credential.
func (r srRunner) deleteRemoteAccount(ctx context.Context, name, serverName string) error {
	server, err := r.resolveServer(serverName)
	if err != nil {
		return err
	}
	// Upstream's control-request transport policy (c96c072, f97faf2): the
	// base URL is verified (HTTPS, loopback, or a verified tailnet node) and the
	// client is pinned to it, the same way every upstream control request is
	// built. This request carries the admin or account-import token, and it
	// used to go out over whatever r.client was, unverified; errors carry the
	// request URL, so they are redacted before printing.
	//
	// When the private URL is out of reach, the same DELETE goes to the box's
	// public admin path, if the entry has one (PATCHES §43.2).
	res, via, err := r.doServerAdmin(ctx, server, serverAdminCall{
		method:  http.MethodDelete,
		path:    fmt.Sprintf("/_subrouter/accounts?account=%s&confirm=%s", url.QueryEscape(name), url.QueryEscape(name)),
		timeout: 15 * time.Second,
		quiet:   true,
	})
	if err != nil {
		return fmt.Errorf("delete %s on %s: %w", name, server.Name, redactServerRequestError(err, server))
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		where := server.Name
		if via == viaPublicAdmin {
			where += " (via public admin)"
		}
		fmt.Fprintf(r.out, "Removed %q from %s.\n", name, where)
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%s does not have an account named %q", server.Name, name)
	default:
		return fmt.Errorf("delete %s on %s: HTTP %d", name, server.Name, res.StatusCode)
	}
}

// takeServerFlag pulls `--server <name>` out of an argument list, matching how
// every `sr tenant` subcommand scopes itself to a remote. Returns the remaining
// args and the server name ("" when absent).
func takeServerFlag(args []string) ([]string, string) {
	rest := make([]string, 0, len(args))
	server := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--server" && i+1 < len(args) {
			server = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(args[i], "--server=") {
			server = strings.TrimPrefix(args[i], "--server=")
			continue
		}
		rest = append(rest, args[i])
	}
	return rest, server
}
