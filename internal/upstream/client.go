// Package upstream is the agent's client for the server's machine API
// (/api/machine/... on vending-server), authenticated with the machine key.
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrOffline means the server could not be reached: no network, a timeout,
// or the proxy in front of it answering because the server itself is down.
var ErrOffline = errors.New("server unreachable")

type offlineError struct{ cause error }

func (e *offlineError) Error() string        { return "server unreachable: " + e.cause.Error() }
func (e *offlineError) Is(target error) bool { return target == ErrOffline }
func (e *offlineError) Unwrap() error        { return e.cause }

// IsOffline reports whether err means the server could not be reached.
func IsOffline(err error) bool { return errors.Is(err, ErrOffline) }

// APIError is an error answer from the server application: a non-2xx status
// with its JSON body {"code", "message", ...}.
type APIError struct {
	Status  int
	Code    string
	Message string
	Body    []byte
}

func (e *APIError) Error() string {
	return fmt.Sprintf("server answered %d %s: %s", e.Status, e.Code, e.Message)
}

// AsAPIError returns the *APIError inside err.
func AsAPIError(err error) (*APIError, bool) {
	var e *APIError
	ok := errors.As(err, &e)
	return e, ok
}

// Response is the server's answer, as received.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Client calls one server with one machine key.
type Client struct {
	base      string
	key       string
	userAgent string
	hc        *http.Client
}

const maxBody = 8 << 20

// New returns a client for baseURL (e.g. https://35.219.98.32). insecureTLS
// skips certificate checks, for a server with a self-signed certificate only.
func New(baseURL, machineKey string, insecureTLS bool, version string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("serverUrl %q: use http://host:port or https://host", baseURL)
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	if insecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in for a self-signed demo server
	}
	return &Client{
		base:      u.String(),
		key:       machineKey,
		userAgent: "vending-machine/" + version,
		hc:        &http.Client{Transport: transport},
	}, nil
}

// BaseURL is the server address.
func (c *Client) BaseURL() string { return c.base }

// Do sends one request and reads the whole answer. Every request carries the
// machine key; header adds Authorization (an operator token) or X-Request-ID.
func (c *Client) Do(ctx context.Context, method, path string, body []byte, header http.Header) (*Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("X-Machine-Key", c.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &offlineError{cause: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, &offlineError{cause: err}
	}
	out := &Response{Status: resp.StatusCode, Header: resp.Header, Body: data}
	// Caddy answers 502 when the Go server is stopped; that is "offline" too.
	// The server's own errors always carry a JSON code.
	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
		if code, _ := errorCode(data); code == "" {
			return nil, &offlineError{cause: fmt.Errorf("HTTP %d from %s", resp.StatusCode, c.base)}
		}
	}
	return out, nil
}

// JSON sends in (nil for no body) and decodes a 2xx answer into out (may be
// nil). A non-2xx answer is returned as an *APIError, with the response.
func (c *Client) JSON(ctx context.Context, method, path string, in any, header http.Header, out any) (*Response, error) {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = b
	}
	resp, err := c.Do(ctx, method, path, body, header)
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		return resp, AsError(resp)
	}
	if out != nil && len(resp.Body) > 0 {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return resp, fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return resp, nil
}

// AsError turns a non-2xx response into an *APIError.
func AsError(resp *Response) *APIError {
	code, msg := errorCode(resp.Body)
	if code == "" {
		code = fmt.Sprintf("HTTP_%d", resp.Status)
	}
	return &APIError{Status: resp.Status, Code: code, Message: msg, Body: resp.Body}
}

func errorCode(body []byte) (code, message string) {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) != nil {
		return "", ""
	}
	return e.Code, e.Message
}

// Get streams a plain GET, for product images. The caller closes the body.
func (c *Client) Get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &offlineError{cause: err}
	}
	return resp, nil
}
