// Package client talks to the running `cox server` over its HTTP API. The
// CLI uses it (and later the desktop app).
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/hua-ri/coxswain/internal/clients"
	"github.com/hua-ri/coxswain/internal/eventlog"
	"github.com/hua-ri/coxswain/internal/instance"
	"github.com/hua-ri/coxswain/internal/server"
)

// Client calls one server as one client name (cli, desktop).
type Client struct {
	Info instance.Info
	Name string
	HTTP *http.Client
}

// Outcome is an action's result with the result left as JSON.
type Outcome struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Hint   string          `json:"hint,omitempty"`
}

func New(info instance.Info, name string) *Client {
	return &Client{Info: info, Name: name, HTTP: &http.Client{}}
}

// Discover finds the running server in home.
func Discover(ctx context.Context, home, name string) (*Client, error) {
	info, err := instance.Discover(ctx, home)
	if err != nil {
		return nil, err
	}
	return New(info, name), nil
}

// do sends a request; a server that cannot be reached is ErrNotRunning.
func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.Info.Addr+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Info.Token)
	req.Header.Set(server.ClientHeader, c.Name)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &instance.NotRunningError{Cause: err}
	}
	return resp, nil
}

// call sends a request and decodes a JSON answer into v. Non-2xx answers
// become errors carrying the server's message.
func (c *Client) call(ctx context.Context, method, path string, body []byte, v any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var out Outcome
		json.NewDecoder(resp.Body).Decode(&out)
		if out.Error == "" {
			out.Error = resp.Status
		}
		return fmt.Errorf("server: %s (%d)", out.Error, resp.StatusCode)
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Actions lists the server's actions.
func (c *Client) Actions(ctx context.Context) ([]server.ActionInfo, error) {
	var out []server.ActionInfo
	return out, c.call(ctx, http.MethodGet, "/api/v1/actions", nil, &out)
}

// Invoke runs an action. An action that fails is an Outcome with status
// "error", not a Go error.
func (c *Client) Invoke(ctx context.Context, id string, args json.RawMessage) (Outcome, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/v1/actions/"+url.PathEscape(id), args)
	if err != nil {
		return Outcome{}, err
	}
	defer resp.Body.Close()
	var out Outcome
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("server: %s: %w", resp.Status, err)
	}
	return out, nil
}

// Clients lists the authorized clients.
func (c *Client) Clients(ctx context.Context) ([]clients.Client, error) {
	var out []clients.Client
	return out, c.call(ctx, http.MethodGet, "/api/v1/clients", nil, &out)
}

// Revoke withdraws a client's access; it is refused until allowed again.
func (c *Client) Revoke(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/api/v1/clients/"+url.PathEscape(name), nil, nil)
}

// Allow lets a client use Coxswain without asking.
func (c *Client) Allow(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodPut, "/api/v1/clients/"+url.PathEscape(name), nil, nil)
}

// Events streams the activity stream until ctx ends; the channel then
// closes.
func (c *Client) Events(ctx context.Context) (<-chan eventlog.Event, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/events", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("server: events: %s", resp.Status)
	}
	ch := make(chan eventlog.Event)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev eventlog.Event
			if json.Unmarshal([]byte(data), &ev) != nil {
				continue
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
