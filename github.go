package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// api is GitHub's API.
const api = "https://api.github.com"

// github calls GitHub's REST and GraphQL APIs with tokens from hive-broker.
type github struct {
	api    string
	broker Broker
	client *http.Client

	token   string
	expires time.Time
}

func newGitHub(c *Config) *github {
	return &github{api: api, broker: c.Broker, client: &http.Client{Timeout: time.Minute}}
}

// getToken returns a GitHub token, asking the broker for a new one when the
// last one has less than five minutes left.
func (g *github) getToken(ctx context.Context) (string, error) {
	if time.Until(g.expires) > 5*time.Minute {
		return g.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.broker.URL+"/github/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.broker.Token)
	var t struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := g.send(req, &t); err != nil {
		return "", fmt.Errorf("broker: %w", err)
	}
	g.token, g.expires = t.Token, t.ExpiresAt
	return g.token, nil
}

// rest calls the REST API, such as rest(ctx, "POST", "/repos/o/r/issues/1/labels", body, nil).
func (g *github) rest(ctx context.Context, method, path string, body, out any) error {
	return g.call(ctx, method, g.api+path, body, out)
}

// graphql runs a GraphQL query and decodes its data into out.
func (g *github) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	var r struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := g.call(ctx, http.MethodPost, g.api+"/graphql", map[string]any{"query": query, "variables": vars}, &r); err != nil {
		return err
	}
	if len(r.Errors) > 0 {
		return &refusal{r.Errors[0].Message}
	}
	return json.Unmarshal(r.Data, out)
}

// refusal is GitHub's answer to a GraphQL request that reached it and
// that it did not carry out, with its reason. A request that fails on the
// way, or with a status outside 2xx, gives another error.
type refusal struct{ message string }

func (r *refusal) Error() string { return "graphql: " + r.message }

func (g *github) call(ctx context.Context, method, url string, body, out any) error {
	token, err := g.getToken(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return g.send(req, out)
}

// send sends a request and decodes a JSON answer into out, if out is not
// nil. An answer outside 2xx is an error with GitHub's message.
func (g *github) send(req *http.Request, out any) error {
	req.Header.Set("User-Agent", "fc-worker")
	if req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct{ Message string }
		json.Unmarshal(data, &e)
		if e.Message == "" {
			return errors.New(resp.Status)
		}
		return fmt.Errorf("%s: %s", resp.Status, e.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// rateLimit is what is left of one of GitHub's rate limits until it resets.
type rateLimit struct {
	Remaining int
	Reset     time.Time
}

// rateLimits returns what is left of the GraphQL and REST rate limits of
// the installation whose token the worker uses. The workers and agent
// jobs of an installation share these. Asking costs nothing.
func (g *github) rateLimits(ctx context.Context) (graphql, rest rateLimit, err error) {
	var r struct {
		Resources map[string]struct {
			Remaining int   `json:"remaining"`
			Reset     int64 `json:"reset"`
		} `json:"resources"`
	}
	if err := g.rest(ctx, http.MethodGet, "/rate_limit", nil, &r); err != nil {
		return graphql, rest, err
	}
	for _, name := range []string{"graphql", "core"} {
		if _, ok := r.Resources[name]; !ok {
			return graphql, rest, fmt.Errorf("rate_limit: no %s limit in GitHub's answer", name)
		}
	}
	get := func(name string) rateLimit {
		l := r.Resources[name]
		return rateLimit{l.Remaining, time.Unix(l.Reset, 0)}
	}
	return get("graphql"), get("core"), nil
}
