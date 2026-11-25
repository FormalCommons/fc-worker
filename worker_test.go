package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	const (
		open    = `{"state": "OPEN", "isInMergeQueue": false, "autoMergeRequest": null}`
		merged  = "Pull request is already merged"
		hourly  = "API rate limit already exceeded for installation ID 1."
		atOnce  = "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."
		unknown = "Something went wrong while executing your query."
	)
	for _, tc := range []struct {
		name, refusal string // what GitHub says when it does not turn auto-merge on
		pr            string // the pull request read again, or "" if reading it fails
		park, fails   bool
	}{
		{"merged meanwhile", merged, `{"state": "MERGED", "isInMergeQueue": false, "autoMergeRequest": null}`, false, false},
		{"closed meanwhile", merged, `{"state": "CLOSED", "isInMergeQueue": false, "autoMergeRequest": null}`, false, false},
		{"queued meanwhile", merged, `{"state": "OPEN", "isInMergeQueue": true, "autoMergeRequest": null}`, false, false},
		{"auto-merge on meanwhile", merged, `{"state": "OPEN", "isInMergeQueue": false, "autoMergeRequest": {"enabledAt": "2026-11-18T02:00:41Z"}}`, false, false},
		{"still needs auto-merge", unknown, open, true, false},
		// Neither says anything of the pull request, so it is not parked.
		{"cannot read it again", unknown, "", false, true},
		{"the hourly rate limit", hourly, open, false, true},
		{"the hourly rate limit, and cannot read it again", hourly, "", false, true},
		{"a secondary rate limit", atOnce, open, false, true},
		{"a rate limit, and merged meanwhile", hourly, `{"state": "MERGED", "isInMergeQueue": false, "autoMergeRequest": null}`, false, false},
	} {
		var parked bool
		reads := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			switch {
			case r.URL.Path == "/graphql" && strings.Contains(string(body), "enablePullRequestAutoMerge"):
				fmt.Fprintf(w, `{"errors": [{"message": %q}]}`, tc.refusal)
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
		err := w.autoMerge(context.Background(), Item{PR: true, ID: "PR_1", Number: 7})
		if (err != nil) != tc.fails {
			t.Errorf("%s: error %v, want one: %v", tc.name, err, tc.fails)
		}
		if tc.fails && !strings.Contains(err.Error(), tc.refusal) {
			t.Errorf("%s: the error %q does not have GitHub's refusal", tc.name, err)
		}
		if parked != tc.park || reads != 1 {
			t.Errorf("%s: parked %v after %d reads, want parked %v after 1", tc.name, parked, reads, tc.park)
		}
		srv.Close()
	}
}

// fakes is a GitHub and a desk that follows it. Each has the open issues by
// number, with their labels; all are the agent's. The desk's are a copy,
// which a refresh brings up to date for one issue.
type fakes struct {
	t            *testing.T
	github, desk map[int][]string
	pulls        string    // the desk's open pull requests, as it gives them
	asOf         time.Time // how old the desk says its copy is
	noRead       bool      // the desk answers a survey with 503
	hang         bool      // the desk takes each request and does not answer
	failRefresh  []int     // the refreshes, counted from 1, that the desk answers with 502
	stopAt       int       // the refresh, counted from 1, at which the worker is told to stop; the desk does not answer it
	stop         func()    // tells the worker to stop
	down         bool      // GitHub answers a survey with 502
	other        *claims   // another worker's view of the claims
	ran          string    // the file to which each job adds its prompt

	calls     []string // the requests to the desk; a refresh with the claims held when it came
	refreshes int
	surveys   int      // the surveys of GitHub
	early     int      // those of them from before any job ran
	comments  []string // the comments made on GitHub
	parked    []int    // the items labelled on GitHub
}

