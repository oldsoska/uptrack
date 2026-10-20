// Package checker performs a single HTTP check of a monitor.
package checker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/oldsoska/uptrack/internal/monitor"
)

// ErrBlockedTarget is returned when a monitor resolves to an address the checker refuses to call.
var ErrBlockedTarget = errors.New("target resolves to a private or internal address")

// maxBodyBytes caps how much of a response is read; the body is drained only so the
// connection can be reused, its content does not matter.
const maxBodyBytes = 64 << 10

type HTTPChecker struct {
	client *http.Client
	now    func() time.Time
}

// New returns a checker. With blockPrivate set, connections to loopback, private, link-local
// and similar ranges are refused, so a monitor cannot be used to probe the server's own network
// (SSRF), e.g. the cloud metadata endpoint at 169.254.169.254.
func New(blockPrivate bool) *HTTPChecker {
	if !blockPrivate {
		return newChecker(nil)
	}
	return newChecker(func(target netip.AddrPort) bool { return isInternal(target.Addr()) })
}

// newChecker takes the blocking rule as a function so tests can exercise it with loopback servers.
func newChecker(blocked func(netip.AddrPort) bool) *HTTPChecker {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if blocked != nil {
		// Control runs after DNS resolution with the concrete IP being dialled. Checking here rather
		// than resolving the hostname up front also defeats DNS rebinding, and applies to every
		// redirect hop, since each one dials through this transport.
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			target, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if blocked(target) {
				return ErrBlockedTarget
			}
			return nil
		}
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &HTTPChecker{
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("stopped after 5 redirects")
				}
				return nil
			},
		},
		now: time.Now,
	}
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isInternal(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || cgnat.Contains(ip)
}

// Check requests the monitor's URL once. It never returns an error: every failure is a
// failed Result with the reason in Result.Error.
func (c *HTTPChecker) Check(ctx context.Context, m monitor.Monitor) monitor.Result {
	started := c.now()
	result := monitor.Result{MonitorID: m.ID, CheckedAt: started}

	ctx, cancel := context.WithTimeout(ctx, m.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, m.Method, m.URL, nil)
	if err != nil {
		result.Error = "invalid request: " + err.Error()
		return result
	}
	req.Header.Set("User-Agent", "Uptrack/1.0 (+https://github.com/oldsoska/uptrack)")

	resp, err := c.client.Do(req)
	result.Latency = c.now().Sub(started)
	if err != nil {
		result.Error = describe(err, m.Timeout)
		return result
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))

	result.StatusCode = resp.StatusCode
	if resp.StatusCode < m.ExpectedStatusMin || resp.StatusCode > m.ExpectedStatusMax {
		result.Error = fmt.Sprintf("unexpected status %d (expected %d-%d)",
			resp.StatusCode, m.ExpectedStatusMin, m.ExpectedStatusMax)
		return result
	}
	result.OK = true
	return result
}

// describe turns transport errors into short, stable causes for incidents and alerts.
func describe(err error, timeout time.Duration) string {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, ErrBlockedTarget):
		return ErrBlockedTarget.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("timed out after %s", timeout)
	case errors.As(err, &dnsErr):
		return "DNS lookup failed: " + dnsErr.Err
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	default:
		return err.Error()
	}
}
