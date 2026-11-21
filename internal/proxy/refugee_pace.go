package proxy

import (
	"encoding/json"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func (s Server) localRefugeeCap(account, poolModel string) int {
	scheduler := s.selectionSchedulerFrom(s.scheduler(), poolModel, "")
	if _, ok := scheduler.LookupScore(accounts.ProviderClaude, account); !ok {
		return refugeeSlots
	}
	return scheduler.NewSessionVerdict(accounts.ProviderClaude, account, time.Now()).RefugeeCap.Or(refugeeSlots)
}

func (s Server) peerRefugeeCap(peer, account, poolModel string) int {
	row, ok := s.cachedPeerRow(peer, account)
	if !ok {
		return refugeeSlots
	}
	verdict := row.Normalized.NewSession
	if pool, ok := row.Normalized.Pools[selectacct.ModelKey(poolModel)]; ok && poolModel != "" {
		verdict = pool.NewSession
	}
	if verdict == nil {
		return refugeeSlots
	}
	return verdict.RefugeeCap.Or(refugeeSlots)
}

func (s Server) cachedPeerRow(peer, account string) (remotePoolAccountUsage, bool) {
	for _, pool := range s.peers() {
		if pool.name() != peer {
			continue
		}
		var rows []remotePoolAccountUsage
		if json.Unmarshal(pool.cachedUsage().Body, &rows) != nil {
			return remotePoolAccountUsage{}, false
		}
		for _, row := range rows {
			if row.Provider == string(accounts.ProviderClaude) && row.ID == account {
				return row, true
			}
		}
		return remotePoolAccountUsage{}, false
	}
	return remotePoolAccountUsage{}, false
}
