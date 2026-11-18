package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCountRounds(t *testing.T) {
	agent := &actor{"Bot", "fc-agent"}
	c := func(a *actor, body string) comment { return comment{body, a} }
	for _, tc := range []struct {
		name         string
		comments     []comment
		rounds, idle int
	}{
		{"none", nil, 0, 0},
		{"rounds", []comment{
			c(agent, roundMarker+"\n…"), c(agent, idleMarker+"\n…"), c(agent, idleMarker+"\n…"),
		}, 3, 2},
		{"progress ends the idle streak", []comment{
			c(agent, idleMarker+"\n…"), c(agent, roundMarker+"\n…"), c(agent, idleMarker+"\n…"),
		}, 3, 1},
		{"parking starts afresh", []comment{
			c(agent, idleMarker+"\n…"), c(agent, idleMarker+"\n…"), c(agent, parkMarker+"\n…"), c(agent, roundMarker+"\n…"),
		}, 1, 0},
		{"only the worker's own comments", []comment{
			c(&actor{"User", "mallory"}, idleMarker), c(&actor{"User", "fc-agent"}, idleMarker), c(nil, idleMarker),
		}, 0, 0},
		{"only markers at the start", []comment{
			c(agent, "> "+idleMarker), c(agent, "As the worker said: "+roundMarker),
		}, 0, 0},
	} {
		rounds, idle := countRounds(tc.comments, "fc-agent[bot]")
		if rounds != tc.rounds || idle != tc.idle {
			t.Errorf("%s: %d rounds, %d idle, want %d and %d", tc.name, rounds, idle, tc.rounds, tc.idle)
		}
	}
}

func TestAutoMerge(t *testing.T) {
	settle = 0
	for _, tc := range []struct {
		name, state string
		park        bool
	}{
		{"merged meanwhile", "MERGED", false},
		{"still open", "OPEN", true},
	} {
		var parked bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			switch {
			case r.URL.Path == "/graphql" && strings.Contains(string(body), "enablePullRequestAutoMerge"):
				fmt.Fprint(w, `{"errors": [{"message": "Pull request is already merged"}]}`)
			case r.URL.Path == "/graphql":
				fmt.Fprintf(w, `{"data": {"repository": {"pullRequest": {"state": %q, "isInMergeQueue": false, "autoMergeRequest": null}}}}`, tc.state)
			case r.URL.Path == "/repos/o/r/issues/7/labels":
				parked = true
				fmt.Fprint(w, `[]`)
			case r.URL.Path == "/repos/o/r/issues/7/comments":
				fmt.Fprint(w, `{}`)
			default:
				http.NotFound(w, r)
			}
		}))
		g := &github{api: srv.URL, client: srv.Client(), token: "t", expires: time.Now().Add(time.Hour)}
		w := &worker{c: &Config{Repo: "o/r", Name: "w"}, gh: g}
		if err := w.autoMerge(context.Background(), Item{PR: true, ID: "PR_1", Number: 7}); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if parked != tc.park {
			t.Errorf("%s: parked %v, want %v", tc.name, parked, tc.park)
		}
		srv.Close()
	}
}
