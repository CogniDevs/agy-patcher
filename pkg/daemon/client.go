package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"agy-patcher/pkg/proxy"
)

// Client communicates with the background agpatcher tray/proxy daemon over loopback HTTP.
type Client struct {
	info Info
	http *http.Client
}

// Info returns the daemon metadata read from daemon.json.
func (c *Client) Info() Info {
	return c.info
}

// ReadStateFile reads daemon.json without attempting an HTTP connection.
func ReadStateFile() (Info, error) {
	statePath, err := StateFilePath()
	if err != nil {
		return Info{}, err
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return Info{}, ErrNotRunning
	}
	var info Info
	if err := json.Unmarshal(raw, &info); err != nil || info.Addr == "" || info.Token == "" {
		return Info{}, ErrNotRunning
	}
	return info, nil
}

// Connect loads daemon.json, pings the running daemon, and returns a Client + initial StateSnapshot.
func Connect() (*Client, *StateSnapshot, error) {
	info, err := ReadStateFile()
	if err != nil {
		return nil, nil, err
	}

	cl := &Client{
		info: info,
		http: &http.Client{
			Timeout:   8 * time.Second,
			Transport: &http.Transport{Proxy: nil},
		},
	}
	snap, err := cl.stateWithTimeout(0, 900*time.Millisecond)
	if err != nil {
		if info.PID > 0 && !IsPIDAlive(info.PID) {
			if sp, spErr := StateFilePath(); spErr == nil {
				removeOwnedStateFile(sp, info)
			}
		}
		return nil, nil, fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	return cl, snap, nil
}

// WaitUntilReady polls Connect until the daemon HTTP endpoint responds or timeout elapses.
func WaitUntilReady(timeout time.Duration) (*Client, *StateSnapshot, error) {
	deadline := time.Now().Add(timeout)
	for {
		cl, snap, err := Connect()
		if err == nil {
			return cl, snap, nil
		}
		if time.Now().After(deadline) {
			return nil, nil, err
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// State fetches current state and all logs with Seq > since.
func (c *Client) State(since uint64) (*StateSnapshot, error) {
	return c.stateWithTimeout(since, 1500*time.Millisecond)
}

func (c *Client) stateWithTimeout(since uint64, timeout time.Duration) (*StateSnapshot, error) {
	var snap StateSnapshot
	u := fmt.Sprintf("http://%s/v1/state?since=%s", c.info.Addr, strconv.FormatUint(since, 10))
	if err := c.doJSONTimeout(http.MethodGet, u, nil, &snap, timeout); err != nil {
		return nil, err
	}
	return &snap, nil
}

// StartProxy instructs the daemon to start its embedded bypass proxy.
func (c *Client) StartProxy() (*StateSnapshot, error) {
	var snap StateSnapshot
	u := fmt.Sprintf("http://%s/v1/proxy/start", c.info.Addr)
	if err := c.doJSONTimeout(http.MethodPost, u, nil, &snap, 8*time.Second); err != nil {
		return nil, err
	}
	return &snap, nil
}

// StopProxy instructs the daemon to stop its embedded bypass proxy.
func (c *Client) StopProxy() (*StateSnapshot, error) {
	var snap StateSnapshot
	u := fmt.Sprintf("http://%s/v1/proxy/stop", c.info.Addr)
	if err := c.doJSONTimeout(http.MethodPost, u, nil, &snap, 8*time.Second); err != nil {
		return nil, err
	}
	return &snap, nil
}

// SetLang updates the daemon/tray display language ("ru" or "en").
func (c *Client) SetLang(lang string) (*StateSnapshot, error) {
	var snap StateSnapshot
	u := fmt.Sprintf("http://%s/v1/lang", c.info.Addr)
	if err := c.doJSONTimeout(http.MethodPost, u, map[string]string{"lang": lang}, &snap, 2*time.Second); err != nil {
		return nil, err
	}
	return &snap, nil
}

// SetDNS pushes updated custom DNS resolution mode and provider list to the live daemon/proxy.
func (c *Client) SetDNS(mode string, providers []proxy.DNSProviderSpec) (*StateSnapshot, error) {
	var snap StateSnapshot
	u := fmt.Sprintf("http://%s/v1/dns", c.info.Addr)
	req := struct {
		Mode      string                  `json:"mode"`
		Providers []proxy.DNSProviderSpec `json:"providers"`
	}{
		Mode:      mode,
		Providers: providers,
	}
	if err := c.doJSONTimeout(http.MethodPost, u, req, &snap, 2*time.Second); err != nil {
		return nil, err
	}
	return &snap, nil
}

// RefreshAutostart notifies the daemon to refresh autostart status.
func (c *Client) RefreshAutostart() (*StateSnapshot, error) {
	var snap StateSnapshot
	u := fmt.Sprintf("http://%s/v1/autostart/sync", c.info.Addr)
	if err := c.doJSONTimeout(http.MethodPost, u, nil, &snap, 2*time.Second); err != nil {
		return nil, err
	}
	return &snap, nil
}

// Shutdown asks the daemon to stop proxy, clean the bypass proxy env var, remove tray icon, and exit.
func (c *Client) Shutdown() error {
	u := fmt.Sprintf("http://%s/v1/shutdown", c.info.Addr)
	if err := c.doJSONTimeout(http.MethodPost, u, nil, nil, 5*time.Second); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.info.PID <= 0 || !IsPIDAlive(c.info.PID) {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	return nil
}

func (c *Client) doJSONTimeout(method, rawURL string, body any, out any, timeout time.Duration) error {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(raw)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, rawURL, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.info.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("daemon http %d: %s", resp.StatusCode, string(msg))
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
