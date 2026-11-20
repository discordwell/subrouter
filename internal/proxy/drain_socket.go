package proxy

import (
	"net/http"
)

// DrainSocketHandler is everything the drain socket serves: drain, resume and
// drain-status, at the paths the private listener uses, and a 404 for every
// other path -- no accounts, no usage, no proxy, no quiesce (PATCHES section
// 38.1). cmd/subrouter/drain_socket.go serves it on a mode-0600 Unix socket in
// the daemon's state directory, and on nothing else: it is never mounted on a
// TCP listener.
//
// It checks no credential, because the socket is the credential. Reaching it
// takes write permission on a mode-0600 socket owned by the daemon's user, in
// that user's private state directory: root, or the service user through
// `sudo -u`. The listener also refuses a peer whose kernel-reported uid is
// neither, where the kernel reports one. Neither gains anything here: root can
// already stop the service and read the admin token from its env file, and the
// service user is the daemon's own uid, which can already signal it and read
// the token from its environment.
//
// caller describes who is on the other end for the lifecycle log line; nil
// logs "drain socket".
//
// Resume here lifts a drain and never a quiesce, as a resume from off the
// machine does: a quiesce is set from loopback only, and so is its undo. It
// also refuses once the process has begun to shut down (BeginShutdown), as
// every resume does. This socket answers until the process exits, so that
// drain-status can report the shutdown's progress, and a resume must not make
// a leaving process look like it is taking new sessions.
func (s Server) DrainSocketHandler(caller func(*http.Request) string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/_subrouter/drain", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.Lifecycle != nil {
			s.Lifecycle.Drain()
		}
		s.logDrainSocketControl("drain", caller, r)
		writeJSON(w, s.lifecycleStatus(true))
	})
	mux.HandleFunc("/_subrouter/resume", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// One step under gateMu: refused while quiesced (403) or once
		// BeginShutdown has run (409). A check made before it could pass just
		// before SIGTERM and let the resume undo the shutdown's drain.
		if writeResumeRefusal(w, s.Lifecycle.ResumeDrain()) {
			return
		}
		s.logDrainSocketControl("resume", caller, r)
		writeJSON(w, s.lifecycleStatus(true))
	})
	mux.HandleFunc("/_subrouter/drain-status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, s.lifecycleStatus(false))
	})
	mux.HandleFunc("/", http.NotFound)
	return mux
}

// logDrainSocketControl is logLifecycleControl for the drain socket, whose
// callers have no address. The message is the same, so one search finds every
// drain and resume, whichever door it came through.
func (s Server) logDrainSocketControl(action string, caller func(*http.Request) string, r *http.Request) {
	if s.Logger == nil {
		return
	}
	who := "drain socket"
	if caller != nil {
		who = caller(r)
	}
	s.Logger.Info("subrouter lifecycle control", "action", action, "via", "drain socket", "caller", who)
}
