package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimits(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	remaining := map[string]int{"graphql": 4000, "core": 500}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resource := "core"
		if r.URL.Path == "/graphql" {
			resource = "graphql"
		}
		w.Header().Set("X-RateLimit-Resource", resource)
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(remaining[resource]))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	g := &github{client: srv.Client(), token: "t", expires: time.Now().Add(time.Hour)}
	ctx := context.Background()

	if _, _, ok := g.low(1000); ok {
		t.Error("low before any answer")
	}
	if err := g.call(ctx, "POST", srv.URL+"/graphql", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := g.low(1000); ok {
		t.Error("low with 4000 GraphQL points left")
	}
	if err := g.call(ctx, "GET", srv.URL+"/repos/o/r", nil, nil); err != nil {
		t.Fatal(err)
	}
	r, l, ok := g.low(1000)
	if !ok || r != "core" || l.Remaining != 500 || !l.Reset.Equal(reset) {
		t.Errorf("low: %q %+v %v, want core with 500 left", r, l, ok)
	}

	// A limit whose reset has passed is not low any more.
	g.limits["core"] = rateLimit{0, time.Now().Add(-time.Minute)}
	if _, _, ok := g.low(1000); ok {
		t.Error("low after the reset")
	}
}
