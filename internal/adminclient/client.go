// Package adminclient is the bearer-only transport shared by dsgctl commands.
package adminclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/secretfile"
)

var (
	ErrUnsupported = errors.New("command is not supported by this gateway")
	ErrNotFound    = errors.New("Admin resource not found (HTTP 404)")
)

const maxResponseBytes = 2 << 20

type Client struct {
	endpoint string
	token    string
	secret   string
	http     *http.Client
}

func New(endpoint, tokenFile string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(endpoint, "#") || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return nil, errors.New("invalid Admin endpoint: use an HTTP loopback or HTTPS origin")
	}
	if u.Scheme != "https" {
		ip, err := netip.ParseAddr(u.Hostname())
		if u.Scheme != "http" || err != nil || !ip.Unmap().IsLoopback() {
			return nil, errors.New("plaintext Admin endpoint must use literal loopback; remote Admin requires HTTPS")
		}
	}
	if timeout <= 0 || timeout > 30*time.Minute {
		return nil, errors.New("timeout must be positive and at most 30m")
	}
	b, err := secretfile.Read(tokenFile, 4096)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	_, secret, ok := admin.ParseBearer([]string{"Bearer " + token})
	if !ok {
		return nil, errors.New("invalid bearer token file")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{endpoint: u.Scheme + "://" + u.Host, token: token, secret: secret, http: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	// Commands choose Admin routes only; a bearer can never become S3 authorization.
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/admin/v1/") || u.Fragment != "" || u.RawPath != "" || strings.Contains(u.Path, "..") {
		return nil, errors.New("invalid Admin route")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return nil, errors.New("cannot construct Admin request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Transport errors can contain URLs or credential-bearing redirects.
		return nil, errors.New("Admin request failed (connection, TLS or timeout)")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return nil, ErrUnsupported
	case http.StatusUnauthorized:
		return nil, errors.New("Admin authentication failed (HTTP 401)")
	case http.StatusForbidden:
		return nil, errors.New("Admin access denied (HTTP 403)")
	}
	if response.StatusCode != http.StatusNotFound && (response.StatusCode < 200 || response.StatusCode >= 300) {
		return nil, fmt.Errorf("Admin request failed (HTTP %d)", response.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(b) > maxResponseBytes {
		return nil, errors.New("Admin response unreadable or exceeds 2 MiB")
	}
	if response.StatusCode == http.StatusNotFound {
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		// Admin resource errors have the API envelope; router 404s do not.
		if json.Unmarshal(b, &failure) == nil && failure.Error.Code != "" {
			return nil, ErrNotFound
		}
		return nil, ErrUnsupported
	}
	if !json.Valid(b) {
		return nil, errors.New("Admin response is not JSON")
	}
	b = bytes.ReplaceAll(b, []byte(c.token), []byte("[REDACTED]"))
	b = bytes.ReplaceAll(b, []byte(c.secret), []byte("[REDACTED]"))
	return b, nil
}
