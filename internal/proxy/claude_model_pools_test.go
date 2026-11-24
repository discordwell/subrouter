package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/selectacct"
)

// claudeUsageSweepClient answers the two Anthropic calls one usage sweep makes
// for a Claude account: the OAuth usage endpoint, then the Claude-Code-shaped
// probe whose unified rate-limit headers carry the 5h, 7d and Fable meters.
func claudeUsageSweepClient(t *testing.T, usageStatus int, usageBody string, probeHeader http.Header) *http.Client {
	t.Helper()
	return &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/oauth/usage":
			return &http.Response{StatusCode: usageStatus, Body: io.NopCloser(strings.NewReader(usageBody)), Header: http.Header{}}, nil
		case "/v1/messages":
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"type":"message"}`)), Header: probeHeader}, nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})}
}

func unifiedRateLimitHeader(fiveHour, sevenDay, fable float64, weekReset time.Time) http.Header {
	header := http.Header{}
	set := func(prefix string, utilization float64, reset time.Time) {
		header.Set("Anthropic-Ratelimit-Unified-"+prefix+"-Status", "allowed")
		header.Set("Anthropic-Ratelimit-Unified-"+prefix+"-Utilization", strconv.FormatFloat(utilization, 'f', -1, 64))
		header.Set("Anthropic-Ratelimit-Unified-"+prefix+"-Reset", strconv.FormatInt(reset.Unix(), 10))
	}
	set("5h", fiveHour, time.Now().Add(time.Hour))
	set("7d", sevenDay, weekReset)
	set("7d_oi", fable, weekReset)
	return header
}

// A one-year setup token is inference-only: the usage endpoint refuses it, so
// the probe headers (5h, 7d, Fable) are its only meters. Opus and Sonnet have
// no limit of their own, so it must still take Opus and Sonnet work on its
// account-wide budget -- beside OAuth siblings whether or not they publish an
// Opus bucket. Live on 2026-09-27, three setup-token accounts with the most
// headroom in the pool took 0 of 120 Opus requests.
func TestSetupTokenAccountTakesOpusAndSonnetBesideOAuthAccounts(t *testing.T) {
	weekReset := time.Now().Add(5 * 24 * time.Hour)
	hourReset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	weekResetText := weekReset.UTC().Format(time.RFC3339)
	oauthBase := fmt.Sprintf(`"five_hour":{"utilization":10,"resets_at":%q},"seven_day":{"utilization":70,"resets_at":%q}`, hourReset, weekResetText)

	for _, tc := range []struct {
		name       string
		oauthUsage string
		// reported: the OAuth sibling publishes Opus/Sonnet pools, so ForModel
		// narrows and the setup token must inherit rather than zero-fill.
		// Otherwise no account publishes one and ForModel stays account-wide.
		reported bool
	}{
		{
			name:       "oauth reports null opus and sonnet buckets",
			oauthUsage: `{` + oauthBase + `,"seven_day_opus":null,"seven_day_sonnet":null}`,
		},
		{
			name:       "oauth reports an opus bucket",
			oauthUsage: `{` + oauthBase + fmt.Sprintf(`,"seven_day_opus":{"utilization":40,"resets_at":%q},"seven_day_sonnet":{"utilization":40,"resets_at":%q}}`, weekResetText, weekResetText),
			reported:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oauthAccount := accounts.Account{ID: "oauth", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "oauth-token"}
			setupAccount := accounts.Account{ID: "setup", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "sk-ant-oat01-setup"}

			oauthWindows, err := fetchAccountUsageWindowsLive(context.Background(),
				claudeUsageSweepClient(t, http.StatusOK, tc.oauthUsage, unifiedRateLimitHeader(0.1, 0.7, 0.2, weekReset)),
				oauthAccount)
			if err != nil {
				t.Fatal(err)
			}
			setupWindows, err := fetchAccountUsageWindowsLive(context.Background(),
				claudeUsageSweepClient(t, http.StatusForbidden,
					`{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`,
					unifiedRateLimitHeader(0, 0, 0, weekReset)),
				setupAccount)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.reported {
				for _, window := range oauthWindows {
					if window.Name == "opus-weekly" || window.Name == "sonnet-weekly" {
						t.Fatalf("null bucket shows %s with no Opus/Sonnet limit behind it: %+v", window.Name, oauthWindows)
					}
				}
			}

			scheduler := selectacct.NewScheduler([]selectacct.Score{
				scoreFromUsageWindows(accounts.ProviderClaude, oauthAccount.ID, oauthWindows),
				scoreFromUsageWindows(accounts.ProviderClaude, setupAccount.ID, setupWindows),
			})
			for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5"} {
				if got := scheduler.HasModelPoolFor(accounts.ProviderClaude, claudePoolModel(model)); got != tc.reported {
					t.Fatalf("%s: HasModelPoolFor = %v, want %v", model, got, tc.reported)
				}
				pool := scheduler.ForModel(claudePoolModel(model))
				if pool.Exhausted(accounts.ProviderClaude, setupAccount.ID) {
					t.Fatalf("%s: setup-token account scored exhausted with its whole week unspent", model)
				}
				if !pool.UsableForNewSession(accounts.ProviderClaude, setupAccount.ID) {
					t.Fatalf("%s: setup-token account cannot start a session", model)
				}
				picked, err := pool.Pick([]accounts.Account{oauthAccount, setupAccount})
				if err != nil || picked.ID != setupAccount.ID {
					t.Fatalf("%s: picked %q err=%v, want the setup-token account (unspent week vs 70%% spent)", model, picked.ID, err)
				}
			}
		})
	}
}

// Opus and Sonnet have no limit of their own: Anthropic reports their buckets
// as null and they spend the general 7d budget. A null bucket must publish no
// window and no pool -- a 0%-used placeholder reads as a meter that does not
// exist -- while ForModel still routes the account on its account-wide budget.
// A bucket Anthropic does report is published as measured.
func TestClaudeUsageWindowsPublishOpusSonnetOnlyWhenReported(t *testing.T) {
	week := time.Now().Add(5 * 24 * time.Hour).Format(time.RFC3339)
	null := &agentclaude.UsageResponse{
		FiveHour:          &agentclaude.RateLimit{Utilization: floatPtr(10), ResetsAt: time.Now().Add(time.Hour).Format(time.RFC3339)},
		SevenDay:          &agentclaude.RateLimit{Utilization: floatPtr(20), ResetsAt: week},
		SevenDayOAuthApps: &agentclaude.RateLimit{Utilization: floatPtr(100), ResetsAt: week},
	}
	nullWindows := claudeUsageWindows(null)
	for _, window := range nullWindows {
		if window.Name == "opus-weekly" || window.Name == "sonnet-weekly" {
			t.Fatalf("null %s bucket was published as a window: %+v", window.Name, nullWindows)
		}
	}
	nullScore := scoreFromUsageWindows(accounts.ProviderClaude, "null", nullWindows)
	for _, feature := range []string{agentclaude.OpusFeature, agentclaude.SonnetFeature} {
		if _, ok := nullScore.ModelScores[selectacct.ModelKey(feature)]; ok {
			t.Fatalf("null bucket published a %s pool: %+v", feature, nullScore.ModelScores)
		}
	}

	reported := *null
	reported.SevenDayOpus = &agentclaude.RateLimit{Utilization: floatPtr(40), ResetsAt: week}
	reportedWindows := claudeUsageWindows(&reported)
	var opus *accounts.UsageWindow
	for i := range reportedWindows {
		if reportedWindows[i].Name == "opus-weekly" {
			opus = &reportedWindows[i]
		}
	}
	if opus == nil || opus.UsedPercent != 40 || opus.Feature != agentclaude.OpusFeature {
		t.Fatalf("reported Opus bucket = %+v, want published at 40%% under %s", opus, agentclaude.OpusFeature)
	}
	reportedScore := scoreFromUsageWindows(accounts.ProviderClaude, "reported", reportedWindows)

	opusPool := selectacct.NewScheduler([]selectacct.Score{nullScore, reportedScore}).ForModel(agentclaude.OpusFeature)
	// Base 7d at 20% caps both; the reported account's own 40%-spent Opus
	// bucket binds it lower. Fable's exhaustion must not leak into either.
	if got := opusPool.ScoreFor(accounts.ProviderClaude, "null").Headroom; got < 0.79 || got > 0.81 {
		t.Fatalf("null-bucket account Opus headroom = %.3f, want the account-wide ~0.8", got)
	}
	if got := opusPool.ScoreFor(accounts.ProviderClaude, "reported").Headroom; got < 0.59 || got > 0.61 {
		t.Fatalf("reported-bucket account Opus headroom = %.3f, want ~0.6 from its Opus bucket", got)
	}
}

// With no base snapshot publishing an Opus pool, a live Opus mark makes that
// pool "introduced" by the overlay, and a refresh strips carried-forward
// copies of it. A bucket Anthropic reports on a freshly measured account is
// not such a copy and must survive: an account whose real Opus bucket is spent
// stays out of Opus routing, and the marked account stays out until its mark
// expires.
func TestReportedOpusBucketSurvivesARefreshWhileAnOpusMarkIsLive(t *testing.T) {
	week := time.Now().Add(5 * 24 * time.Hour).Format(time.RFC3339)
	hour := time.Now().Add(time.Hour).Format(time.RFC3339)
	usage := func(opus *float64) *agentclaude.UsageResponse {
		response := &agentclaude.UsageResponse{
			FiveHour:          &agentclaude.RateLimit{Utilization: floatPtr(10), ResetsAt: hour},
			SevenDay:          &agentclaude.RateLimit{Utilization: floatPtr(30), ResetsAt: week},
			SevenDayOAuthApps: &agentclaude.RateLimit{Utilization: floatPtr(0), ResetsAt: week},
		}
		if opus != nil {
			response.SevenDayOpus = &agentclaude.RateLimit{Utilization: opus, ResetsAt: week}
		}
		return response
	}
	measured := func(id string, response *agentclaude.UsageResponse) selectacct.Score {
		score := scoreFromUsageWindows(accounts.ProviderClaude, id, claudeUsageWindows(response))
		score.Fresh = true
		return score
	}

	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		measured("marked", usage(nil)), measured("spent", usage(nil)),
	}))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "marked", agentclaude.OpusFeature, time.Now().Add(72*time.Hour))
	ref.Set(selectacct.NewScheduler([]selectacct.Score{
		measured("marked", usage(nil)), measured("spent", usage(floatPtr(100))),
	}))

	opus := ref.Get().ForModel(agentclaude.OpusFeature)
	if !opus.Exhausted(accounts.ProviderClaude, "spent") {
		t.Fatalf("an account whose reported Opus bucket is spent was routed Opus on its account budget: %+v", opus.ScoreFor(accounts.ProviderClaude, "spent"))
	}
	if !opus.Exhausted(accounts.ProviderClaude, "marked") {
		t.Fatal("the marked account left Opus routing before its mark expired")
	}
}
