package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/discordwell/subrouter/internal/accesstoken"
)

// Named access tokens (PATCHES §43).
//
// The static SUBROUTER_PROXY_TOKEN is one secret shared by every caller: no
// identity, no expiry, and the only way to cut one holder off is to rotate it
// for everyone. On the public listener it also reads /_subrouter/accounts and
// /_subrouter/usage-status -- every pooled account's email and spend. Named
// tokens give each caller its own secret, a role fixed at mint, an expiry and
// a revoked flag, and put the caller's NAME on every request it makes.
//
// They are strictly additive. Every existing grant is untouched: the static
// proxy and admin tokens work exactly as before, loopback is still admin on
// the private listener, and a daemon enforcing no proxy token still enforces
// none. A named token only adds a way in, and a revoked or expired one opens
// nothing it did not already open without it.
//
//	credential          private proxy   private admin   public proxy   public roster
//	static proxy        yes             no              yes            yes
//	static admin        yes             yes             no             no
//	named membership    yes             no              yes            no (403)
//	named peer          yes             no              yes            usage-status only
//	named admin         yes             yes             no             no (yes with --public-admin)
//
// A named admin token is the static admin token with a name: admin on the
// private listener and no credential at all on the public one, where the more
// powerful secret must never travel (publicSurfaceServer, the box deploy) --
// unless serve runs with --public-admin, where it is the ONLY admin credential
// the public listener takes, and still never a spend credential there. The
// peer role (§43.1) is what the laptop's peer link needs: spend, and read the
// usage-status it ranks the box on (§31) on the public listener. Both are in
// public_admin.go.
//
// A membership caller also never learns a pooled account's name on the proxy
// path: the selectors it could pin or probe an account with are dropped from
// its request (dropAccountSelectors), and the headers naming the account or
// peer that served it are dropped from its response (hideServingAccount).

// requestCaller names the credential that let a request through the proxy
// gate. The label is the token's name for a named token, and otherwise one of
// the fixed labels below, which a token name can never equal
// (accesstoken.ValidateName reserves them).
type requestCaller struct {
	label string
	// role is set only for a named token.
	role accesstoken.Role
}

const (
	callerStaticProxy = "static:proxy"
	callerStaticAdmin = "static:admin"
	callerLoopback    = "loopback"
	callerOpen        = "open"
)

type requestCallerKey struct{}

// withRequestCaller records who authorized r, for the proxy request log line
// and for hideServingAccount.
func withRequestCaller(r *http.Request, caller requestCaller) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestCallerKey{}, caller))
}

// requestCallerLabel is the logged caller: a token name, or static:proxy,
// static:admin, loopback or open. Empty for a request the gate never saw.
func requestCallerLabel(r *http.Request) string {
	if r == nil {
		return ""
	}
	caller, _ := r.Context().Value(requestCallerKey{}).(requestCaller)
	return caller.label
}

// presentedCredential reads a credential the way every admin-family gate
// always has: the dedicated header first, then a bearer. The static and the
// named admin check both read through it, so they can never see different
// credentials in one request.
func presentedCredential(r *http.Request, dedicatedHeader string) string {
	if got := strings.TrimSpace(r.Header.Get(dedicatedHeader)); got != "" {
		return got
	}
	return bearerToken(r.Header.Get("Authorization"))
}

// namedToken classifies a presented credential against the named-token store.
// It never logs: whether a refusal is worth a line depends on whether the
// request was refused at all, which only the gate knows
// (logNamedTokenRefusal).
func (s Server) namedToken(presented string) accesstoken.Result {
	if s.AccessTokens == nil {
		return accesstoken.Result{Verdict: accesstoken.VerdictUnknown}
	}
	return s.AccessTokens.Check(presented)
}

// namedAdminToken is an active admin-role named token, on the private listener,
// and on the public one only with --public-admin (PATCHES §43.1). Without it,
// no admin token -- static or named -- is a credential on the public listener
// at all.
func (s Server) namedAdminToken(r *http.Request) (accesstoken.Token, bool) {
	if (s.publicSurface && !s.PublicAdmin) || s.AccessTokens == nil {
		return accesstoken.Token{}, false
	}
	result := s.namedToken(presentedCredential(r, adminTokenHeader))
	if result.Verdict != accesstoken.VerdictValid || result.Token.Role != accesstoken.RoleAdmin {
		return accesstoken.Token{}, false
	}
	return result.Token, true
}

