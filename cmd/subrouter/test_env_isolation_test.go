package main

import (
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/storepath"
	"github.com/manaflow-ai/subrouter/internal/testenv"
)

// Most tests here set HOME and nothing else, then write through the state dir,
// the cloud config or the Codex home. TestMain clears the exported overrides
// for them (testenv.HomeOverrides) and gives the rest an empty HOME; this
// proves it does.
func TestNoDevelopersHomeReachesATest(t *testing.T) {
	testenv.CheckHomeIsolated(t, func() (map[string]string, error) {
		cloudConfig, err := broker.DefaultConfigPath()
		if err != nil {
			return nil, err
		}
		codexConfig, err := defaultCodexConfigPath()
		if err != nil {
			return nil, err
		}
		claudeConfig, err := defaultClaudeConfigDir()
		if err != nil {
			return nil, err
		}
		return map[string]string{
			"storepath.StateDir()":            storepath.StateDir(),
			"broker.DefaultConfigPath()":      cloudConfig,
			"accounts.DefaultCodexAuthPath()": accounts.DefaultCodexAuthPath(),
			"defaultCodexConfigPath()":        codexConfig,
			"defaultClaudeConfigDir()":        claudeConfig,
			"claudeUserConfigPath()":          claudeUserConfigPath(),
		}, nil
	})
}
