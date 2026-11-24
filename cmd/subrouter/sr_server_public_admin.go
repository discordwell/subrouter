package main

// sr's public admin path (PATCHES §43.2).
//
// A servers.json entry is reached at its private URL (the box's tailnet
// address) for every admin request, exactly as before. When that address
// cannot be used from here -- Tailscale off, so the transport policy refuses
// plaintext; the dial refused or timed out; the name did not resolve; in no
// case once a TCP connection to it was made -- and
// the entry carries the public trio (publicUrl, fingerprint,
// publicAdminToken), the SAME request is sent once more to the box's public
// listener (:31416), which serves the routes a box running with
// --public-admin marks (proxy.PublicAdminPath) to a NAMED admin-role token
// and to nothing else. The one exception: a Claude upload or a Codex login's
// upload to an unpinned plaintext entry goes to the public path only
// (publicOnly, PATCHES §51).
//
// What this never does:
//   - fall back on an ANSWER: a 401, 403, 404 or 5xx from the private URL is
//     the box speaking, and is returned as it is today;
//   - fall back once a connection to the private URL was made (the request
//     may have gone out; a reset sent twice spends two credits);
//   - send anything but publicAdminToken to the public URL: not the static
//     adminToken (the box refuses it there by design, and it must never cross
//     the internet), not the import token, not a tenant key;
//   - send it anywhere the certificate pin does not match: the pin is checked
//     inside the TLS handshake, before a byte of the request is written;
//   - try the public path for a route the box never serves there (tenant
//     CRUD, transcripts): those say they need the tailnet. The Claude reset
//     claim is served there since §46.9 and falls back like any other.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discordwell/subrouter/internal/accesstoken"
	"github.com/discordwell/subrouter/internal/proxy"
)

// privateAdminDialBound caps the private URL's dial when a public admin path
// stands behind it. A tailnet that is up in `tailscale status` but blocked (a
// VPN kill switch) black-holes the SYN; unbounded, the dial would spend the
// whole budget of `sr status` (5 s) and leave the public path none. Without a
// public path the dial is not bounded here, as before.
const privateAdminDialBound = 2 * time.Second

// serverAdminVia is the address that answered an admin request.
type serverAdminVia int

const (
	viaPrivateURL serverAdminVia = iota
	viaPublicAdmin
)

// suffix is ", via public admin" when the public admin path answered.
func (v serverAdminVia) suffix() string {
	if v == viaPublicAdmin {
		return ", via public admin"
	}
	return ""
}

// serverAdminCall is one admin request to a servers.json entry.
type serverAdminCall struct {
	method string
	// path is the route with any query: /_subrouter/usage-status.
	path string
	body []byte
	// header carries the request's non-credential headers (Content-Type).
	header http.Header
	// timeout is the client timeout when the runner has no client of its own.
	timeout time.Duration
	// clientTimeout, when set, replaces every client's timeout (a Claude
	// claim waits minutes for its answer).
	clientTimeout time.Duration
	// protected builds the private base URL as protectedServerControlBaseURL.
	protected bool
	// privateAuth sets the PRIVATE request's credential; nil is
	// addServerAdminAuth. The public request carries publicAdminToken only.
	privateAuth func(*http.Request, srServerConfig)
	// quiet leaves saying which path answered to the caller, which prints it
	// in its own output; otherwise a public answer prints one note on errOut.
	quiet bool
	// publicOnly sends the request to the public admin path and never to the
	// private URL, not even first: a Claude upload or a Codex login's upload
	// to an unpinned plaintext entry (PATCHES §51). Without a usable public
	// admin path it is refused unsent. The caller says which path it took.
	publicOnly bool
}

// serverAdminError is an admin request that got no usable answer. Error() is
// redacted; nothingDone reports that the request never reached a handler
// that could act on it (never sent, or refused at the public gate), so a
// reset or claim certainly did not land.
type serverAdminError struct {
	cause       error
	message     string
	nothingDone bool
}

func (e *serverAdminError) Error() string { return e.message }

func (e *serverAdminError) Unwrap() error { return e.cause }

func (e *serverAdminError) requestDidNothing() bool { return e.nothingDone }

func newServerAdminError(server srServerConfig, cause error, nothingDone bool) *serverAdminError {
	return &serverAdminError{cause: cause, message: redactServerRequestError(cause, server).Error(), nothingDone: nothingDone}
}