// logNamedTokenRefusal names the holder of a real named token that a gate has
// just REFUSED: revoked, expired, or an admin token on the public listener.
// Gates call it only on their refusal path, so a request some other grant let
// through (an open gate, loopback) never logs a refusal. Never the secret; at
// most once a minute per name, so an operator can see who is still knocking
// after being cut off without the log filling with it.
func (s Server) logNamedTokenRefusal(result accesstoken.Result) {
	reason := string(result.Verdict)
	switch {
	case result.Verdict == accesstoken.VerdictRevoked, result.Verdict == accesstoken.VerdictExpired:
	case result.Verdict == accesstoken.VerdictValid && s.publicSurface && !s.PublicAdmin &&
		result.Token.Role == accesstoken.RoleAdmin:
		reason = "admin_token_on_public_listener"
	default:
		return
	}
	s.logTokenRefused(result.Token, reason)
}

// logTokenRefused is logNamedTokenRefusal's line, for a gate that knows the
// reason itself.
func (s Server) logTokenRefused(token accesstoken.Token, reason string) {
	if s.Logger == nil {
		return
	}
	if s.AccessTokens.ShouldLogDenial(token.Name) {
		s.Logger.Warn("access token refused", "caller", token.Name,
			"role", string(token.Role), "reason", reason,
			"expires_at", token.ExpiresAt)
	}
}

// staticCredentialUseInterval is how often a request made with a STATIC token
// outside the proxy path is logged. The proxy path logs every request with
// caller=; the roster reads and admin requests the static tokens still make
// (the peer link, srdash, sr) would otherwise never show, and "caller=static:*
// has stopped appearing" is how an operator knows a static token can go.
const staticCredentialUseInterval = time.Hour

// logAdminCredentialUse records an admin request by the credential it
// presented: every request made with a named token, by name, and one an hour
// made with the static admin token.
func (s Server) logAdminCredentialUse(r *http.Request) {
	if s.Logger == nil {
		return
	}
	if token, ok := s.namedAdminToken(r); ok {
		attrs := []any{"caller", token.Name, "method", r.Method, "path", r.URL.Path, "remote_addr", clientRemoteIP(r)}
		if s.publicSurface {
			// Only --public-admin gets a named admin token this far on the
			// public listener (PATCHES §43.1). Every such call is logged, and
			// says where it came in.
			attrs = append(attrs, "listener", "public")
		}
		s.Logger.Info("admin request by access token", attrs...)
		return
	}
	if matchesConfiguredBearerToken(r, s.AdminToken, adminTokenHeader) &&
		s.AccessTokens.ShouldLog("admin-use:"+callerStaticAdmin, staticCredentialUseInterval) {
		s.Logger.Info("admin request by static token", "caller", callerStaticAdmin,
			"method", r.Method, "path", r.URL.Path, "remote_addr", clientRemoteIP(r),
			"sampled", "hourly")
	}
}