// newFakes returns fakes with the given open issues, which the desk has as
// GitHub does, and a worker on them whose config names the desk. Its work
// and triage jobs add their prompts, such as "work 12", to f.ran.
func newFakes(t *testing.T, issues ...int) (*fakes, *worker) {
	a, b := testClaims(t)
	dir := t.TempDir()
	f := &fakes{t: t, github: map[int][]string{}, desk: map[int][]string{}, asOf: time.Now(), other: b, ran: filepath.Join(dir, "ran")}
	for _, n := range issues {
		f.github[n], f.desk[n] = nil, nil
	}
	gh := httptest.NewServer(http.HandlerFunc(f.serveGitHub))
	t.Cleanup(gh.Close)
	dk := httptest.NewServer(http.HandlerFunc(f.serveDesk))
	t.Cleanup(dk.Close)
	jobs := map[string]Job{}
	for _, job := range []string{"work", "triage"} {
		prompt := filepath.Join(dir, job+".md")
		if err := os.WriteFile(prompt, []byte(job+" {{.Number}}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		jobs[job] = Job{Prompt: prompt, Command: []string{"sh", "-c", `cat "$1" >> ` + f.ran, "sh", "{prompt}"}, Timeout: duration{time.Minute}}
	}
	c := &Config{
		Repo: "o/r", Name: "a", Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]", IdleRounds: 3, MaxRounds: 20,
		Jobs: jobs, Desk: &Desk{URL: dk.URL, Token: "desk-token"},
	}
	g := &github{api: gh.URL, client: gh.Client(), token: "t", expires: time.Now().Add(time.Hour)}
	return f, &worker{c: c, gh: g, desk: newDesk(c), claims: a}
}

// fakeIssues gives the issues of one side in its form.
func fakeIssues(side map[int][]string, form, label string) string {
	var out []string
	for _, n := range slices.Sorted(maps.Keys(side)) {
		var labels []string
		for _, l := range side[n] {
			labels = append(labels, fmt.Sprintf(label, l))
		}
		out = append(out, fmt.Sprintf(form, n, strings.Join(labels, ", ")))
	}
	return strings.Join(out, ", ")
}

func (f *fakes) serveGitHub(w http.ResponseWriter, r *http.Request) {
	var q struct{ Query, Body string }
	json.NewDecoder(r.Body).Decode(&q)
	var number int
	on := func(what string) bool {
		_, err := fmt.Sscanf(r.URL.Path, "/repos/o/r/issues/%d/"+what, &number)
		return err == nil && r.Method == http.MethodPost
	}
	switch {
	case r.URL.Path == "/rate_limit":
		fmt.Fprint(w, `{"resources": {"graphql": {"remaining": 5000}, "core": {"remaining": 5000}}}`)
	case strings.Contains(q.Query, "issues(states: OPEN"):
		f.surveys++
		if f.jobs() == "" {
			f.early++
		}
		if f.down {
			http.Error(w, `{"message": "GitHub is down"}`, http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, `{"data": {"repository": {"issues": {"nodes": [%s]}}}}`, fakeIssues(f.github,
			`{"id": "I_%[1]d", "number": %[1]d, "title": "t", "createdAt": "2026-11-24T12:00:00Z", "author": {"__typename": "Bot", "login": "fc-agent"}, "labels": {"nodes": [%[2]s]}}`,
			`{"name": %q}`))
	case strings.Contains(q.Query, "pullRequests(states: OPEN"):
		fmt.Fprint(w, `{"data": {"repository": {"pullRequests": {"nodes": []}}}}`)
	case strings.Contains(q.Query, "comments(last: 100)"):
		fmt.Fprint(w, `{"data": {"repository": {"issueOrPullRequest": {"comments": {"nodes": []}}}}}`)
	case on("comments"):
		f.comments = append(f.comments, fmt.Sprintf("#%d: %s", number, q.Body))
		fmt.Fprint(w, `{}`)
	case on("labels"):
		f.parked = append(f.parked, number)
		fmt.Fprint(w, `[]`)
	default:
		f.t.Errorf("%s %s %q: not a request that these tests expect of a worker", r.Method, r.URL.Path, q.Query)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}
}

func (f *fakes) serveDesk(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Header.Get("Authorization") != "Bearer desk-token":
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case f.hang:
		// The server sees that the worker gave up only once the body is
		// read.
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	case r.Method == http.MethodGet && r.URL.Path == "/survey" && r.URL.Query().Get("repo") == "o/r":
		f.calls = append(f.calls, "survey")
		if f.noRead {
			http.Error(w, "no read of o/r has worked yet", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"repo": "o/r", "as_of": %q, "issues": [%s], "pulls": [%s]}`, f.asOf.UTC().Format(time.RFC3339), fakeIssues(f.desk,
			`{"id": "I_%[1]d", "number": %[1]d, "title": "t", "created_at": "2026-11-24T12:00:00Z", "author": {"type": "Bot", "login": "fc-agent"}, "labels": [%[2]s]}`,
			`%q`), f.pulls)
	case r.Method == http.MethodPost && r.URL.Path == "/refresh":
		var q struct {
			Repo   string
			Number int
		}
		json.NewDecoder(r.Body).Decode(&q)
		held, _ := f.claimed()
		f.calls = append(f.calls, strings.TrimSpace(fmt.Sprintf("refresh %d %s", q.Number, strings.Join(held, " "))))
		f.refreshes++
		if f.refreshes == f.stopAt {
			f.stop()
			io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		if q.Repo != "o/r" || slices.Contains(f.failRefresh, f.refreshes) {
			http.Error(w, fmt.Sprintf("reading #%d from GitHub failed: GitHub is down", q.Number), http.StatusBadGateway)
			return
		}
		state := "OPEN"
		if labels, ok := f.github[q.Number]; ok {
			f.desk[q.Number] = labels
		} else if _, ok := f.desk[q.Number]; ok {
			delete(f.desk, q.Number)
			state = "CLOSED"
		}
		fmt.Fprintf(w, `{"repo": "o/r", "number": %d, "state": %q, "issue": null, "pull": null}`, q.Number, state)
	default:
		f.t.Errorf("%s %s: not a request that a worker makes of a desk", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

// claimed returns the keys of the claims that are held, and of those that
// were released.
func (f *fakes) claimed() (held, released []string) {
	all, err := f.other.fetch(context.Background())
	if err != nil {
		f.t.Error(err)
	}
	for _, key := range slices.Sorted(maps.Keys(all)) {
		if time.Now().Before(all[key].Lease.Expires) {
			held = append(held, key)
		} else {
			released = append(released, key)
		}
	}
	return held, released
}

// jobs returns the prompts of the jobs that ran, such as "work 12\n".
func (f *fakes) jobs() string {
	data, _ := os.ReadFile(f.ran)
	return string(data)
}

// logged returns what the worker logs from now on, a line for each.
func logged(t *testing.T) *bytes.Buffer {
	var b bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&b)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	})
	return &b
}

// A survey is the desk's. It is GitHub's, with one line in the log that
// says why, if the desk does not answer or its answer is old.
func TestSurveyFallback(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	for _, tc := range []struct {
		name     string
		set      func(f *fakes, w *worker)
		fromDesk bool
		why      string // what the log says, or "" if it says nothing
	}{
		{"the desk answers", func(f *fakes, w *worker) {}, true, ""},
		{"an answer of four minutes ago", func(f *fakes, w *worker) { f.asOf = time.Now().Add(-4 * time.Minute) }, true, ""},
		{"an answer of six minutes ago", func(f *fakes, w *worker) { f.asOf = time.Now().Add(-6 * time.Minute) }, false, "desk: its answer is 6m"},
		{"no read yet", func(f *fakes, w *worker) { f.noRead = true }, false, "desk: 503 Service Unavailable: no read of o/r has worked yet; surveying GitHub instead\n"},
		{"another token", func(f *fakes, w *worker) { w.desk.token = "wrong" }, false, "desk: 401 Unauthorized"},
		{"no desk there", func(f *fakes, w *worker) { w.desk.url = gone.URL }, false, "desk: Get "},
		{"no desk in the config", func(f *fakes, w *worker) { w.c.Desk, w.desk = nil, nil }, false, ""},
	} {
		f, w := newFakes(t, 12, 13)
		tc.set(f, w)
		out := logged(t)
		items, fromDesk, err := w.survey(context.Background())
		if err != nil || len(items) != 2 || items[0].Number != 12 || items[1].Number != 13 {
			t.Errorf("%s: items %+v, error %v", tc.name, items, err)
		}
		direct := 1
		if tc.fromDesk {
			direct = 0
		}
		if fromDesk != tc.fromDesk || f.surveys != direct {
			t.Errorf("%s: from the desk %v, with %d surveys of GitHub; want %v with %d", tc.name, fromDesk, f.surveys, tc.fromDesk, direct)
		}
		lines := 0
		if tc.why != "" {
			lines = 1
		}
		if got := out.String(); strings.Count(got, "\n") != lines || !strings.Contains(got, tc.why) ||
			strings.Count(got, "; surveying GitHub instead\n") != lines {
			t.Errorf("%s: logged %q, want %d line with %q", tc.name, got, lines, tc.why)
		}
		if w.desk == nil && len(f.calls) != 0 {
			t.Errorf("%s: asked the desk: %v", tc.name, f.calls)
		}
	}
}

// With a desk, a step surveys from it, looks at the item again once it is
// claimed, and has the desk read the item before the round is recorded and
// before the claim is released.
func TestStepDesk(t *testing.T) {
	f, w := newFakes(t, 12)
	// The desk has not yet seen the label.
	f.github[12] = []string{"lemma"}
	out := logged(t)
	ctx := context.Background()
	if found, err := w.step(ctx); !found || err != nil {
		t.Fatalf("found %v, error %v\n%s", found, err, out)
	}
	// The claim is still held at each refresh.
	want := []string{"survey", "refresh 12 issue-12", "survey", "refresh 12 issue-12", "survey", "refresh 12 issue-12"}
	if !slices.Equal(f.calls, want) || f.surveys != 0 {
		t.Errorf("asked the desk %q and surveyed GitHub %d times, want %q and 0", f.calls, f.surveys, want)
	}
	if got := f.jobs(); got != "work 12\n" {
		t.Errorf("jobs %q", got)
	}
	if held, released := f.claimed(); len(held) != 0 || !slices.Equal(released, []string{"issue-12"}) {
		t.Errorf("claims held %v, released %v", held, released)
	}
	// The job began with the item as the desk had it after the claim, with
	// the label, so the round changed nothing.
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "#12: "+idleMarker) {
		t.Errorf("comments %q, want a round that changed nothing", f.comments)
	}
	if strings.Contains(out.String(), "GitHub instead") {
		t.Errorf("logged %s", out)
	}

	// -list asks the desk too.
	var list bytes.Buffer
	if err := w.list(ctx, &list); err != nil || !strings.Contains(list.String(), "issue-12") {
		t.Errorf("list: %v\n%s", err, &list)
	}
	if want = append(want, "survey"); !slices.Equal(f.calls, want) || f.surveys != 0 {
		t.Errorf("after -list, asked the desk %q and surveyed GitHub %d times, want %q and 0", f.calls, f.surveys, want)
	}
}

// The desk's copy is behind: of four issues that it shows as ready, one was
// parked, one closed and one proposed meanwhile. The worker gives each of
// their claims back and works on the fourth.
func TestStepDeskGivesBack(t *testing.T) {
	f, w := newFakes(t, 11, 12, 13, 14)
	f.github[11] = []string{"needs-human"}
	delete(f.github, 12)
	f.github[13] = []string{"proposed"}
	out := logged(t)
	if found, err := w.step(context.Background()); !found || err != nil {
		t.Fatalf("found %v, error %v\n%s", found, err, out)
	}
	for _, n := range []int{11, 12, 13} {
		if line := fmt.Sprintf("issue-%d: no longer needs work; claim given back\n", n); !strings.Contains(out.String(), line) {
			t.Errorf("the log lacks %q:\n%s", line, out)
		}
	}
	if got := f.jobs(); got != "work 14\n" {
		t.Errorf("jobs %q, want only work on 14", got)
	}
	if held, released := f.claimed(); len(held) != 0 || !slices.Equal(released, []string{"issue-11", "issue-12", "issue-13", "issue-14"}) {
		t.Errorf("claims held %v, released %v", held, released)
	}
	// Each refresh came while its item's claim was the only one held.
	want := []string{"survey", "refresh 11 issue-11", "survey", "refresh 12 issue-12", "survey", "refresh 13 issue-13", "survey",
		"refresh 14 issue-14", "survey", "refresh 14 issue-14", "survey", "refresh 14 issue-14"}
	if !slices.Equal(f.calls, want) || f.surveys != 0 {
		t.Errorf("asked the desk %q and surveyed GitHub %d times, want %q and 0", f.calls, f.surveys, want)
	}
}

// If the desk cannot read a claimed item again, GitHub says whether it
// still needs the job.
func TestStepDeskRefreshFails(t *testing.T) {
	f, w := newFakes(t, 12, 13)
	f.github[12] = []string{"needs-human"}
	f.github[13] = []string{"lemma"}
	f.failRefresh = []int{1, 2}
	out := logged(t)
	if found, err := w.step(context.Background()); !found || err != nil {
		t.Fatalf("found %v, error %v\n%s", found, err, out)
	}
	// One survey of GitHub for each refresh that failed. The first shows
	// that 12 needs no work and that 13 does, so the worker does not look
	// at 13 again once it has claimed it. The second is for the round.
	want := []string{"survey", "refresh 12 issue-12", "refresh 13 issue-13", "refresh 13 issue-13"}
	if !slices.Equal(f.calls, want) || f.surveys != 2 || f.early != 1 {
		t.Errorf("asked the desk %q and surveyed GitHub %d times, %d of them before the job; want %q, 2 and 1", f.calls, f.surveys, f.early, want)
	}
	for _, line := range []string{
		"issue-12: refresh: desk: 502 Bad Gateway: reading #12 from GitHub failed: GitHub is down; surveying GitHub instead\n",
		"issue-12: no longer needs work; claim given back\n",
		"issue-13: refresh: desk: 502 Bad Gateway: reading #13 from GitHub failed: GitHub is down; surveying GitHub instead\n",
	} {
		if strings.Count(out.String(), line) != 1 {
			t.Errorf("the log does not have %q once:\n%s", line, out)
		}
	}
	// The job on 13 began with the item as GitHub had it, with the label.
	if got := f.jobs(); got != "work 13\n" || len(f.comments) != 1 || !strings.Contains(f.comments[0], "#13: "+idleMarker) {
		t.Errorf("jobs %q, comments %q; want work on 13 that changed nothing", got, f.comments)
	}
	if held, released := f.claimed(); len(held) != 0 || !slices.Equal(released, []string{"issue-12", "issue-13"}) {
		t.Errorf("claims held %v, released %v", held, released)
	}

	// An item that GitHub shows as the desk did is worked on.
	f, w = newFakes(t, 12)
	f.failRefresh = []int{1}
	if found, err := w.step(context.Background()); !found || err != nil {
		t.Errorf("found %v, error %v", found, err)
	}
	want = []string{"survey", "refresh 12 issue-12", "refresh 12 issue-12", "survey", "refresh 12 issue-12"}
	if !slices.Equal(f.calls, want) || f.surveys != 1 || f.jobs() != "work 12\n" {
		t.Errorf("asked the desk %q, surveyed GitHub %d times and ran %q; want %q, 1 and work on 12", f.calls, f.surveys, f.jobs(), want)
	}

	// With GitHub down too, the worker gives the claim back and does no
	// job.
	f, w = newFakes(t, 12)
	f.failRefresh, f.down = []int{1}, true
	found, err := w.step(context.Background())
	if found || err == nil || !strings.Contains(err.Error(), "GitHub is down") {
		t.Errorf("found %v, error %v; want GitHub's error", found, err)
	}
	if held, released := f.claimed(); len(held) != 0 || !slices.Equal(released, []string{"issue-12"}) || f.jobs() != "" || f.surveys != 1 {
		t.Errorf("claims held %v, released %v, jobs %q, %d surveys of GitHub", held, released, f.jobs(), f.surveys)
	}

	// A refresh that fails before the release keeps nothing from the
	// release.
	f, w = newFakes(t, 12)
	f.failRefresh = []int{3}
	if found, err := w.step(context.Background()); !found || err != nil {
		t.Errorf("found %v, error %v", found, err)
	}
	if !strings.Contains(out.String(), "issue-12: refresh: desk: 502 Bad Gateway: reading #12 from GitHub failed: GitHub is down\n") {
		t.Errorf("the log does not say that the refresh failed:\n%s", out)
	}
	if held, released := f.claimed(); len(held) != 0 || !slices.Equal(released, []string{"issue-12"}) || f.refreshes != 3 || f.surveys != 0 {
		t.Errorf("claims held %v, released %v, %d refreshes, %d surveys of GitHub", held, released, f.refreshes, f.surveys)
	}
}

// After a refresh that failed, the step goes on with GitHub's survey: it
// does not claim each item that the desk's copy shows as ready only to
// survey GitHub again for it.
func TestStepDeskRefreshDown(t *testing.T) {
	f, w := newFakes(t, 11, 12, 13, 14)
	f.github[11] = []string{"needs-human"}
	delete(f.github, 12)
	f.github[13] = []string{"needs-human"}
	f.failRefresh = []int{1, 2, 3}
	out := logged(t)
	if found, err := w.step(context.Background()); !found || err != nil {
		t.Fatalf("found %v, error %v\n%s", found, err, out)
	}
	// One survey of GitHub before the job, after the claim on 11, and one
	// to record the round.
	want := []string{"survey", "refresh 11 issue-11", "refresh 14 issue-14", "refresh 14 issue-14"}
	if !slices.Equal(f.calls, want) || f.surveys != 2 || f.early != 1 {
		t.Errorf("asked the desk %q and surveyed GitHub %d times, %d of them before the job; want %q, 2 and 1", f.calls, f.surveys, f.early, want)
	}
	if held, released := f.claimed(); len(held) != 0 || !slices.Equal(released, []string{"issue-11", "issue-14"}) || f.jobs() != "work 14\n" {
		t.Errorf("claims held %v, released %v, jobs %q; want claims on 11 and 14 only, and work on 14", held, released, f.jobs())
	}

	// 11 is given back since it needs another job now. GitHub's survey,
	// made for 12, shows that too, but the step does not claim 11 again.
	f, w = newFakes(t, 11, 12, 13)
	f.github[11] = []string{"proposed"}
	f.github[12] = []string{"needs-human"}
	f.failRefresh = []int{2}
	if found, err := w.step(context.Background()); !found || err != nil {
		t.Fatalf("found %v, error %v\n%s", found, err, out)
	}
	want = []string{"survey", "refresh 11 issue-11", "survey", "refresh 12 issue-12", "refresh 13 issue-13", "survey", "refresh 13 issue-13"}
	if !slices.Equal(f.calls, want) || f.surveys != 1 || f.jobs() != "work 13\n" {
		t.Errorf("asked the desk %q, surveyed GitHub %d times and ran %q; want %q, 1 and work on 13", f.calls, f.surveys, f.jobs(), want)
	}
	if strings.Contains(out.String(), "not claimed") {
		t.Errorf("tried a claim that it could not take:\n%s", out)
	}
}

// A desk that takes a request and does not answer holds the worker up for
// deskLimit, and then the survey is GitHub's.
func TestDeskHangs(t *testing.T) {
	t.Cleanup(func(d time.Duration) func() { return func() { deskLimit = d } }(deskLimit))
	deskLimit = 50 * time.Millisecond
	f, w := newFakes(t, 12)
	f.hang = true
	out := logged(t)
	ctx := context.Background()
	start := time.Now()
	items, fromDesk, err := w.survey(ctx)
	if err != nil || len(items) != 1 || fromDesk || f.surveys != 1 {
		t.Errorf("items %+v, from the desk %v, error %v, with %d surveys of GitHub", items, fromDesk, err, f.surveys)
	}
	if got := out.String(); !strings.HasPrefix(got, "desk: Get ") || strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "; surveying GitHub instead\n") {
		t.Errorf("logged %q", got)
	}
	// So is the survey after a refresh.
	items, fromDesk, err = w.fresh(ctx, candidate{Item: Item{Number: 12}})
	if err != nil || len(items) != 1 || fromDesk || f.surveys != 2 {
		t.Errorf("after a refresh: items %+v, from the desk %v, error %v, with %d surveys of GitHub", items, fromDesk, err, f.surveys)
	}
	if !strings.Contains(out.String(), "issue-12: refresh: desk: Post ") {
		t.Errorf("logged %s", out)
	}
	// Each of the two waited for deskLimit and no longer.
	if took := time.Since(start); took < 2*deskLimit || took > 5*time.Second {
		t.Errorf("the two took %s with a limit of %s each", took, deskLimit)
	}
}

// A worker that is told to stop during its job has the desk read the item
// and releases the claim all the same.
func TestStepDeskStopsInJob(t *testing.T) {
	f, w := newFakes(t, 12)
	job := w.c.Jobs["work"]
	job.Command = []string{"sh", "-c", `cat "$1" >> ` + f.ran + `; exec sleep 30`, "sh", "{prompt}"}
	w.c.Jobs["work"] = job
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for f.jobs() == "" && ctx.Err() == nil {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	found, err := w.step(ctx)
	if !found || err == nil || !strings.Contains(err.Error(), "issue-12: work: ") {
		t.Errorf("found %v, error %v; want the error of a job that was stopped", found, err)
	}
	// No round is recorded, but the claim is still held at the last
	// refresh.
	want := []string{"survey", "refresh 12 issue-12", "survey", "refresh 12 issue-12"}
	if !slices.Equal(f.calls, want) || f.surveys != 0 || len(f.comments) != 0 {
		t.Errorf("asked the desk %q, surveyed GitHub %d times, comments %q; want %q, 0 and none", f.calls, f.surveys, f.comments, want)
	}
	if held, released := f.claimed(); len(held) != 0 || !slices.Equal(released, []string{"issue-12"}) || f.jobs() != "work 12\n" {
		t.Errorf("claims held %v, released %v, jobs %q", held, released, f.jobs())
	}
}

// A worker that is told to stop while the desk reads the item that it just
// claimed gives the claim back, and neither surveys GitHub nor runs a job.
func TestStepDeskStopsInRefresh(t *testing.T) {
	f, w := newFakes(t, 12)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.stopAt, f.stop = 1, cancel
	out := logged(t)
	found, err := w.step(ctx)
	if found || !errors.Is(err, context.Canceled) {
		t.Errorf("found %v, error %v; want the error of a request that was stopped", found, err)
	}
	want := []string{"survey", "refresh 12 issue-12"}
	if !slices.Equal(f.calls, want) || f.surveys != 0 || f.jobs() != "" {
		t.Errorf("asked the desk %q, surveyed GitHub %d times and ran %q; want %q, 0 and no job", f.calls, f.surveys, f.jobs(), want)
	}
	if held, released := f.claimed(); len(held) != 0 || !slices.Equal(released, []string{"issue-12"}) {
		t.Errorf("claims held %v, released %v", held, released)
	}
	if strings.Contains(out.String(), "surveying GitHub instead") {
		t.Errorf("logged %s", out)
	}
}

// While the desk answers no survey, the worker surveys GitHub, whose facts
// need no second look after the claim. The desk still reads the item before
// the round is recorded and before the release.
func TestStepDeskDown(t *testing.T) {
	f, w := newFakes(t, 12)
	f.noRead = true
	out := logged(t)
	if found, err := w.step(context.Background()); !found || err != nil {
		t.Fatalf("found %v, error %v\n%s", found, err, out)
	}
	want := []string{"survey", "refresh 12 issue-12", "survey", "refresh 12 issue-12"}
	if !slices.Equal(f.calls, want) || f.surveys != 2 {
		t.Errorf("asked the desk %q and surveyed GitHub %d times, want %q and 2", f.calls, f.surveys, want)
	}
	if n := strings.Count(out.String(), "; surveying GitHub instead\n"); n != 2 {
		t.Errorf("the log says %d times that it surveys GitHub, want once for each survey:\n%s", n, out)
	}
	if held, released := f.claimed(); len(held) != 0 || !slices.Equal(released, []string{"issue-12"}) || f.jobs() != "work 12\n" {
		t.Errorf("claims held %v, released %v, jobs %q", held, released, f.jobs())
	}
}

// Without a desk in the config, the worker asks none, and surveys GitHub as
// before: once to pick and once to record the round.
func TestStepNoDesk(t *testing.T) {
	f, w := newFakes(t, 12)
	w.c.Desk, w.desk = nil, nil
	ctx := context.Background()
	if found, err := w.step(ctx); !found || err != nil {
		t.Fatalf("found %v, error %v", found, err)
	}
	if err := w.list(ctx, io.Discard); err != nil {
		t.Error(err)
	}
	if len(f.calls) != 0 || f.surveys != 3 {
		t.Errorf("asked the desk %q and surveyed GitHub %d times, want no request and 3", f.calls, f.surveys)
	}
	if held, released := f.claimed(); len(held) != 0 || !slices.Equal(released, []string{"issue-12"}) || f.jobs() != "work 12\n" || len(f.comments) != 1 {
		t.Errorf("claims held %v, released %v, jobs %q, comments %q", held, released, f.jobs(), f.comments)
	}
}

// The desk reads the item before the release also after a job that the
// worker does by itself.
func TestStepDeskPark(t *testing.T) {
	f, w := newFakes(t)
	// Approved by the reviewer, but GitHub still requires a review.
	f.pulls = `{"id": "PR_30", "number": 30, "title": "t", "created_at": "2026-11-24T12:00:00Z", "author": {"type": "Bot", "login": "fc-agent"},
		"labels": [], "head": "aaa", "mergeable": "MERGEABLE", "checks": "SUCCESS", "review_decision": "REVIEW_REQUIRED",
		"reviews": [{"author": {"type": "Bot", "login": "fc-reviewer"}, "state": "APPROVED", "submitted_at": "2026-11-24T13:00:00Z", "commit": "aaa"}]}`
	if found, err := w.step(context.Background()); !found || err != nil {
		t.Fatalf("found %v, error %v", found, err)
	}
	want := []string{"survey", "refresh 30 pr-30", "survey", "refresh 30 pr-30"}
	if !slices.Equal(f.calls, want) || f.surveys != 0 {
		t.Errorf("asked the desk %q and surveyed GitHub %d times, want %q and 0", f.calls, f.surveys, want)
	}
	if held, released := f.claimed(); len(held) != 0 || !slices.Equal(released, []string{"pr-30"}) || !slices.Equal(f.parked, []int{30}) {
		t.Errorf("claims held %v, released %v, parked %v", held, released, f.parked)
	}
}

// record looks at the item as it is after the job: the desk reads it again
// first, and if it cannot, GitHub is surveyed.
func TestRecordDesk(t *testing.T) {
	for _, tc := range []struct {
		name    string
		set     func(f *fakes, w *worker)
		calls   []string
		surveys int
	}{
		{"the desk reads it again", func(f *fakes, w *worker) {}, []string{"refresh 12", "survey"}, 0},
		{"the refresh fails", func(f *fakes, w *worker) { f.failRefresh = []int{1} }, []string{"refresh 12"}, 1},
		{"the desk answers no survey", func(f *fakes, w *worker) { f.noRead = true }, []string{"refresh 12", "survey"}, 1},
		{"no desk in the config", func(f *fakes, w *worker) { w.c.Desk, w.desk = nil, nil }, nil, 1},
	} {
		f, w := newFakes(t, 12)
		tc.set(f, w)
		// The job added a label, which the desk has not yet seen.
		f.github[12] = []string{"proposed"}
		cand := candidate{Item{ID: "I_12", Number: 12, Title: "t"}, "work", "ready"}
		if err := w.record(context.Background(), cand, "", nil); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if len(f.comments) != 1 || !strings.Contains(f.comments[0], "#12: "+roundMarker) || !strings.Contains(f.comments[0], "It changed something.") {
			t.Errorf("%s: comments %q, want a round that changed something", tc.name, f.comments)
		}
		if !slices.Equal(f.calls, tc.calls) || f.surveys != tc.surveys {
			t.Errorf("%s: asked the desk %q and surveyed GitHub %d times, want %q and %d", tc.name, f.calls, f.surveys, tc.calls, tc.surveys)
		}
	}
}
