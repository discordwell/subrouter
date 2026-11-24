package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/discordwell/subrouter/internal/standby"
)

// checkStandby is `subrouter standby --check` (PATCHES §79.5): the look a
// stand-in with this config takes at start, printed one fact per line for the
// install script, which refuses a stand-in that would not arm. The verdict is
// the run's own armingVerdict, but from the supervisor's answer now: a saved
// reading is not read, no lock is taken, no path is bound, no file is written
// and no box is dialled.
//
// Fixed since 2026-11-22: install-standby.sh matches these line names and
// words.
//
//	socket <path>
//	control_socket <path>
//	state_dir <path>
//	peers_file <word> <path> | peers_file not_named   (only with the gate open)
//	box <name> <token word>                           (each box, only with the gate open)
//	supervisor <claude_socket> waits_for_lock=<bool> | supervisor no_answer
//	verdict armed | verdict disarmed <reason>
func checkStandby(ctx context.Context, config standbyConfig, w io.Writer) error {
	config = config.withDefaults()
	s := &standIn{cfg: config, logger: config.logger, supervisor: unixSocketClient(config.ControlSocket, config.StatusTimeout)}
	lines := []string{"socket " + config.Socket, "control_socket " + config.ControlSocket, "state_dir " + config.StateDir}
	// As in a run, a machine without the key reads no peers file and no token.
	if config.RemoteUnlocked {
		if config.PeersFile == "" {
			lines = append(lines, "peers_file not_named")
		} else {
			set, err := standby.NewSet(standby.Config{
				PeersFile: config.PeersFile, Tailnet: config.Tailnet, StateDir: config.StateDir,
				Logger: slog.New(slog.DiscardHandler),
			})
			if err != nil {
				return err
			}
			s.set = set
			status := set.Status()
			lines = append(lines, fmt.Sprintf("peers_file %s %s", status.PeersFile, config.PeersFile))
			for _, box := range status.Boxes {
				lines = append(lines, fmt.Sprintf("box %s %s", box.Name, box.Token))
			}
		}
	}
	if reading, ok := s.askSupervisor(ctx); ok {
		s.last = &reading
		lines = append(lines, fmt.Sprintf("supervisor %s waits_for_lock=%t", reading.Word, reading.WaitsForLock))
	} else {
		lines = append(lines, "supervisor no_answer")
	}
	if ok, reason := s.armingVerdict(config.clock()); ok {
		lines = append(lines, "verdict armed")
	} else {
		lines = append(lines, "verdict disarmed "+reason)
	}
	_, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
	return err
}
