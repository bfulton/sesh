package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Remote is a store on a sesh server (see package server). Objects are sent
// already encrypted; the server never sees keys or plaintext.
//
// Protocol:
//
//	PUT /v1/o/<name>             store an object (bearer token required)
//	GET /v1/o/<name>             fetch an object
//	GET /v1/list?prefix=<prefix> newline-separated names (bearer token required)
type Remote struct {
	Base   *url.URL
	Token  string // bearer token; derived from the vault key for vault owners
	Client *http.Client
}

// NewRemote returns a store for the sesh server at raw.
func NewRemote(raw string) (*Remote, error) {
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil {
		return nil, err
	}
	return &Remote{Base: u, Client: &http.Client{Timeout: 60 * time.Second}}, nil
}

func (r *Remote) do(ctx context.Context, method, path string, q url.Values, body []byte) (*http.Response, error) {
	u := *r.Base
	u.Path += path
	u.RawQuery = q.Encode()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	return r.Client.Do(req)
}

func statusErr(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("sesh server: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
}

func (r *Remote) Put(ctx context.Context, name string, data []byte) error {
	if err := CheckName(name); err != nil {
		return err
	}
	resp, err := r.do(ctx, http.MethodPut, "/v1/o/"+name, nil, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusErr(resp)
	}
	return nil
}

func (r *Remote) Get(ctx context.Context, name string) ([]byte, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	resp, err := r.do(ctx, http.MethodGet, "/v1/o/"+name, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr(resp)
	}
	return io.ReadAll(resp.Body)
}

func (r *Remote) List(ctx context.Context, prefix string) ([]string, error) {
	if err := checkPrefix(prefix); err != nil {
		return nil, err
	}
	resp, err := r.do(ctx, http.MethodGet, "/v1/list", url.Values{"prefix": {prefix}}, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}