// publicAdminTarget is an entry's public admin path, validated.
type publicAdminTarget struct {
	base        string
	fingerprint string
	token       string
}

// validNamedAccessToken reports the shape `subrouter token mint` gives a
// named access token: srk_ and 64 lowercase hex. The static tokens have no
// prefix, so this also keeps a pasted static admin token off the public wire.
func validNamedAccessToken(token string) bool {
	rest, ok := strings.CutPrefix(token, accesstoken.Prefix)
	if !ok || len(rest) != 64 {
		return false
	}
	for _, r := range rest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// validatePublicAdminURL returns the public listener's origin: https, a host,
// no credentials, query, fragment or path.
func validatePublicAdminURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return "", errors.New("publicUrl must be an absolute https URL, such as https://box.example:31416")
	}
	switch {
	case !strings.EqualFold(parsed.Scheme, "https"):
		return "", errors.New("publicUrl must use https: the public listener is TLS, pinned by fingerprint")
	case parsed.User != nil:
		return "", errors.New("publicUrl must not carry credentials")
	case parsed.RawQuery != "" || parsed.Fragment != "" || strings.Trim(parsed.Path, "/") != "":
		return "", errors.New("publicUrl must be the listener's origin (https://host:port), with no path or query")
	}
	return "https://" + parsed.Host, nil
}

// publicAdminFor reports the entry's public admin path. ok is false with a nil
// error when the entry has none (no publicAdminToken); an error names what is
// wrong with one that is set but unusable, and nothing is sent to it.
func publicAdminFor(server srServerConfig) (publicAdminTarget, bool, error) {
	token := strings.TrimSpace(server.PublicAdminToken)
	if server.requestClient != nil || token == "" {
		return publicAdminTarget{}, false, nil
	}
	// A tenant key in TenantKey or in the URL's path (/t/<key>) scopes every
	// private request to one tenant's pool; the public admin path has no
	// tenant scope, so it would answer for the box's ROOT pool instead.
	if tenantScopedServer(server) {
		return publicAdminTarget{}, false, errors.New("a tenant-scoped entry has no public admin path")
	}
	if !validNamedAccessToken(token) {
		return publicAdminTarget{}, false, errors.New("publicAdminToken is not a named access token (srk_...); the box takes only a named admin-role token on its public listener")
	}
	base, err := validatePublicAdminURL(server.PublicURL)
	if err != nil {
		return publicAdminTarget{}, false, err
	}
	fingerprint := strings.ToLower(strings.TrimSpace(server.Fingerprint))
	if !validCertificateFingerprint(fingerprint) {
		return publicAdminTarget{}, false, errors.New("the public listener's certificate pin (fingerprint) is missing or not 64 hex characters")
	}
	return publicAdminTarget{base: base, fingerprint: fingerprint, token: token}, true, nil
}

// tenantScopedServer reports an entry with a tenant key, in TenantKey or in
// its URL's path. publicAdminFor gives none of them a public admin path.
func tenantScopedServer(server srServerConfig) bool {
	return strings.TrimSpace(server.TenantKey) != "" || urlTenantKey(server.URL) != ""
}

// urlTenantKey is the tenant key an entry's URL carries in its path
// (http://box/t/<key>), or "".
func urlTenantKey(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return tenantKeyFromURL(parsed)
}

// samePublicEndpoint reports that two entries send a public admin token to
// the same listener under the same pin. A token minted on one box is never
// carried over to another endpoint (sr server set-public, sr server add).
func samePublicEndpoint(a, b srServerConfig) bool {
	return publicOrigin(a.PublicURL) == publicOrigin(b.PublicURL) &&
		strings.EqualFold(strings.TrimSpace(a.Fingerprint), strings.TrimSpace(b.Fingerprint))
}

func publicOrigin(raw string) string {
	if base, err := validatePublicAdminURL(raw); err == nil {
		return strings.ToLower(base)
	}
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(raw), "/"))
}

// hasPublicConfig reports an entry that names a public address at all, so an
// error on a private-only route can say that the public one would not help.
func hasPublicConfig(server srServerConfig) bool {
	return strings.TrimSpace(server.PublicURL) != "" || strings.TrimSpace(server.PublicAdminToken) != ""
}

