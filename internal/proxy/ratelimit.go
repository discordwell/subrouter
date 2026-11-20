package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Rate limiting for the PUBLIC listener.
//
// The tailnet substituted for this and there is nothing in the tree: no
// limiter, no per-IP or per-token counter, and subrouter never emits a 429 of
// its own. The only global control is replayablePostUploadLimiter, a size-4
// channel, which one caller can stall process-wide. On a public endpoint that
// absence is a direct cost-amplification path against pooled subscriptions.
//
// KEYED ON THE TOKEN FIRST. The bearer is the actual principal; the source IP is
// a fallback for callers that present none, and it is shared by everyone behind
// one NAT. Keying primarily on IP would let one busy laptop exhaust a bucket
// that another user also sits behind. A request presenting more than one
// credential is charged to each (ratePrincipals).
//
// DEFAULTS ARE DELIBERATELY GENEROUS, and the default MODE is observe. A
// limiter tuned too tight throttles our own fleet: a single fan-out session was
// measured at ~1000 requests in 20 minutes (~0.8/s sustained, burstier in
// peaks), and every one of those is real work. The same reasoning MachinePolicy
// documents applies -- arming enforcement before reading real traffic is how a
// guess becomes an outage -- so observe logs the decision it WOULD have made and
// refuses nothing.
const (
	// publicRatePerSecond is ~25x the measured sustained rate of a heavy
	// fan-out session. It is meant to stop a scanner or a runaway loop, not to
	// shape legitimate use.
	publicRatePerSecond = 20.0
	// publicRateBurst absorbs the start of a fan-out, where many subagent
	// requests arrive at once under one session.
	publicRateBurst = 200.0
	// publicRateIdleEviction drops a principal's bucket after this long with no
	// traffic, so the map cannot grow without bound on a public endpoint where
	// the source IP set is unbounded.
	publicRateIdleEviction = 10 * time.Minute
)

// RateLimitMode mirrors MachinePolicyMode: the same observe-then-enforce
// discipline, for the same reason.
type RateLimitMode string

const (
	// RateLimitOff disables the limiter entirely.
	RateLimitOff RateLimitMode = "off"
	// RateLimitObserve logs what it would have refused and refuses nothing.
	// The default.
	RateLimitObserve RateLimitMode = "observe"
	// RateLimitEnforce returns 429.
	RateLimitEnforce RateLimitMode = "enforce"
)

type rateBucket struct {
	tokens   float64
	lastFill time.Time
	lastSeen time.Time
}

type rateLimiter struct {
	mode    RateLimitMode
	rate    float64
	burst   float64
	logger  *slog.Logger
	now     func() time.Time
	mu      sync.Mutex
	buckets map[string]*rateBucket
}

func newRateLimiter(mode RateLimitMode, logger *slog.Logger) *rateLimiter {
	// Unset means observe, said explicitly rather than left to fall through the
	// enforce check below. An unrecognised value lands here too: a typo in the
	// mode must not silently arm or disable enforcement.
	if mode != RateLimitOff && mode != RateLimitEnforce {
		mode = RateLimitObserve
	}
	return &rateLimiter{
		mode:    mode,
		rate:    publicRatePerSecond,
		burst:   publicRateBurst,
		logger:  logger,
		now:     time.Now,
		buckets: map[string]*rateBucket{},
	}
}

// principal identifies who to charge, and never returns the raw bearer: the
// first of ratePrincipals.
func ratePrincipal(r *http.Request) (key, kind string) {
	principals := ratePrincipals(r)
	return principals[0].key, principals[0].kind
}

type ratePrincipalKey struct{ key, kind string }

