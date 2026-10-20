package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/oldsoska/uptrack/internal/monitor"
)

func monitorFor(url string) monitor.Monitor {
	return monitor.Monitor{
		ID: 1, URL: url, Method: "GET", Timeout: 2 * time.Second,
		ExpectedStatusMin: 200, ExpectedStatusMax: 399,
	}
}

func TestCheckSucceedsOnExpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "Uptrack/") {
			t.Errorf("user agent = %q", r.UserAgent())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r := New(false).Check(context.Background(), monitorFor(srv.URL))

	if !r.OK || r.StatusCode != http.StatusNoContent || r.Error != "" {
		t.Fatalf("got %+v", r)
	}
	if r.Latency <= 0 {
		t.Errorf("latency not measured: %v", r.Latency)
	}
}

func TestCheckFailsOnUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	r := New(false).Check(context.Background(), monitorFor(srv.URL))

	if r.OK || r.StatusCode != 503 || !strings.Contains(r.Error, "unexpected status 503") {
		t.Fatalf("got %+v", r)
	}
}

func TestCheckTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	m := monitorFor(srv.URL)
	m.Timeout = 100 * time.Millisecond
	r := New(false).Check(context.Background(), m)

	if r.OK || r.Error != "timed out after 100ms" {
		t.Fatalf("got %+v", r)
	}
}

func TestCheckReportsConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens on the port any more

	r := New(false).Check(context.Background(), monitorFor(url))

	if r.OK || r.Error != "connection refused" {
		t.Fatalf("got %+v", r)
	}
}

func TestCheckBlocksInternalTargets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an internal address must not be called")
	}))
	defer srv.Close()

	r := New(true).Check(context.Background(), monitorFor(srv.URL)) // 127.0.0.1

	if r.OK || r.Error != ErrBlockedTarget.Error() {
		t.Fatalf("got %+v", r)
	}
}

func TestCheckBlocksRedirectToInternalTarget(t *testing.T) {
	// An allowed first hop that redirects to a blocked address must be stopped at the redirect.
	// Both servers are on loopback, so the rule here blocks by port: only the second one counts as internal.
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("redirect target must not be called")
	}))
	defer internal.Close()
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer public.Close()

	internalPort := netip.MustParseAddrPort(strings.TrimPrefix(internal.URL, "http://")).Port()
	c := newChecker(func(target netip.AddrPort) bool { return target.Port() == internalPort })
	r := c.Check(context.Background(), monitorFor(public.URL))

	if r.OK || r.Error != ErrBlockedTarget.Error() {
		t.Fatalf("got %+v", r)
	}
}

func TestIsInternal(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1":       true,
		"10.1.2.3":        true,
		"172.16.0.1":      true,
		"192.168.1.10":    true,
		"169.254.169.254": true, // cloud metadata
		"100.64.0.1":      true, // carrier-grade NAT
		"0.0.0.0":         true,
		"::1":             true,
		"fd00::1":         true,
		"::ffff:10.0.0.1": true, // IPv4-mapped IPv6 must not slip through
		"8.8.8.8":         false,
		"2a00:1450::1":    false,
	} {
		if got := isInternal(netip.MustParseAddr(addr)); got != want {
			t.Errorf("isInternal(%s) = %v, want %v", addr, got, want)
		}
	}
}