// needsTailnet is the refusal for a route the box never serves on its public
// listener, when the private URL could not be used.
func needsTailnet(server srServerConfig, path string, cause *serverAdminError) *serverAdminError {
	route := path
	if i := strings.IndexAny(route, "?#"); i >= 0 {
		route = route[:i]
	}
	return &serverAdminError{
		cause: cause,
		message: fmt.Sprintf("%s; this needs the tailnet: %s is private and never served on %s's public admin listener, so its public URL cannot stand in",
			cause.message, route, server.Name),
		nothingDone: true,
	}
}

// privateOnlyUnreachable is the error for a request on a private-only route
// (tenant CRUD) whose private URL could not be used: it says the command
// needs the tailnet when the entry has a public address that cannot stand in.
func privateOnlyUnreachable(server srServerConfig, path string, err error) error {
	unreachable := newServerAdminError(server, err, true)
	if hasPublicConfig(server) {
		return needsTailnet(server, path, unreachable)
	}
	return unreachable
}

// doServerAdmin sends one admin request to a servers.json entry: to its
// private URL, and only when that URL cannot be used, to its public admin
// path. See the top of this file. A publicOnly call skips the private URL.
func (r srRunner) doServerAdmin(ctx context.Context, server srServerConfig, call serverAdminCall) (*http.Response, serverAdminVia, error) {
	target, publicOK, publicErr := publicAdminFor(server)
	routePublic := proxy.PublicAdminPath(call.path)
	if call.publicOnly {
		skipped := &serverAdminError{nothingDone: true, message: fmt.Sprintf(
			"%s's private URL is plain http with no pinned Tailscale node, so this request is not sent there", server.Name)}
		if !routePublic || !publicOK {
			if publicErr != nil {
				skipped.message += fmt.Sprintf(" (public admin path not used: %v)", publicErr)
			}
			return nil, viaPrivateURL, skipped
		}
		res, err := r.doPublicServerAdmin(ctx, server, target, call, skipped)
		if err != nil {
			return nil, viaPrivateURL, err
		}
		return res, viaPublicAdmin, nil
	}
	var private *serverAdminError
	if publicOK && routePublic {
		// This command already found the private URL out of reach: the
		// public path answers without paying the refusal or the bounded
		// dial again.
		private = r.adminReach.missed(server, call.protected, time.Now())
	}
	if private == nil {
		res, reached, dialed, err := r.doPrivateServerAdmin(ctx, server, call, publicOK && routePublic)
		if err == nil || reached {
			return res, viaPrivateURL, err
		}
		private = err.(*serverAdminError)
		if ctx.Err() == nil {
			// Not when the caller's own clock ran out: that says nothing
			// about the private URL.
			r.adminReach.remember(server, call.protected, dialed, private, time.Now())
		}
	}
	switch {
	case !routePublic:
		if hasPublicConfig(server) {
			return nil, viaPrivateURL, needsTailnet(server, call.path, private)
		}
		return nil, viaPrivateURL, private
	case publicErr != nil:
		return nil, viaPrivateURL, &serverAdminError{cause: private, nothingDone: true,
			message: fmt.Sprintf("%s (public admin path not used: %v)", private.message, publicErr)}
	case !publicOK:
		return nil, viaPrivateURL, private
	}
	res, err := r.doPublicServerAdmin(ctx, server, target, call, private)
	if err != nil {
		return nil, viaPrivateURL, err
	}
	r.notePublicAdmin(server, target, private, call.quiet)
	return res, viaPublicAdmin, nil
}

// doPrivateServerAdmin is the request as every admin command sent it before
// §43.2. reached reports that it got an answer or made a connection (a TCP
// connect, even one whose TLS then failed); only when it did not may the
// public path be tried. dialed reports that the transport policy let the
// request through and the dial itself failed.
func (r srRunner) doPrivateServerAdmin(ctx context.Context, server srServerConfig, call serverAdminCall, bounded bool) (res *http.Response, reached, dialed bool, err error) {
	baseURL, err := serverControlBaseURLFor(server, call.protected)
	if err != nil {
		// The transport policy refused (Tailscale off, a plaintext address it
		// cannot verify): nothing was sent.
		return nil, false, false, newServerAdminError(server, err, true)
	}
	endpoint := baseURL + call.path
	req, err := newServerAdminRequest(ctx, call, endpoint)
	if err != nil {
		return nil, true, false, newServerAdminError(server, err, true)
	}
	if call.privateAuth != nil {
		call.privateAuth(req, server)
	} else {
		addServerAdminAuth(req, server)
	}
	secured, err := r.securedRequestClientForServer(server, endpoint, call.timeout)
	if err != nil {
		return nil, false, false, newServerAdminError(server, err, true)
	}
	client := *secured
	if call.clientTimeout > 0 {
		client.Timeout = call.clientTimeout
	}
	if bounded {
		client = boundedDialClient(client, privateAdminDialBound)
	}
	res, trace, err := doTraced(&client, req)
	if err != nil {
		// A TLS failure after a TCP connect (an untrusted or impersonating
		// certificate on an https private URL) is the private path failing,
		// not the private URL being out of reach: it is reported, never
		// papered over by the public path.
		return nil, trace.connected, !trace.connected, newServerAdminError(server, err, !trace.mayHaveSent)
	}
	return res, true, false, nil
}

