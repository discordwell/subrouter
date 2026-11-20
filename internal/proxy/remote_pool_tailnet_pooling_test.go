package proxy

import (
	"net/http"
	"testing"
)

func TestPeerOnTheTailnetKeepsItsConnectionPool(t *testing.T) {
	p := NewRemotePool("https://203.0.113.10:31416", "", "").WithFingerprint("00").WithName("box")
	if err := DialPeersOverTailnet([]*RemotePool{p}, "box=100.64.0.7"); err != nil {
		t.Fatal(err)
	}
	tr := p.client.Transport.(*http.Transport)
	if tr.DialTLSContext == nil {
		t.Fatal("tailnet routing was not installed")
	}
	if tr.MaxIdleConns != remotePoolMaxIdleConns || tr.MaxIdleConnsPerHost != remotePoolMaxIdleConns || tr.IdleConnTimeout != remotePoolIdleConnTimeout {
		t.Fatalf("pool lost: MaxIdleConns=%d MaxIdleConnsPerHost=%d IdleConnTimeout=%s", tr.MaxIdleConns, tr.MaxIdleConnsPerHost, tr.IdleConnTimeout)
	}
}
