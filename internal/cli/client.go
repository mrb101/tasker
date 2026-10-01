package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Exit codes (documented in agent-guide).
const (
	ExitError    = 1
	ExitUsage    = 2
	ExitNotFound = 3
	ExitConflict = 4
	ExitInvalid  = 5
)

type APIError struct {
	Status int
	Kind   string
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

func (e *APIError) ExitCode() int {
	switch e.Kind {
	case "not_found":
		return ExitNotFound
	case "conflict":
		return ExitConflict
	case "invalid":
		return ExitInvalid
	}
	return ExitError
}

type Client struct {
	Base  string
	Actor string
	http  *http.Client
}

func NewClient(base, actor string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Actor: actor, http: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) raw(method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tasker-Actor", c.Actor)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach tasker at %s (set TASKER_URL): %w", c.Base, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		var e struct{ Error, Kind string }
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, &APIError{Status: res.StatusCode, Kind: e.Kind, Msg: e.Error}
		}
		return nil, &APIError{Status: res.StatusCode, Msg: fmt.Sprintf("%s %s: %s", method, path, res.Status)}
	}
	return data, nil
}

// Do performs a request and decodes the JSON response into out (if non-nil).
func (c *Client) Do(method, path string, body, out any) error {
	data, err := c.raw(method, path, body)
	if err != nil {
		return err
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func q(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Add(kv[i], kv[i+1])
		}
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

func esc(s string) string { return url.PathEscape(s) }