// proxyCaller is the proxy gate: whether r may spend the pool, and on whose
// credential. localProxyAuthorized is its boolean.
//
// The static checks come first and are exactly the old ones, so the answer for
// every request that presents no named token is unchanged. A named token is
// identified BEFORE the "no proxy token configured" branch only so that the
// log names it; it cannot close that branch -- a daemon that enforces nothing
// keeps enforcing nothing.
func (s Server) proxyCaller(r *http.Request) (requestCaller, bool) {
	token := strings.TrimSpace(s.LocalProxyToken)
	named := accesstoken.Result{Verdict: accesstoken.VerdictUnknown}
	if got := bearerToken(r.Header.Get("Authorization")); got != "" {
		if bearerMatches(got, token) {
			return requestCaller{label: callerStaticProxy}, true
		}
		// The admin token also opens the proxy, because admin is strictly the
		// greater privilege: a credential that can drain the pool and
		// enumerate its accounts can obviously spend it, and refusing it here
		// buys nothing.
		//
		// Without this the two tokens cannot differ in practice. A peer daemon
		// sends ONE bearer for both surfaces -- it reads peer capacity from
		// /_subrouter/usage-status, which is requireAdmin, and forwards
		// traffic through the proxy gate -- so splitting them makes the peer
		// fail one or the other: either forwarding 401s, or the scheduler goes
		// blind to the peer and silently stops spilling.
		//
		// Making admin a superset is what allows a PROXY-ONLY token to exist,
		// which is the point: someone who should spend the pool but never
		// enumerate or drain it gets a credential that does exactly that.
		if bearerMatches(got, strings.TrimSpace(s.AdminToken)) {
			return requestCaller{label: callerStaticAdmin}, true
		}
		named = s.namedToken(got)
		// An admin-role token is refused on the public listener as the static
		// admin token is (publicSurfaceServer blanks that one): the secret
		// that is admin privately never becomes usable across the public wire.
		// --public-admin does not change that here (PATCHES §43.1): it opens
		// the ADMIN routes to a named admin token, through publicAdminGranted
		// with its browser checks and its per-call log. This gate also admits
		// the public roster handlers, and spend is what a peer or membership
		// token is for, so an admin token in a client's config still 401s on
		// the proxy rather than working quietly with the admin-capable secret.
		if named.Verdict == accesstoken.VerdictValid &&
			!(s.publicSurface && named.Token.Role == accesstoken.RoleAdmin) {
			return requestCaller{label: named.Token.Name, role: named.Token.Role}, true
		}
	}
	if token == "" {
		return requestCaller{label: callerOpen}, true
	}
	// Loopback exemption, opt-in only. Checked against the socket peer, which
	// cannot be forged -- deliberately not X-Forwarded-For, for the same reason
	// clientRemoteIP ignores it: a tailnet caller could set that header and
	// claim to be local.
	if s.AllowLoopbackProxyWithoutToken && isLoopbackRemote(r.RemoteAddr) {
		return requestCaller{label: callerLoopback}, true
	}
	// Every caller of this gate answers false with a 401.
	if named.Verdict == accesstoken.VerdictValid && s.publicSurface && s.PublicAdmin &&
		named.Token.Role == accesstoken.RoleAdmin {
		s.logTokenRefused(named.Token, "admin_token_spends_privately_only")
	} else {
		s.logNamedTokenRefusal(named)
	}
	return requestCaller{}, false
}

// accountSelectorHeaders are the request headers that pin a turn to a named
// pooled account, or prefer one: the forced selector, the peer's hard target
// (§31) and the soft preference. Pinning answers "does this account exist?"
// -- a 409 unknown_account for a guess, a 200 for a member -- and chooses whose
// subscription pays, so none of them is a membership caller's to send.
var accountSelectorHeaders = []string{
	"X-Subrouter-Account-ID",
	"X-Subrouter-Account",
	remotePoolTargetAccountHeader,
	"X-Subrouter-Preferred-Account-ID",
}

// dropAccountSelectors removes every account selector from a membership
// caller's request, so it gets the pool's own pick. Dropped rather than
// refused: `sr` launchers send a preference by default, and the answer to
// "which account?" is still "whichever the pool chooses".
func dropAccountSelectors(header http.Header) {
	for _, name := range accountSelectorHeaders {
		header.Del(name)
	}
}

// servingAccountHidden reports whether r's caller must not learn which pooled
// account or peer served it: a membership token spends the pool and nothing
// else (PATCHES §43). A Codex account's ID is its email, and a caller that
// varied its session could otherwise collect the roster one response at a
// time.
func servingAccountHidden(r *http.Request) bool {
	if r == nil {
		return false
	}
	caller, _ := r.Context().Value(requestCallerKey{}).(requestCaller)
	return caller.role == accesstoken.RoleMembership
}

// hideServingAccount drops, for a membership caller, the response headers that
// name the pooled account or the peer that served the request. Every site that
// stamps them calls it after stamping: HTTP and websocket, served here or
// relayed from a peer. A peer daemon never presents a membership token (it
// needs the roster, which membership cannot read): it presents a peer token
// (PATCHES §43.1) or a static one, and keeps both its selectors and these
// headers, so the forwarding side's own use of them is unaffected.
func hideServingAccount(r *http.Request, header http.Header) {
	if servingAccountHidden(r) {
		header.Del(subrouterAccountHeader)
		header.Del(subrouterPeerHeader)
	}
}
