package main

import (
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// server.Shutdown waits for the handlers, not for work they handed off: the
// stale-score sweep (PATCHES section 44) refreshes OAuth tokens, and a process
// that exits between the provider's rotation and the save keeps a spent
// refresh token. shutdownOnSignal returns only once that work is done, or at
// the shutdown deadline.
func TestShutdownOnSignalWaitsForBackgroundWork(t *testing.T) {
	lifecycle := proxy.NewLifecycle()
	release := make(chan struct{})
	finished := make(chan struct{})
	if !lifecycle.StartBackground(func() {
		<-release
		close(finished)
	}) {
		t.Fatal("background work refused before any drain")
	}
	errCh := make(chan error, 1)
	errCh <- nil
	returned := make(chan error, 1)
	go func() {
		returned <- shutdownOnSignal(&http.Server{}, nil, lifecycle, syscall.SIGTERM, time.Minute, nil, errCh)
	}()
	select {
	case err := <-returned:
		close(release)
		t.Fatalf("shutdown returned (%v) while background work was still running", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown never returned after the background work finished")
	}
	select {
	case <-finished:
	default:
		t.Fatal("shutdown returned before the background work finished")
	}
}

// The wait shares the shutdown deadline: work that never finishes cannot hold
// the process past --shutdown-timeout.
func TestShutdownOnSignalStopsWaitingForBackgroundWorkAtItsDeadline(t *testing.T) {
	lifecycle := proxy.NewLifecycle()
	release := make(chan struct{})
	defer close(release)
	lifecycle.StartBackground(func() { <-release })
	errCh := make(chan error, 1)
	errCh <- nil
	returned := make(chan error, 1)
	go func() {
		returned <- shutdownOnSignal(&http.Server{}, nil, lifecycle, syscall.SIGTERM, 50*time.Millisecond, nil, errCh)
	}()
	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown waited for background work past its deadline")
	}
}