// doPublicServerAdmin sends the request to the entry's public admin path with
// its named admin token, over TLS pinned to its fingerprint. A 401 there (the
// token refused) and the listener's own 404 (public admin off, or a build
// without the route) are refusals, returned as errors.
func (r srRunner) doPublicServerAdmin(ctx context.Context, server srServerConfig, target publicAdminTarget, call serverAdminCall, private *serverAdminError) (*http.Response, error) {
	where := fmt.Sprintf("%s; public admin %s", private.message, target.base)
	endpoint := target.base + call.path
	req, err := newServerAdminRequest(ctx, call, endpoint)
	if err != nil {
		return nil, &serverAdminError{cause: err, message: where + ": " + redactServerRequestError(err, server).Error(), nothingDone: true}
	}
	// The named token and nothing else: never the static admin or import
	// token, which the box refuses on this listener and which must not cross
	// the internet.
	req.Header.Del("X-Subrouter-Admin-Token")
	req.Header.Del("X-Subrouter-Account-Import-Token")
	req.Header.Set("Authorization", "Bearer "+target.token)
	timeout := call.timeout
	if r.client != nil && r.client.Timeout > 0 {
		timeout = r.client.Timeout
	}
	if call.clientTimeout > 0 {
		timeout = call.clientTimeout
	}
	res, trace, err := doTraced(publicAdminHTTPClient(target.fingerprint, timeout), req)
	if err != nil {
		failed := " failed too: "
		if call.publicOnly {
			failed = " failed: " // the private URL was skipped, not tried
		}
		message := where + failed + redactServerRequestError(err, server).Error()
		if strings.Contains(err.Error(), "does not match the pin") {
			message += fmt.Sprintf(". Nothing was sent. Treat it as a possible impersonation, not a routine change: take the pin only from the box itself over the tailnet (`subrouter fingerprint` there), never from this endpoint, then `sr server set-public %s --fingerprint <pin> --keep-token`",
				server.Name)
		}
		return nil, &serverAdminError{cause: err, message: message, nothingDone: !trace.mayHaveSent}
	}
	switch res.StatusCode {
	case http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		res.Body.Close()
		return nil, &serverAdminError{nothingDone: true, cause: errors.New(res.Status), message: fmt.Sprintf(
			"%s refused publicAdminToken (401): public admin is off on the box (SUBROUTER_PUBLIC_ADMIN), or the token is revoked, expired or not an admin-role token (mint another on the box and pipe it in: `<print it> | sr server set-public %s --token-file -`)",
			where, server.Name)}
	case http.StatusNotFound:
		head, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		if strings.TrimSpace(string(head)) == "404 page not found" {
			res.Body.Close()
			route := call.path
			if i := strings.IndexAny(route, "?#"); i >= 0 {
				route = route[:i]
			}
			return nil, &serverAdminError{nothingDone: true, cause: errors.New(res.Status), message: fmt.Sprintf(
				"%s does not serve %s (404): public admin is off on the box (SUBROUTER_PUBLIC_ADMIN=1 turns it on), or its build predates the route",
				where, route)}
		}
		res.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(head), res.Body), Closer: res.Body}
	}
	return res, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

func newServerAdminRequest(ctx context.Context, call serverAdminCall, endpoint string) (*http.Request, error) {
	var body io.Reader
	if call.body != nil {
		body = bytes.NewReader(call.body)
	}
	req, err := http.NewRequestWithContext(ctx, call.method, endpoint, body)
	if err != nil {
		return nil, err
	}
	for key, values := range call.header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	return req, nil
}

