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
	t.Cleanup(func(s time.Duration) func() { return func() { settle = s } }(settle))
	settle = 0
	for _, tc := range []struct {
		name, pr string // the pull request read again, or "" if reading it fails
		park     bool
	}{
		{"merged meanwhile", `{"state": "MERGED", "isInMergeQueue": false, "autoMergeRequest": null}`, false},
		{"closed meanwhile", `{"state": "CLOSED", "isInMergeQueue": false, "autoMergeRequest": null}`, false},
		{"queued meanwhile", `{"state": "OPEN", "isInMergeQueue": true, "autoMergeRequest": null}`, false},
		{"auto-merge on meanwhile", `{"state": "OPEN", "isInMergeQueue": false, "autoMergeRequest": {"enabledAt": "2026-11-18T02:00:41Z"}}`, false},
		{"still needs auto-merge", `{"state": "OPEN", "isInMergeQueue": false, "autoMergeRequest": null}`, true},
		{"cannot read it again", "", true},
	} {
		var parked bool
		reads := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			switch {
			case r.URL.Path == "/graphql" && strings.Contains(string(body), "enablePullRequestAutoMerge"):
				fmt.Fprint(w, `{"errors": [{"message": "Pull request is already merged"}]}`)
			case r.URL.Path == "/graphql" && tc.pr == "":
				reads++
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			case r.URL.Path == "/graphql":
				reads++
				fmt.Fprintf(w, `{"data": {"repository": {"pullRequest": %s}}}`, tc.pr)
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
		if parked != tc.park || reads != 1 {
			t.Errorf("%s: parked %v after %d reads, want parked %v after 1", tc.name, parked, reads, tc.park)
		}
		srv.Close()
	}
}