// ratePrincipals is every principal a request is charged to: each distinct
// credential it presents, or its source address when it presents none.
//
// Every credential, not just the bearer, because the limiter runs before any
// gate and cannot know which one the gate will honour. The admin gates read
// X-Subrouter-Admin-Token FIRST (presentedCredential), so keyed on the bearer
// alone, a request carrying a real admin token in that header and a fresh junk
// bearer every time would get a fresh bucket every time (PATCHES §43.1, where
// --public-admin mounts admin routes here). Charged to both, the junk buckets
// stay full and the real token's bucket empties. The same token in both places
// is one principal.
//
// A token is hashed before it becomes a map key so a heap dump or a log line
// cannot leak a live credential; a prefix of that hash is what gets logged.
func ratePrincipals(r *http.Request) []ratePrincipalKey {
	var principals []ratePrincipalKey
	add := func(token string) {
		if token == "" {
			return
		}
		sum := sha256.Sum256([]byte(token))
		key := "t:" + hex.EncodeToString(sum[:8])
		for _, seen := range principals {
			if seen.key == key {
				return
			}
		}
		principals = append(principals, ratePrincipalKey{key: key, kind: "token"})
	}
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		add(strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
	}
	add(strings.TrimSpace(r.Header.Get(adminTokenHeader)))
	if len(principals) == 0 {
		principals = append(principals, ratePrincipalKey{key: "a:" + clientRemoteIP(r), kind: "addr"})
	}
	return principals
}

// allow charges one request against the principal's bucket.
func (l *rateLimiter) allow(key string) bool {
	_, ok := l.allowAll([]string{key})
	return ok
}

// allowAll charges one request against every one of its principals' buckets,
// and only if each has a token: a request is as limited as the most limited
// principal it presents. It returns the index of a principal that refused it.
func (l *rateLimiter) allowAll(keys []string) (int, bool) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	buckets := make([]*rateBucket, len(keys))
	for i, key := range keys {
		bucket, ok := l.buckets[key]
		if !ok {
			bucket = &rateBucket{tokens: l.burst, lastFill: now}
			l.buckets[key] = bucket
		} else {
			elapsed := now.Sub(bucket.lastFill).Seconds()
			if elapsed > 0 {
				bucket.tokens += elapsed * l.rate
				if bucket.tokens > l.burst {
					bucket.tokens = l.burst
				}
				bucket.lastFill = now
			}
		}
		bucket.lastSeen = now
		buckets[i] = bucket
	}
	// Seen just now, so none of this request's buckets is idle enough to go.
	l.evictIdleLocked(now)

	for i, bucket := range buckets {
		if bucket.tokens < 1 {
			return i, false
		}
	}
	for _, bucket := range buckets {
		bucket.tokens--
	}
	return -1, true
}

// evictIdleLocked keeps the map bounded. Called under the lock on every request,
// which is cheap while the map is small and is the only thing that sweeps it --
// there is no background goroutine to leak.
func (l *rateLimiter) evictIdleLocked(now time.Time) {
	if len(l.buckets) < 64 {
		return
	}
	for key, bucket := range l.buckets {
		if now.Sub(bucket.lastSeen) > publicRateIdleEviction {
			delete(l.buckets, key)
		}
	}
}

// middleware wraps a handler with the limiter.
func (l *rateLimiter) middleware(next http.Handler) http.Handler {
	if l == nil || l.mode == RateLimitOff {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principals := ratePrincipals(r)
		keys := make([]string, len(principals))
		for i, principal := range principals {
			keys[i] = principal.key
		}
		refused, ok := l.allowAll(keys)
		if ok {
			next.ServeHTTP(w, r)
			return
		}
		key, kind := principals[refused].key, principals[refused].kind
		if l.logger != nil {
			// Logged in BOTH modes: observe exists to produce this line before
			// anyone arms enforcement on a guess.
			l.logger.Warn("public rate limit exceeded",
				"principal", key, "principal_kind", kind, "path", r.URL.Path,
				"mode", string(l.mode), "enforced", l.mode == RateLimitEnforce)
		}
		if l.mode != RateLimitEnforce {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
	})
}