// requestTrace is what one request's httptrace saw.
type requestTrace struct {
	// connected: a TCP connection to the host was made (a connect, or a TLS
	// handshake begun over one), whatever happened after it.
	connected bool
	// mayHaveSent: a connection was handed the request (GotConn), so it may
	// have been written. Without it the request was certainly not sent.
	mayHaveSent bool
}

// doTraced sends req and reports how far it got.
func doTraced(client *http.Client, req *http.Request) (*http.Response, requestTrace, error) {
	var connected, gotConn atomic.Bool
	trace := &httptrace.ClientTrace{
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				connected.Store(true)
			}
		},
		TLSHandshakeStart: func() { connected.Store(true) },
		GotConn:           func(httptrace.GotConnInfo) { gotConn.Store(true) },
	}
	res, err := client.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	sent := gotConn.Load() || res != nil
	return res, requestTrace{connected: connected.Load() || sent, mayHaveSent: sent}, err
}

// privateMissTTL is how long, within one sr command, a private URL found out
// of reach is not tried again for a route the public admin path answers.
const privateMissTTL = 30 * time.Second

// serverAdminReach remembers, for one sr command (srRunner.run sets it), the
// entries whose private URL could not be used. `sr server status` makes
// several admin requests and pushes each Claude web balance under one short
// clock; without it each would pay the refusal, or the bounded dial behind a
// kill switch, again, and every balance after the first would run out of
// time. Only a failure that never connected is remembered, and never one
// the caller's own clock caused. A nil *serverAdminReach remembers nothing.
type serverAdminReach struct {
	misses sync.Map // reachKey -> privateMiss
}

type privateMiss struct {
	err *serverAdminError
	at  time.Time
}

func reachKey(server srServerConfig, protected bool) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%t", server.Name, server.URL, server.TenantKey, protected)
}

// remember records a private URL that could not be used. A dial that failed
// failed for the protected and the plain request alike (one host and port);
// a transport-policy refusal is remembered for the policy that refused.
func (m *serverAdminReach) remember(server srServerConfig, protected, dialed bool, err *serverAdminError, now time.Time) {
	if m == nil {
		return
	}
	miss := privateMiss{err: err, at: now}
	m.misses.Store(reachKey(server, protected), miss)
	if dialed {
		m.misses.Store(reachKey(server, !protected), miss)
	}
}

// missed is the remembered failure of server's private URL, or nil.
func (m *serverAdminReach) missed(server srServerConfig, protected bool, now time.Time) *serverAdminError {
	if m == nil {
		return nil
	}
	value, ok := m.misses.Load(reachKey(server, protected))
	if !ok {
		return nil
	}
	miss := value.(privateMiss)
	if now.Sub(miss.at) > privateMissTTL {
		return nil
	}
	return miss.err
}

// boundedDialClient caps client's dial at bound. A transport it cannot bound
// is left as it is.
func boundedDialClient(client http.Client, bound time.Duration) http.Client {
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		return client
	}
	bounded := transport.Clone()
	dial := bounded.DialContext
	if dial == nil {
		dial = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	}
	bounded.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, bound)
		defer cancel()
		return dial(ctx, network, address)
	}
	client.Transport = bounded
	return client
}

