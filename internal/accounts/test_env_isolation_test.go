package accounts

import (
	"os"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/storepath"
	"github.com/manaflow-ai/subrouter/internal/testenv"
)

// Tests here set HOME and then write the active Codex auth, which an exported
// CODEX_HOME would take instead (testenv.HomeOverrides). A developer shell with
// a host identity would stamp claims into every test's account files; tests
// that need one set it themselves (upstream #393's main_test.go, folded in).
func TestMain(m *testing.M) {
	os.Unsetenv(HostIDEnv)
	removeHome := testenv.IsolateHome()
	code := m.Run()
	removeHome()
	os.Exit(code)
}

func TestNoDevelopersHomeReachesATest(t *testing.T) {
	testenv.CheckHomeIsolated(t, func() (map[string]string, error) {
		return map[string]string{
			"storepath.StateDir()":   storepath.StateDir(),
			"DefaultCodexAuthPath()": DefaultCodexAuthPath(),
		}, nil
	})
}
