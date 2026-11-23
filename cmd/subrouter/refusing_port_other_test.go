//go:build !unix

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// refusingPortURL is a loopback URL whose port refused connections when it
// was returned: a server started and closed. Unlike the unix one it does not
// hold the port.
func refusingPortURL(t *testing.T) string {
	t.Helper()
	box := httptest.NewServer(http.NotFoundHandler())
	box.Close()
	return box.URL
}