// publicAdminHTTPClient reaches a box's public listener: pinned TLS
// (proxy.PinnedTLSConfig, the peer link's policy), no ambient proxy, no
// redirects, no connection kept.
func publicAdminHTTPClient(fingerprint string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:     proxy.PinnedTLSConfig(fingerprint),
			TLSHandshakeTimeout: 10 * time.Second,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// publicAdminNoted keeps the note to one line per server per output. Each key
// is an output's address, and its value is that output. Holding the output
// keeps its address from going to a later output, which would then find
// itself already noted and print nothing.
var publicAdminNoted sync.Map

// notePublicAdmin says, once per server, that an answer came through the
// public admin path and why the private URL was not used. A quiet call's
// caller says it in its own output (a heading), which counts as said.
func (r srRunner) notePublicAdmin(server srServerConfig, target publicAdminTarget, private *serverAdminError, quiet bool) {
	if r.errOut == nil {
		return
	}
	if _, seen := publicAdminNoted.LoadOrStore(fmt.Sprintf("%p\x00%s", r.errOut, server.Name), r.errOut); seen || quiet {
		return
	}
	fmt.Fprintf(r.errOut, "note: %s's private URL is out of reach (%s); answered via public admin (%s)\n",
		server.Name, private.message, target.base)
}

// notePublicOnlyUpload is the one line a credential upload sent over an
// unpinned plaintext entry's public admin path only prints once it landed
// (credentialUploadPublicOnly, PATCHES §51).
func (r srRunner) notePublicOnlyUpload(name string, server srServerConfig, target publicAdminTarget) {
	if r.errOut != nil {
		fmt.Fprintf(r.errOut, "note: sent %s to %s via public admin (%s): its private URL is plain http with no pinned Tailscale node\n",
			name, server.Name, target.base)
	}
}

// requestDidNothing reports an admin request that certainly did nothing on
// the box: never sent, or refused at the public listener's gate.
func requestDidNothing(err error) (bool, bool) {
	var admin interface{ requestDidNothing() bool }
	if errors.As(err, &admin) {
		return admin.requestDidNothing(), true
	}
	return false, false
}

// serverSetPublic is `sr server set-public <name>`: the entry's public admin
// path. The token is read from a file or a pipe, never from the command line
// (shell history, ps) or a terminal (the screen), is never printed, and lands
// in servers.json through the store's locked, atomic 0600 write. A stored
// token is bound to the endpoint and pin it was set with: pointing the entry
// at another needs a new token, or --keep-token for the same box.
func (r srRunner) serverSetPublic(store srServerStore, args []string) error {
	command := r.serverCommand()
	usage := fmt.Errorf("usage: %s set-public <name> [--url <https-url>] [--fingerprint <hex>] [--token-file <path|->] [--keep-token] [--clear]", command)
	flags := flag.NewFlagSet(command+" set-public", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	publicURL := flags.String("url", "", "the box's public listener, such as https://box.example:31416")
	fingerprint := flags.String("fingerprint", "", "SHA-256 pin of the public listener's certificate (64 hex), from `subrouter fingerprint` run on the box over the tailnet")
	tokenFile := flags.String("token-file", "", "file holding a named admin-role token (srk_...), or - to read it from a pipe")
	keepToken := flags.Bool("keep-token", false, "keep the stored token although --url or --fingerprint changes (the same box; its pin taken on the box itself)")
	clearToken := flags.Bool("clear", false, "remove the public admin token (the URL and pin stay)")
	name, err := parseFlagsOneName(flags, args, usage)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if isBuiltInRemoteName(name) {
		return fmt.Errorf("%s is a built-in remote and has no public admin path", name)
	}
	given := 0
	flags.Visit(func(*flag.Flag) { given++ })
	if given == 0 {
		return usage
	}
	if *clearToken && given > 1 {
		return errors.New("--clear takes no other flag")
	}
	if *keepToken && *tokenFile != "" {
		return errors.New("--keep-token keeps the stored token; --token-file replaces it: give one")
	}
	token := ""
	if !*clearToken && *tokenFile != "" {
		if token, err = r.readPublicAdminToken(*tokenFile, name); err != nil {
			return err
		}
	}
	var saved srServerConfig
	if err := store.update(func(file *srServerFile) error {
		for i := range file.Servers {
			if file.Servers[i].Name != name {
				continue
			}
			stored := file.Servers[i]
			next := stored
			if *clearToken {
				next.PublicAdminToken = ""
				file.Servers[i] = next
				saved = next
				return nil
			}
			if strings.TrimSpace(*publicURL) != "" {
				next.PublicURL = strings.TrimSpace(*publicURL)
			}
			if strings.TrimSpace(*fingerprint) != "" {
				next.Fingerprint = strings.ToLower(strings.TrimSpace(*fingerprint))
			}
			if token != "" {
				next.PublicAdminToken = token
			}
			if strings.TrimSpace(next.PublicAdminToken) == "" {
				return fmt.Errorf("%s has no public admin token yet: pass --token-file <path|-> with a named admin-role token minted on the box", name)
			}
			if !validCertificateFingerprint(next.Fingerprint) {
				return errors.New("--fingerprint must be 64 hex characters (SHA-256 of the public listener's certificate), as `subrouter fingerprint` prints it on the box")
			}
			base, err := validatePublicAdminURL(next.PublicURL)
			if err != nil {
				return fmt.Errorf("--url: %w", err)
			}
			next.PublicURL = base
			if token == "" && !*keepToken && !samePublicEndpoint(stored, next) {
				// The stored token was minted for the old endpoint. Whoever
				// holds the new pin would be sent it: a rebuilt box, a
				// reassigned address, or an impersonator whose pin was read
				// off the endpoint itself.
				return fmt.Errorf("%s's stored public admin token was set for %s (pin %s...), and this points it at %s (pin %s...): nothing was changed. "+
					"Pass --token-file with a token minted on that box, or --keep-token if it is the same box and you took the pin on the box itself over the tailnet (`subrouter fingerprint`), never from the endpoint",
					name, redactedServerURL(publicOrigin(stored.PublicURL)), pinPrefix(stored.Fingerprint), redactedServerURL(base), pinPrefix(next.Fingerprint))
			}
			if _, ok, err := publicAdminFor(next); err != nil || !ok {
				if err == nil {
					err = errors.New("the entry cannot use a public admin path")
				}
				return err
			}
			file.Servers[i] = next
			saved = next
			return nil
		}
		return fmt.Errorf("server %q not found; add it first with %s add", name, command)
	}); err != nil {
		return err
	}
	if *clearToken {
		fmt.Fprintf(r.out, "Public admin token removed for %s; it is reached at its private URL only.\n", name)
		return nil
	}
	fmt.Fprintf(r.out, "Public admin for %s: %s (pin %s...), named admin token saved in %s (not shown).\n",
		name, saved.PublicURL, pinPrefix(saved.Fingerprint), store.Path)
	if unpinnedPlaintextServer(saved) {
		fmt.Fprintf(r.out, "sr uses it when %s's private URL is out of reach, and for every Claude upload and Codex login upload to %s: its private URL is plain http with no pinned Tailscale node (PATCHES §51). The box must run with SUBROUTER_PUBLIC_ADMIN=1.\n", name, name)
	} else {
		fmt.Fprintf(r.out, "sr uses it only when %s's private URL is out of reach; the box must run with SUBROUTER_PUBLIC_ADMIN=1.\n", name)
	}
	return nil
}

// pinPrefix is the first 12 characters of a pin, for display.
func pinPrefix(pin string) string {
	pin = strings.ToLower(strings.TrimSpace(pin))
	if len(pin) > 12 {
		return pin[:12]
	}
	if pin == "" {
		return "none"
	}
	return pin
}

// readPublicAdminToken reads a named admin token from path, or from a pipe
// for "-". Neither the value nor the path is ever echoed, in errors included:
// a token passed as the path by mistake would otherwise print.
func (r srRunner) readPublicAdminToken(path, name string) (string, error) {
	var raw []byte
	var err error
	if path == "-" {
		if r.in == nil {
			return "", errors.New("--token-file -: no stdin")
		}
		if readerIsTerminal(r.in) {
			// A token pasted at a terminal shows on screen and in scrollback.
			return "", fmt.Errorf("--token-file - reads the token from a pipe, not a terminal, where it would show as you paste it: "+
				"`<command that prints it> | sr server set-public %s --token-file -` (on a Mac, `pbpaste | ...`), or --token-file <path>", name)
		}
		raw, err = io.ReadAll(io.LimitReader(r.in, 4096))
	} else {
		raw, err = os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) && looksLikeAToken(path) {
			return "", fmt.Errorf("--token-file takes a file path or -, and the value given is no file but looks like a token itself: it was not used, and is not shown. "+
				"If it is one, it is now in your shell history: revoke it on the box (`subrouter token revoke <name>`), mint another, and pipe it in (`... | sr server set-public %s --token-file -`)", name)
		}
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
	}
	if err != nil {
		return "", fmt.Errorf("read the --token-file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if !validNamedAccessToken(token) {
		return "", errors.New("the token read is not a named access token (srk_ and 64 hex): mint one on the box with `subrouter token mint <name> --role admin`; a static admin token is never sent to the public listener")
	}
	return token, nil
}

// looksLikeAToken reports a --token-file value naming no file that is more
// likely a credential than a path: a named token, or a long run of token
// characters with no path separator or extension.
func looksLikeAToken(value string) bool {
	value = strings.TrimSpace(value)
	if validNamedAccessToken(value) || strings.HasPrefix(value, accesstoken.Prefix) {
		return true
	}
	if len(value) < 32 || strings.ContainsAny(value, "/.~") {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
