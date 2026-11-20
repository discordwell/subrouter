package proxy

import "net/http"

// The peer role and admin over the public listener (PATCHES §43.1).
//
// §43 left two holes it named as an owner decision, and the owner took both
// (2026-09-29):
//
//   - The laptop's peer link had no named credential. It ranks the box by
//     reading usage-status over the public listener and forwards turns there,
//     one bearer for both; membership cannot read the roster and a named
//     admin token was refused there, so the link stayed on the static proxy
//     token. A PEER token is exactly what that link needs, on the listener it
//     uses: it spends the pool (targeted forwards included, so its account
//     selectors are kept) and reads usage-status on the PUBLIC listener. It is
//     never admin, it never reads /_subrouter/accounts, and privately it reads
//     nothing the static proxy token does not (usage-status there stays
//     requireAdmin).
//
//   - Admin over the public listener, behind an explicit opt-in
//     (Server.PublicAdmin, serve's --public-admin / SUBROUTER_PUBLIC_ADMIN=1).
//     Off -- the default -- nothing here changes what the public listener
//     serves. On, the routes marked publicAdmin in the route table are mounted
//     there too, and ONLY a named admin-role token opens them, through
//     publicAdminGranted: the public copy has already dropped the static admin
//     and import tokens, the tailnet authorizer and the loopback exemption
//     (publicSurfaceServer), the import gate asks publicAdminGranted and
//     nothing else there, and loopbackPeer keeps a relay on the box from
//     passing as a local caller. Every grant runs the browser checks and logs
//     the token's name, and the public rate limiter wraps these routes as it
//     wraps the rest of the listener. A named admin token is still never a
//     SPEND credential on the public listener (proxyCaller).
//
//	credential        listener           proxy   usage-status   admin route
//	static proxy      private            yes     no             no
//	static proxy      public             yes     yes            no (404, or 401 with --public-admin)
//	static admin      private            yes     yes            yes
//	static admin      public             no      no             no
//	named membership  either             yes     no (401 / 403) no
//	named peer        private            yes     no (401)       no
//	named peer        public             yes     yes            no
//	named admin       private            yes     yes            yes
//	named admin       public             no      no             no (404)
//	named admin       public + opt-in    no      yes            yes

// loopbackPeer reports a caller whose socket peer is loopback, on the PRIVATE
// listener. On the public one, position is no credential (publicSurfaceServer):
// a relay or a curl on the box itself presents 127.0.0.1 there, and must not
// pass the loopback-only controls or undo a quiesce because of it. Every gate
// that grants on loopback position alone asks this rather than
// isLoopbackRemote, so a route that --public-admin mounts, or one promoted to
// public later, cannot inherit the grant.
func (s Server) loopbackPeer(r *http.Request) bool {
	return !s.publicSurface && isLoopbackRemote(r.RemoteAddr)
}

// publicAdminGranted is every admin grant on the public listener -- authorizeAdmin
// and the import gate (requireAccountImportAuth) both ask it there and nothing
// else: nothing, unless serve opted in, and then an active named admin-role
// token and nothing else. The browser checks still run first (PATCHES §29): a
// cross-site page gets no further here than it does privately.
func (s Server) publicAdminGranted(r *http.Request) bool {
	if !s.publicSurface || !s.PublicAdmin {
		return false
	}
	if !adminBrowserSignalsAllowed(r) {
		return false
	}
	_, ok := s.namedAdminToken(r)
	return ok
}

// publicAdminOrPublic serves a route that is public for everyone and also an
// admin route (the roster): a named admin token gets the admin handler, which
// authorizes and logs it by name like any admin grant, and every other caller
// gets the public handler exactly as without --public-admin. That includes a
// named admin token presented only as the bearer beside some other
// X-Subrouter-Admin-Token: the public handler's gate (proxyCaller) refuses an
// admin-role bearer, so the admin token reads the roster through the admin
// handler, with its browser checks and its log, or not at all.
func (s Server) publicAdminOrPublic(admin, public http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.namedAdminToken(r); ok {
			admin.ServeHTTP(w, r)
			return
		}
		public.ServeHTTP(w, r)
	})
}
