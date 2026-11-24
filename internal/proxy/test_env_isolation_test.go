package proxy

import (
	"os"
	"testing"

	"github.com/discordwell/subrouter/internal/accounts"
	agentclaude "github.com/discordwell/subrouter/internal/agents/claude"
	"github.com/discordwell/subrouter/internal/storepath"
	"github.com/discordwell/subrouter/internal/testenv"
)

// Some tests here set HOME and then write the active Codex auth, which an
// exported CODEX_HOME would take instead (testenv.HomeOverrides). Others set no
// HOME and build an AccountRef, which lists the Claude store under it. A
// developer shell with a host identity would stamp claims into every test's
// account files; tests that need one set it themselves (upstream #393's
// main_test.go, folded in).
func TestMain(m *testing.M) {
	os.Unsetenv(accounts.HostIDEnv)
	removeHome := testenv.IsolateHome()
	code := m.Run()
	removeHome()
	os.Exit(code)
}

func TestNoDevelopersHomeReachesATest(t *testing.T) {
	testenv.CheckHomeIsolated(t, func() (map[string]string, error) {
		return map[string]string{
			"storepath.StateDir()":            storepath.StateDir(),
			"accounts.DefaultCodexAuthPath()": accounts.DefaultCodexAuthPath(),
			"agentclaude.DefaultStore().Dir":  agentclaude.DefaultStore().Dir,
		}, nil
	})
}
