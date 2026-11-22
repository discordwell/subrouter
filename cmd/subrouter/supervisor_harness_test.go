package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

type testSupervisor struct {
	*supervisor
	signals chan os.Signal
	exited  chan struct{}
	err     error
}

func startTestSupervisor(t *testing.T, config supervisorConfig) *testSupervisor {
	t.Helper()
	return startTestSupervisorWith(t, config, nil)
}

// startTestSupervisorWith lets a test change the supervisor before it runs,
// for what the config does not carry, such as its clock.
func startTestSupervisorWith(t *testing.T, config supervisorConfig, prepare func(*supervisor)) *testSupervisor {
	t.Helper()
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER", "1")
	initial, err := startWorkerGeneration(config, generationInitial)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newSupervisor(config, initial)
	if err != nil {
		terminateWorker(initial, time.Second)
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(s)
	}
	go s.monitorWorker(initial)
	ts := &testSupervisor{supervisor: s, signals: make(chan os.Signal, 1), exited: make(chan struct{})}
	go func() {
		ts.err = s.run(ts.signals)
		close(ts.exited)
	}()
	t.Cleanup(func() {
		select {
		case s.fatal <- errors.New("test over"):
		default:
		}
		select {
		case <-ts.exited:
		case <-time.After(30 * time.Second):
			t.Error("supervisor did not exit at cleanup")
		}
		s.terminateAllWorkers()
	})
	waitForSupervisorStatus(t, unixHTTPClient(config.ControlSocket), nil)
	return ts
}

func (ts *testSupervisor) waitExit(t *testing.T, limit time.Duration, why string) error {
	t.Helper()
	select {
	case <-ts.exited:
		return ts.err
	case <-time.After(limit):
		t.Fatalf("supervisor still running %s after %s", why, limit)
		return nil
	}
}

func testSupervisorConfig(t *testing.T, dir string) supervisorConfig {
	return supervisorConfig{
		Addr:            reserveSupervisorAddress(t),
		ControlSocket:   filepath.Join(dir, "control.sock"),
		WorkerBin:       os.Args[0],
		ReadyTimeout:    10 * time.Second,
		DrainTimeout:    time.Minute,
		WorkerStopGrace: time.Second,
	}
}

func streamingUnixClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

func workerPID(t *testing.T, response *http.Response) int {
	t.Helper()
	pid, err := strconv.Atoi(response.Header.Get("X-Fake-Worker-Pid"))
	if err != nil {
		t.Fatalf("response carries no worker pid: %v", err)
	}
	return pid
}

func getThroughClaudeSocket(t *testing.T, socket, path string) (*http.Response, string) {
	t.Helper()
	client := unixHTTPClient(socket)
	defer client.CloseIdleConnections()
	response, err := client.Get("http://api.anthropic.com" + path)
	if err != nil {
		t.Fatalf("GET %s through the Claude socket: %v", path, err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, string(body)
}

func upgradeSupervisor(t *testing.T, control *http.Client) string {
	t.Helper()
	response, err := control.Post("http://supervisor/_subrouter/upgrade", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("upgrade status = %d: %s", response.StatusCode, body)
	}
	var upgraded struct {
		Active struct {
			ID string `json:"id"`
		} `json:"active"`
	}
	if err := json.NewDecoder(response.Body).Decode(&upgraded); err != nil {
		t.Fatal(err)
	}
	return upgraded.Active.ID
}
