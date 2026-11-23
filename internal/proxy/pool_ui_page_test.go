package proxy

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// nodeForTest returns the node that runs the board page's script. Without
// node the test skips, and with SUBROUTER_TEST_REQUIRE_NODE=1, which the
// Docker test image and CI set, it fails instead. A skip there is how these
// tests once went unrun.
func nodeForTest(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err == nil {
		return node
	}
	if os.Getenv("SUBROUTER_TEST_REQUIRE_NODE") == "1" {
		t.Fatalf("SUBROUTER_TEST_REQUIRE_NODE=1 and no node to run the board page: %v", err)
	}
	t.Skipf("no node to run the board page (deploy/docker/test.sh has one): %v", err)
	return ""
}

// The board page's own tests (pool_ui_page_test.mjs) run its script on
// pool.json answers in node, so `go test ./internal/proxy` runs them too.
func TestTheBoardPagePassesItsNodeTests(t *testing.T) {
	node := nodeForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "--test", "pool_ui_page_test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("node --test pool_ui_page_test.mjs: %v\n%s", err, out)
	}
}
