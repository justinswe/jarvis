package mcpx

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/justinswe/std/errors"
)

// newHTTPClient builds the outbound client for remote MCP servers. The DialContext
// hook re-checks the addresses a name actually resolves to and dials the vetted IP,
// which is what closes DNS rebinding: the allow/deny decision and the connection use
// the same resolution. This protects the combined image's unauthenticated loopback
// Valkey and NATS from a guild-configured URL, no matter who wrote the row.
func newHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{Transport: &http.Transport{
		// No proxy, deliberately. A proxy would make DialContext receive the proxy's
		// address instead of the MCP server's, so the resolved-IP check below would vet
		// the wrong host and this guard would silently void itself based on an
		// environment variable set outside the application.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, errors.Wrapf(err, "resolve MCP server host %q", host)
			}
			if len(addresses) == 0 {
				return nil, errors.Errorf("MCP server host %q resolved to no addresses", host)
			}
			if !allowPrivate {
				for _, address := range addresses {
					if internalIP(address.IP) {
						return nil, errors.Errorf("MCP server host %q resolves to a private or internal network", host)
					}
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
		},
	}}
}

// internalIP reports whether ip belongs to a range an internet-facing MCP server can
// never legitimately occupy: loopback, RFC1918/ULA private, link-local (which covers
// cloud metadata services), and unspecified.
func internalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
