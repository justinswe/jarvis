package character

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/justinswe/std/errors"
)

// maxCardBytes bounds a fetched card file (PNG or JSON).
const maxCardBytes = 5 << 20

// fetchClient refuses hosts resolving to private or internal networks and dials the
// vetted IP — the same DNS-rebinding guard as worker/pkg/mcpx. Card URLs are untrusted
// user input, so there is no allow-private override: local cards arrive as attachments.
var fetchClient = &http.Client{Transport: &http.Transport{
	Proxy: nil,
	DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, errors.Wrapf(err, "resolve card host %q", host)
		}
		if len(addresses) == 0 {
			return nil, errors.Errorf("card host %q resolved to no addresses", host)
		}
		for _, a := range addresses {
			if a.IP.IsLoopback() || a.IP.IsPrivate() || a.IP.IsLinkLocalUnicast() ||
				a.IP.IsLinkLocalMulticast() || a.IP.IsUnspecified() {
				return nil, errors.Errorf("card host %q resolves to a private or internal network", host)
			}
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	},
}}

// Fetch downloads a card file from an https URL, bounded by maxCardBytes.
func Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.Wrap(err, "parse card URL")
	}
	if u.Scheme != "https" || u.User != nil {
		return nil, errors.New("card URLs must be https without credentials")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.Wrap(err, "build card request")
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "fetch character card")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Errorf("card fetch returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCardBytes+1))
	if err != nil {
		return nil, errors.Wrap(err, "read character card")
	}
	if len(body) > maxCardBytes {
		return nil, errors.Errorf("card file exceeds %d bytes", maxCardBytes)
	}
	return body, nil
}

// Decode splits a card file into card JSON and, for PNG cards, the original image bytes.
func Decode(body []byte) (cardJSON, avatarPNG []byte, err error) {
	if bytes.HasPrefix(body, pngSignature) {
		card, err := ExtractCard(body)
		return card, body, err
	}
	return body, nil, nil
}
