// Package report sends hello and report requests to the platform and keeps
// samples in memory while the platform is unreachable.
package report

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/towamori/agent/protocol"
)

// StatusError is a non-2xx answer from the platform.
type StatusError struct {
	Code       int
	RetryAfter time.Duration
	Body       string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("platform answered HTTP %d", e.Code)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Client talks HTTPS to one platform endpoint with one token.
type Client struct {
	endpoint  string
	token     string
	userAgent string
	http      *http.Client
	// Print, when set, receives every request body before it is sent.
	Print io.Writer
}

func NewClient(endpoint, token string, agent protocol.AgentInfo) *Client {
	return &Client{
		endpoint:  strings.TrimRight(endpoint, "/"),
		token:     token,
		userAgent: fmt.Sprintf("towamori-agent/%s (%s/%s)", agent.Version, agent.OS, agent.Arch),
		http: &http.Client{
			Timeout: 15 * time.Second,
			// The ingest API never redirects. Refusing redirects means the
			// token can never be forwarded to another URL.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (c *Client) Hello(ctx context.Context, req protocol.HelloRequest) (protocol.HelloResponse, error) {
	var resp protocol.HelloResponse
	err := c.post(ctx, protocol.HelloPath, req, &resp)
	return resp, err
}

func (c *Client) Report(ctx context.Context, req protocol.ReportRequest) (protocol.ReportResponse, error) {
	var resp protocol.ReportResponse
	err := c.post(ctx, protocol.ReportPath, req, &resp)
	return resp, err
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if c.Print != nil {
		var pretty bytes.Buffer
		if json.Indent(&pretty, raw, "", "  ") == nil {
			fmt.Fprintf(c.Print, "POST %s\n%s\n", path, pretty.Bytes())
		}
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, &gz)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set(protocol.HeaderProtocol, strconv.Itoa(protocol.Version))

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &StatusError{Code: resp.StatusCode, Body: shorten(string(data))}
		if s := resp.Header.Get("Retry-After"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				se.RetryAfter = time.Duration(n) * time.Second
			}
		}
		return se
	}
	if len(bytes.TrimSpace(data)) == 0 || out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("platform sent an unreadable answer: %w", err)
	}
	return nil
}

func shorten(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
