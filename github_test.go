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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rate_limit" || r.Header.Get("Authorization") != "Bearer t" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"resources": {"core": {"remaining": 500, "reset": %d}, "graphql": {"remaining": 4000, "reset": %d}, "search": {"remaining": 0, "reset": %d}}}`,
			reset.Unix(), reset.Unix()+60, reset.Unix())
	}))
	defer srv.Close()
	g := &github{api: srv.URL, client: srv.Client(), token: "t", expires: time.Now().Add(time.Hour)}
	graphql, rest, err := g.rateLimits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if graphql.Remaining != 4000 || !graphql.Reset.Equal(reset.Add(time.Minute)) {
		t.Errorf("GraphQL %+v, want 4000 left until %v", graphql, reset.Add(time.Minute))
	}
	if rest.Remaining != 500 || !rest.Reset.Equal(reset) {
		t.Errorf("REST %+v, want 500 left until %v", rest, reset)
	}
}
