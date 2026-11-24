package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	c := &Config{Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]"}
	// ours makes it a pull request that the agent App opened, with checks
	// passing unless the case says otherwise.
	ours := func(it Item) Item {
		it.PR, it.Ours = true, true
		if it.Checks == "" {
			it.Checks = "SUCCESS"
		}
		return it
	}
	for _, tc := range []struct {
		name string
		it   Item
		want string
	}{
		{"ready issue", Item{}, "work"},
		{"parked issue", Item{Labels: []string{"needs-human", "proposed"}}, ""},
		{"proposed", Item{Labels: []string{"proposed"}, OpenBlockers: 1}, "triage"},
		{"close-proposed", Item{Labels: []string{"close-proposed"}}, "triage"},
		{"blocked", Item{OpenBlockers: 1}, ""},
		{"has sub-issues", Item{OpenSubIssues: 2}, ""},
		{"in review", Item{ClosingPRs: 1}, ""},
		{"outside issue", Item{Outside: true}, ""},
		{"outside PR", Item{PR: true, Outside: true, Checks: "SUCCESS"}, ""},

		{"draft", Item{PR: true, Draft: true, Ours: true, Conflict: true, Review: "CHANGES_REQUESTED"}, "continue"},
		{"parked draft", Item{PR: true, Draft: true, Ours: true, Labels: []string{"needs-human"}}, ""},
		{"a person's draft", Item{PR: true, Draft: true, Checks: "SUCCESS"}, ""},
		{"outside draft", Item{PR: true, Draft: true, Outside: true, Checks: "SUCCESS"}, ""},
		{"parked PR", ours(Item{Labels: []string{"needs-human"}, Conflict: true}), ""},
		{"conflict", ours(Item{Conflict: true}), "fix"},
		{"checks failed", ours(Item{Checks: "FAILURE"}), "fix"},
		{"checks errored", ours(Item{Checks: "ERROR"}), "fix"},
		{"changes requested", ours(Item{Review: "CHANGES_REQUESTED"}), "fix"},
		{"changes requested, checks running", ours(Item{Review: "CHANGES_REQUESTED", Checks: "PENDING"}), "fix"},
		{"a person requested changes", ours(Item{Requested: true, Review: "APPROVED"}), "fix"},
		{"person's PR, a person requested changes", Item{PR: true, Checks: "SUCCESS", Requested: true}, ""},
		{"conflict after approval", ours(Item{Conflict: true, Review: "APPROVED"}), "fix"},
		{"removed from queue once", ours(Item{Review: "APPROVED", Removals: 1}), autoMerge},
		{"removed once, back in the queue", ours(Item{Review: "APPROVED", Removals: 1, Queued: true}), ""},
		{"removed once, auto-merge on", ours(Item{Review: "APPROVED", Removals: 1, AutoMerge: true}), ""},
		{"removed from queue twice", ours(Item{Review: "APPROVED", Removals: 2}), "fix"},
		{"removed twice, back in the queue", ours(Item{Review: "APPROVED", Removals: 2, Queued: true}), ""},
		{"removed once, approval does not count", ours(Item{Review: "APPROVED", Removals: 1, Decision: "REVIEW_REQUIRED"}), park},
		{"checks running", ours(Item{Checks: "PENDING"}), ""},
		{"no checks yet", Item{PR: true, Ours: true}, ""},
		{"not reviewed", ours(Item{}), "review"},
		{"approved", ours(Item{Review: "APPROVED"}), autoMerge},
		{"approved, auto-merge on", ours(Item{Review: "APPROVED", AutoMerge: true}), ""},
		{"approved, queued", ours(Item{Review: "APPROVED", Queued: true}), ""},
		{"approved, blocked by a person", ours(Item{Review: "APPROVED", Decision: "CHANGES_REQUESTED"}), park},
		{"approved, approval does not count", ours(Item{Review: "APPROVED", Decision: "REVIEW_REQUIRED"}), park},
		{"approved, auto-merge on, approval does not count", ours(Item{Review: "APPROVED", Decision: "REVIEW_REQUIRED", AutoMerge: true}), park},
		{"approved, approval counts", ours(Item{Review: "APPROVED", Decision: "APPROVED"}), autoMerge},
		{"approved, queued, approval does not count", ours(Item{Review: "APPROVED", Decision: "REVIEW_REQUIRED", Queued: true}), park},
		{"mergeability unknown, approved", ours(Item{MergeUnknown: true, Review: "APPROVED"}), ""},
		{"mergeability unknown, not reviewed", ours(Item{MergeUnknown: true}), ""},
		{"mergeability unknown, checks failed", ours(Item{MergeUnknown: true, Checks: "FAILURE"}), "fix"},

		{"person's PR, not reviewed", Item{PR: true, Checks: "SUCCESS"}, "review"},
		{"person's PR, conflict", Item{PR: true, Conflict: true}, ""},
		{"person's PR, changes requested", Item{PR: true, Checks: "SUCCESS", Review: "CHANGES_REQUESTED"}, ""},
		{"person's PR, approved", Item{PR: true, Checks: "SUCCESS", Review: "APPROVED"}, ""},
	} {
		if got, why := next(tc.it, c, 0); got != tc.want {
			t.Errorf("%s: got %q (%s), want %q", tc.name, got, why, tc.want)
		}
	}
}

// TestNextMergeFirst checks what waits while pull request 7 merges first:
// only the auto-merge of the others.
func TestNextMergeFirst(t *testing.T) {
	c := &Config{Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]"}
	pr := func(number int, it Item) Item {
		it.PR, it.Ours, it.Number, it.Checks = true, true, number, "SUCCESS"
		return it
	}
	const waits = "approved; waiting until #7, which merges first, has merged"
	for _, tc := range []struct {
		name string
		it   Item
		want string
		why  string // if not "", the reason wanted
	}{
		{"it is approved", pr(7, Item{Review: "APPROVED"}), autoMerge, "approved"},
		{"it left the queue once", pr(7, Item{Review: "APPROVED", Removals: 1}), autoMerge, ""},
		{"another is approved", pr(8, Item{Review: "APPROVED"}), "", waits},
		{"another left the queue once", pr(8, Item{Review: "APPROVED", Removals: 1}), "", waits},
		{"another left the queue twice", pr(8, Item{Review: "APPROVED", Removals: 2}), "fix", ""},
		{"another is in the queue", pr(8, Item{Review: "APPROVED", Queued: true}), "", "approved, waiting for the merge queue"},
		{"another has auto-merge on", pr(8, Item{Review: "APPROVED", AutoMerge: true}), "", "approved, waiting for the merge queue"},
		{"another's approval does not count", pr(8, Item{Review: "APPROVED", Decision: "REVIEW_REQUIRED"}), park, ""},
		{"another is not reviewed", pr(8, Item{}), "review", ""},
		{"another has a conflict", pr(8, Item{Conflict: true}), "fix", ""},
		{"an issue", Item{Number: 9}, "work", ""},
	} {
		if got, why := next(tc.it, c, 7); got != tc.want || tc.why != "" && why != tc.why {
			t.Errorf("%s: got %q (%s), want %q (%s)", tc.name, got, why, tc.want, tc.why)
		}
	}
}

func TestFirst(t *testing.T) {
	c := &Config{MergeFirstLimit: duration{4 * time.Hour}}
	now := time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)
	held := Item{Number: 7, PR: true, Labels: []string{mergeFirst}, FirstSince: since}
	closes := Item{Number: 7, PR: true, FirstIssue: 5, FirstSince: since}
	with := func(f func(*Item)) Item {
		it := held
		f(&it)
		return it
	}
	for _, tc := range []struct {
		name  string
		items []Item
		want  int
	}{
		{"none", []Item{{Number: 1}, {Number: 2, PR: true}}, 0},
		{"labelled", []Item{{Number: 1}, {Number: 2, PR: true}, held}, 7},
		{"the label does not count", []Item{with(func(it *Item) { it.FirstSince = time.Time{} })}, 0},
		{"labelled too long ago", []Item{with(func(it *Item) { it.FirstSince = now.Add(-4 * time.Hour) })}, 0},
		{"draft", []Item{with(func(it *Item) { it.Draft = true })}, 0},
		{"parked", []Item{with(func(it *Item) { it.Labels = []string{mergeFirst, "needs-human"} })}, 0},
		{"from outside", []Item{with(func(it *Item) { it.Outside = true })}, 0},
		{"two: the older", []Item{held, with(func(it *Item) { it.Number = 9 })}, 7},
		{"two: the older is a draft", []Item{with(func(it *Item) { it.Draft = true }), with(func(it *Item) { it.Number = 9 })}, 9},
		{"an issue whose label counts", []Item{{Number: 5, Labels: []string{mergeFirst}, FirstSince: since}}, 0},
		{"it closes a labelled issue", []Item{{Number: 5, Labels: []string{mergeFirst}, FirstSince: since}, closes}, 7},
		{"it closes a labelled issue, as a draft", []Item{with(func(it *Item) { *it = closes; it.Draft = true })}, 0},
	} {
		if got := first(tc.items, c, now); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}

	no := "labelled merge-first, which does not count: "
	for _, tc := range []struct {
		name string
		it   Item
		f    int
		want string
	}{
		{"an issue", Item{Number: 1}, 7, ""},
		{"not labelled", Item{Number: 2, PR: true}, 7, ""},
		{"it merges first", held, 7, "merges first until 23:00 UTC"},
		{"another goes before it", with(func(it *Item) { it.Number = 9 }), 7, "labelled merge-first; #7 goes before it"},
		{"the label does not count", with(func(it *Item) { it.FirstSince = time.Time{} }), 0, no + "no maintainer in the config added it, or 20 events came since"},
		{"labelled too long ago", with(func(it *Item) { it.FirstSince = now.Add(-4 * time.Hour) }), 0, "labelled merge-first, which counted until 20:00 UTC"},
		{"draft", with(func(it *Item) { it.Draft = true }), 0, no + "a draft"},
		{"from outside", with(func(it *Item) { it.Outside = true }), 0, no + "from outside the project"},
		{"parked", with(func(it *Item) { it.Labels = []string{mergeFirst, "needs-human"} }), 0, no + "parked"},
		{"an issue whose label counts", Item{Number: 5, Labels: []string{mergeFirst}, FirstSince: since}, 7, "labelled merge-first, which counts for a pull request that closes it"},
		{"an issue whose label does not count", Item{Number: 5, Labels: []string{mergeFirst}}, 0, "labelled merge-first: no open pull request closes it, or the label does not count"},
		{"it closes a labelled issue, ready too late", with(func(it *Item) { *it = closes; it.FirstSince = time.Time{} }), 0, "closes #5, labelled merge-first, which does not count: it was ready for review three days or more after the issue got the label"},
		{"it closes a labelled issue, with a label of its own", with(func(it *Item) { it.FirstIssue = 5; it.Number = 9 }), 7, "closes #5, labelled merge-first; #7 goes before it"},
		{"it closes a labelled issue", closes, 7, "merges first until 23:00 UTC"},
		{"it closes a labelled issue, as a draft", with(func(it *Item) { *it = closes; it.Draft = true }), 0, "closes #5, labelled merge-first, which does not count: a draft"},
		{"it closes a labelled issue, too long ago", with(func(it *Item) { *it = closes; it.FirstSince = now.Add(-5 * time.Hour) }), 0, "closes #5, labelled merge-first, which counted until 19:00 UTC"},
		{"it closes a labelled issue, another goes before it", with(func(it *Item) { *it = closes; it.Number = 9 }), 7, "closes #5, labelled merge-first; #7 goes before it"},
	} {
		if got := firstNote(tc.it, tc.f, c, now); got != tc.want {
			t.Errorf("note, %s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSurveyMergeFirst checks that survey reads from GitHub's answers who
// labelled a pull request or an issue merge-first, and when, and passes
// an issue's label on to the pull requests that close it.
func TestSurveyMergeFirst(t *testing.T) {
	const pr = `{"id": "PR_%[1]d", "number": %[1]d, "title": "t", "mergeable": "MERGEABLE", "createdAt": "2026-11-24T18:30:00Z",
		"author": {"__typename": "Bot", "login": "fc-agent"},
		"labels": {"nodes": [%[4]s]},
		"timelineItems": {"nodes": []},
		"events": {"nodes": [
			{"__typename": "LabeledEvent", "createdAt": "2026-11-24T18:00:00Z", "label": {"name": "other"}, "actor": {"__typename": "User", "login": "maint"}},
			{"__typename": "LabeledEvent", "createdAt": "2026-11-24T19:00:00Z", "label": {"name": "merge-first"}, "actor": {"__typename": %[2]q, "login": %[3]q}}%[5]s]}}`
	const label = `{"name": "merge-first"}`
	const issue = `{"id": "I_%[1]d", "number": %[1]d, "title": "t", "createdAt": %[2]q, "authorAssociation": "NONE",
		"author": {"__typename": "Bot", "login": %[3]q},
		"labels": {"nodes": [%[4]s]},
		"closedByPullRequestsReferences": {"nodes": [%[5]s]}}`
	const adding = `"i%d": {"events": {"nodes": [{"__typename": "LabeledEvent", "createdAt": %q, "label": {"name": "merge-first"}, "actor": {"__typename": %q, "login": %q}}]}}`
	refs := func(repo, state string, numbers ...int) string {
		var out []string
		for _, n := range numbers {
			out = append(out, fmt.Sprintf(`{"number": %d, "state": %q, "authorAssociation": "NONE", "author": {"__typename": "Bot", "login": "fc-agent"}, "repository": {"nameWithOwner": %q}}`, n, state, repo))
		}
		return strings.Join(out, ",")
	}
	var issues, addings string
	var asked []string // the queries about labelled issues
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		q := body.Query
		switch {
		case strings.Contains(q, "issue(number: "):
			asked = append(asked, q)
			fmt.Fprintf(w, `{"data": {"repository": {%s}}}`, addings)
		case !strings.Contains(q, "pullRequests("):
			if !strings.Contains(q, "id number title createdAt authorAssociation") ||
				!strings.Contains(q, "closedByPullRequestsReferences(first: 20) { nodes { number state mergedAt authorAssociation author { __typename login } repository { nameWithOwner } } }") {
				http.Error(w, "the query does not ask when an issue was opened and which pull requests close it", http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, `{"data": {"repository": {"issues": {"nodes": [%s]}}}}`, issues)
		case !strings.Contains(q, "id number title createdAt isDraft") ||
			!strings.Contains(q, "events: timelineItems(last: 20, itemTypes: [LABELED_EVENT, READY_FOR_REVIEW_EVENT, CONVERT_TO_DRAFT_EVENT]) {") ||
			!strings.Contains(q, "nodes { __typename ... on LabeledEvent { createdAt label { name } actor { __typename login } } ... on ReadyForReviewEvent { createdAt } ... on ConvertToDraftEvent { createdAt } }"):
			http.Error(w, "the query does not ask for the events", http.StatusBadRequest)
		default:
			fmt.Fprintf(w, `{"data": {"repository": {"pullRequests": {"nodes": [%s]}}}}`, strings.Join([]string{
				fmt.Sprintf(pr, 7, "User", "maint", label, ""), fmt.Sprintf(pr, 8, "Bot", "fc-agent", label, ""),
				fmt.Sprintf(pr, 9, "Bot", "fc-agent", "", ""),
				fmt.Sprintf(pr, 10, "Bot", "fc-agent", "", `, {"__typename": "ReadyForReviewEvent", "createdAt": "2026-11-24T19:30:00Z"}`),
				fmt.Sprintf(pr, 11, "Bot", "fc-agent", "", `, {"__typename": "ConvertToDraftEvent", "createdAt": "2026-11-24T19:10:00Z"}, {"__typename": "ReadyForReviewEvent", "createdAt": "2026-11-24T19:40:00Z"}`),
				fmt.Sprintf(pr, 12, "Bot", "fc-agent", "", ""), fmt.Sprintf(pr, 13, "Bot", "fc-agent", "", ""),
				fmt.Sprintf(pr, 14, "User", "maint", label, ""), fmt.Sprintf(pr, 15, "Bot", "fc-agent", "", ""),
				fmt.Sprintf(pr, 16, "Bot", "fc-agent", "", ""),
			}, ","))
		}
	}))
	defer srv.Close()
	g := &github{api: srv.URL, client: srv.Client(), token: "t", expires: time.Now().Add(time.Hour)}
	c := &Config{Repo: "o/r", Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]", Maintainers: []string{"maint"}, Apps: []string{"github-actions[bot]"}}
	at := func(hour, min, sec int) time.Time { return time.Date(2026, 11, 24, hour, min, sec, 0, time.UTC) }
	byNumber := func(items []Item) map[int]Item {
		m := map[int]Item{}
		for _, it := range items {
			m[it.Number] = it
		}
		return m
	}

	// No issue is labelled, so nothing is asked about issues.
	items, err := survey(context.Background(), g, c)
	if err != nil || len(items) != 10 || len(asked) != 0 {
		t.Fatalf("survey: %d items, asked %d times, %v", len(items), len(asked), err)
	}
	m := byNumber(items)
	if !m[7].FirstSince.Equal(at(19, 0, 0)) {
		t.Errorf("labelled by a maintainer: %v, want %v", m[7].FirstSince, at(19, 0, 0))
	}
	if !m[8].FirstSince.IsZero() {
		t.Errorf("labelled by the agent App: %v, want zero", m[8].FirstSince)
	}
	if !m[9].FirstSince.IsZero() || m[9].FirstIssue != 0 {
		t.Errorf("no labelled issue: %v, issue %d; want zero and 0", m[9].FirstSince, m[9].FirstIssue)
	}

	// A workflow opened issue 3 with the label at 17:00; 7, 9, 10 and 11
	// close it, as do a merged pull request and number 12 of another
	// repository. The labels of 4, 6 and 21 do not count: the agent App
	// added the first, the workflow added the second an hour after it
	// opened the issue, and the third is on the agent App's issue. 8
	// closes these. Nothing open closes 5. A maintainer labelled 22 at
	// 20:00, which 9 and 14 close; 23 more than three days before 13,
	// which closes it, was opened; and 24 less than three days before 16.
	opened := "2026-11-24T17:00:00Z"
	issues = strings.Join([]string{
		fmt.Sprintf(issue, 2, opened, "github-actions", `{"name": "other"}`, refs("o/r", "OPEN", 15)),
		fmt.Sprintf(issue, 3, opened, "github-actions", label, refs("o/r", "OPEN", 7, 9, 10, 11)+","+refs("o/r", "MERGED", 1)+","+refs("o/other", "OPEN", 12)),
		fmt.Sprintf(issue, 4, opened, "github-actions", label, refs("o/r", "OPEN", 8)),
		fmt.Sprintf(issue, 5, opened, "github-actions", label, refs("o/r", "CLOSED", 1)),
		fmt.Sprintf(issue, 6, opened, "github-actions", label, refs("o/r", "OPEN", 8)),
		fmt.Sprintf(issue, 21, opened, "fc-agent", label, refs("o/r", "OPEN", 8)),
		fmt.Sprintf(issue, 22, opened, "fc-agent", label, refs("o/r", "OPEN", 9, 14)),
		fmt.Sprintf(issue, 23, "2026-11-21T09:00:00Z", "fc-agent", label, refs("o/r", "OPEN", 13)),
		fmt.Sprintf(issue, 24, "2026-11-21T09:00:00Z", "fc-agent", label, refs("o/r", "OPEN", 16)),
	}, ",")
	addings = strings.Join([]string{
		fmt.Sprintf(adding, 3, "2026-11-24T17:00:02Z", "Bot", "github-actions"),
		fmt.Sprintf(adding, 4, "2026-11-24T17:00:02Z", "Bot", "fc-agent"),
		fmt.Sprintf(adding, 6, "2026-11-24T18:00:00Z", "Bot", "github-actions"),
		fmt.Sprintf(adding, 21, "2026-11-24T17:00:02Z", "Bot", "github-actions"),
		fmt.Sprintf(adding, 22, "2026-11-24T20:00:00Z", "User", "maint"),
		fmt.Sprintf(adding, 23, "2026-11-21T18:30:00Z", "User", "maint"),
		fmt.Sprintf(adding, 24, "2026-11-21T18:30:01Z", "User", "maint"),
	}, ",")
	items, err = survey(context.Background(), g, c)
	if err != nil || len(items) != 19 || len(asked) != 1 {
		t.Fatalf("survey with labelled issues: %d items, asked %d times, %v", len(items), len(asked), err)
	}
	var aliases []string
	for _, line := range strings.Split(asked[0], "\n") {
		if alias, rest, ok := strings.Cut(strings.TrimSpace(line), ": issue(number: "); ok {
			aliases = append(aliases, alias)
			if !strings.HasPrefix(rest, alias[1:]+") { events: timelineItems(last: 20, itemTypes: [LABELED_EVENT]) { nodes { __typename ... on LabeledEvent { createdAt label { name } actor { __typename login } } } } }") {
				t.Errorf("the query asks about %s with %q", alias, rest)
			}
		}
	}
	if want := []string{"i3", "i4", "i6", "i21", "i22", "i23", "i24"}; !slices.Equal(aliases, want) {
		t.Errorf("asked about %v, want %v", aliases, want)
	}
	m = byNumber(items)
	for _, tc := range []struct {
		name   string
		number int
		since  time.Time
		issue  int
	}{
		{"an issue with another label", 2, time.Time{}, 0},
		{"an issue that a workflow opened with the label", 3, at(17, 0, 2), 0},
		{"an issue labelled by the agent App", 4, time.Time{}, 0},
		{"a labelled issue that nothing open closes", 5, time.Time{}, 0},
		{"an issue that a workflow labelled after opening it", 6, time.Time{}, 0},
		{"the agent App's issue, labelled by a workflow", 21, time.Time{}, 0},
		{"an issue labelled by a maintainer", 22, at(20, 0, 0), 0},
		{"an issue labelled by a maintainer three days before", 23, at(18, 30, 0).AddDate(0, 0, -3), 0},
		{"an issue labelled by a maintainer a second less than three days before", 24, at(18, 30, 1).AddDate(0, 0, -3), 0},
		{"labelled by a maintainer after it was opened", 7, at(19, 0, 0), 3},
		{"it closes issues whose labels do not count", 8, time.Time{}, 0},
		{"it closes two labelled issues: the later label", 9, at(20, 0, 0), 22},
		{"opened as a draft, marked ready later", 10, at(19, 30, 0), 3},
		{"opened ready, a draft for a while", 11, at(18, 30, 0), 3},
		{"its number in another repository closes a labelled issue", 12, time.Time{}, 0},
		{"ready three days after the label", 13, time.Time{}, 23},
		{"ready a second less than three days after the label", 16, at(18, 30, 0), 24},
		{"its own label is older than that of the issue it closes", 14, at(20, 0, 0), 22},
		{"it closes an issue with another label", 15, time.Time{}, 0},
	} {
		if it := m[tc.number]; !it.FirstSince.Equal(tc.since) || it.FirstIssue != tc.issue {
			t.Errorf("%s (#%d): since %v, issue %d; want %v, %d", tc.name, tc.number, it.FirstSince, it.FirstIssue, tc.since, tc.issue)
		}
	}

	// It asks about ten issues at most, the oldest.
	var many []string
	for n := 31; n <= 42; n++ {
		many = append(many, fmt.Sprintf(issue, n, opened, "github-actions", label, refs("o/r", "OPEN", 9)))
	}
	issues, addings, asked = strings.Join(many, ","), fmt.Sprintf(adding, 41, "2026-11-24T17:00:02Z", "Bot", "github-actions"), nil
	items, err = survey(context.Background(), g, c)
	if err != nil || len(asked) != 1 {
		t.Fatalf("survey with many labelled issues: asked %d times, %v", len(asked), err)
	}
	if n := strings.Count(asked[0], ": issue(number: "); n != 10 || !strings.Contains(asked[0], "i40: issue(number: 40)") {
		t.Errorf("asked about %d issues, want the first 10:\n%s", n, asked[0])
	}
	if it := byNumber(items)[9]; !it.FirstSince.IsZero() || it.FirstIssue != 0 {
		t.Errorf("an issue past the tenth: since %v, issue %d; want zero and 0", it.FirstSince, it.FirstIssue)
	}
}

func TestFirstSince(t *testing.T) {
	c := &Config{Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]", Maintainers: []string{"maint", "github-actions"}, Apps: []string{"github-actions[bot]", "other-app[bot]"}}
	t0 := time.Date(2026, 11, 24, 19, 0, 0, 0, time.UTC)
	add := func(name string, a *actor, d time.Duration) event {
		e := event{Typename: "LabeledEvent", CreatedAt: t0.Add(d), Actor: a}
		e.Label.Name = name
		return e
	}
	maint, agent, app, other := &actor{"User", "Maint"}, &actor{"Bot", "fc-agent"}, &actor{"Bot", "github-actions"}, &actor{"Bot", "other-app"}
	has := []string{"other", mergeFirst}
	for _, tc := range []struct {
		name   string
		labels []string
		events []event
		want   time.Time // on a pull request
		issue  time.Time // on an issue that the App opened at t0
	}{
		{"not labelled", []string{"other"}, []event{add("other", maint, 0)}, time.Time{}, time.Time{}},
		{"label removed again", []string{"other"}, []event{add(mergeFirst, maint, 0)}, time.Time{}, time.Time{}},
		{"by a maintainer", has, []event{add(mergeFirst, maint, time.Hour), add("other", agent, 2*time.Hour)}, t0.Add(time.Hour), t0.Add(time.Hour)},
		{"by the App, with the opening", has, []event{add(mergeFirst, app, 2*time.Second)}, time.Time{}, t0.Add(2 * time.Second)},
		{"by the App, a minute after the opening", has, []event{add(mergeFirst, app, time.Minute)}, time.Time{}, time.Time{}},
		{"by another App in apps, with the opening", has, []event{add(mergeFirst, other, 2*time.Second)}, time.Time{}, time.Time{}},
		{"by the agent App", has, []event{add(mergeFirst, agent, 0)}, time.Time{}, time.Time{}},
		{"by the reviewer App", has, []event{add(mergeFirst, &actor{"Bot", "fc-reviewer"}, 0)}, time.Time{}, time.Time{}},
		{"by a person outside the config", has, []event{add(mergeFirst, &actor{"User", "mallory"}, 0)}, time.Time{}, time.Time{}},
		{"by an App with a maintainer's name", has, []event{add(mergeFirst, &actor{"Bot", "maint"}, 0)}, time.Time{}, time.Time{}},
		{"by an App outside the config", has, []event{add(mergeFirst, &actor{"Bot", "stranger"}, 0)}, time.Time{}, time.Time{}},
		{"by a deleted account", has, []event{add(mergeFirst, nil, 0)}, time.Time{}, time.Time{}},
		{"added again by a maintainer", has, []event{add(mergeFirst, agent, 0), add(mergeFirst, maint, time.Hour)}, t0.Add(time.Hour), t0.Add(time.Hour)},
		{"added again by an agent", has, []event{add(mergeFirst, maint, 0), add(mergeFirst, agent, time.Hour)}, time.Time{}, time.Time{}},
		{"added again by an agent after the App", has, []event{add(mergeFirst, app, 0), add(mergeFirst, agent, 10*time.Second)}, time.Time{}, time.Time{}},
		{"added again by the App later", has, []event{add(mergeFirst, app, 0), add(mergeFirst, app, time.Hour)}, time.Time{}, time.Time{}},
		{"its adding is not among the latest", has, []event{add("other", maint, 0)}, time.Time{}, time.Time{}},
		{"marked ready after it", has, []event{add(mergeFirst, maint, 0), {Typename: "ReadyForReviewEvent", CreatedAt: t0.Add(time.Hour)}}, t0, t0},
	} {
		if got := firstSince(tc.labels, tc.events, c); !got.Equal(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
		if got := issueFirstSince(tc.labels, tc.events, app, t0, c); !got.Equal(tc.issue) {
			t.Errorf("%s, on an issue: %v, want %v", tc.name, got, tc.issue)
		}
	}
	// The App's label counts only on an issue that the same App opened.
	with := []event{add(mergeFirst, app, 2*time.Second)}
	for _, tc := range []struct {
		name   string
		author *actor
	}{
		{"the agent App", agent}, {"another App in apps", other}, {"a maintainer", maint},
		{"a person with the App's name", &actor{"User", "github-actions"}}, {"a deleted account", nil},
	} {
		if got := issueFirstSince(has, with, tc.author, t0, c); !got.IsZero() {
			t.Errorf("the App's label on an issue of %s: %v, want zero", tc.name, got)
		}
	}
}

func TestReady(t *testing.T) {
	t0 := time.Date(2026, 11, 24, 19, 0, 0, 0, time.UTC)
	marked := func(d time.Duration) event { return event{Typename: "ReadyForReviewEvent", CreatedAt: t0.Add(d)} }
	draft := func(d time.Duration) event { return event{Typename: "ConvertToDraftEvent", CreatedAt: t0.Add(d)} }
	label := func(d time.Duration) event { return event{Typename: "LabeledEvent", CreatedAt: t0.Add(d)} }
	for _, tc := range []struct {
		name   string
		events []event
		want   time.Time
	}{
		{"opened ready", nil, t0},
		{"opened ready, labelled later", []event{label(3 * time.Hour)}, t0},
		{"opened as a draft, marked ready", []event{label(time.Minute), marked(time.Hour), label(3 * time.Hour)}, t0.Add(time.Hour)},
		{"opened as a draft, marked ready twice", []event{marked(time.Hour), draft(90 * time.Minute), marked(2 * time.Hour)}, t0.Add(time.Hour)},
		{"opened ready, a draft for a while", []event{draft(time.Hour), marked(2 * time.Hour)}, t0},
		{"opened ready, a draft now", []event{draft(time.Hour)}, t0},
		{"marked ready twice, the draft between is not among the events", []event{marked(time.Hour), marked(2 * time.Hour)}, t0.Add(time.Hour)},
	} {
		if got := ready(t0, tc.events); !got.Equal(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCandidates checks the order of jobs: unfinished pull requests come
// before new work.
func TestCandidates(t *testing.T) {
	c := &Config{}
	items := []Item{
		{Number: 1},
		{Number: 2, Labels: []string{"proposed"}},
		{Number: 3, PR: true, Ours: true, Draft: true},
		{Number: 4, PR: true, Ours: true, Checks: "SUCCESS"},
		{Number: 5, PR: true, Ours: true, Conflict: true},
		{Number: 6, PR: true, Ours: true, Draft: true},
	}
	var got []string
	for _, cand := range candidates(items, c) {
		got = append(got, fmt.Sprintf("%s %d", cand.job, cand.Number))
	}
	want := []string{"fix 5", "review 4", "continue 3", "continue 6", "triage 2", "work 1"}
	if !slices.Equal(got, want) {
		t.Errorf("candidates %v, want %v", got, want)
	}

	// The jobs of the pull request that merges first come before all
	// others, and no other pull request gets auto-merge.
	c = &Config{MergeFirstLimit: duration{4 * time.Hour}}
	items = append(items,
		Item{Number: 7, PR: true, Ours: true, Checks: "SUCCESS", Review: "APPROVED"},
		Item{Number: 8, PR: true, Ours: true, Checks: "SUCCESS", Labels: []string{mergeFirst}, FirstSince: time.Now().Add(-time.Hour)},
		Item{Number: 9, PR: true, Ours: true, Checks: "SUCCESS", Review: "APPROVED", Decision: "REVIEW_REQUIRED"},
	)
	got = nil
	for _, cand := range candidates(items, c) {
		got = append(got, fmt.Sprintf("%s %d", cand.job, cand.Number))
	}
	want = []string{"review 8", "park 9", "fix 5", "review 4", "continue 3", "continue 6", "triage 2", "work 1"}
	if !slices.Equal(got, want) {
		t.Errorf("candidates with a pull request that merges first %v, want %v", got, want)
	}
	items[len(items)-2].FirstSince = time.Now().Add(-5 * time.Hour)
	got = nil
	for _, cand := range candidates(items, c) {
		got = append(got, fmt.Sprintf("%s %d", cand.job, cand.Number))
	}
	want = []string{"park 9", "auto-merge 7", "fix 5", "review 4", "review 8", "continue 3", "continue 6", "triage 2", "work 1"}
	if !slices.Equal(got, want) {
		t.Errorf("candidates after merge_first_limit %v, want %v", got, want)
	}

	// A pull request that closes a labelled issue merges first like one
	// that has the label itself; the issue's own time does not hold back
	// anything.
	items[len(items)-2] = Item{Number: 8, PR: true, Ours: true, Checks: "SUCCESS", FirstIssue: 1, FirstSince: time.Now().Add(-time.Hour)}
	items[0] = Item{Number: 1, Labels: []string{mergeFirst}, FirstSince: time.Now().Add(-2 * time.Hour), ClosingPRs: 1}
	got = nil
	for _, cand := range candidates(items, c) {
		got = append(got, fmt.Sprintf("%s %d", cand.job, cand.Number))
	}
	want = []string{"review 8", "park 9", "fix 5", "review 4", "continue 3", "continue 6", "triage 2"}
	if !slices.Equal(got, want) {
		t.Errorf("candidates with a pull request that closes a labelled issue %v, want %v", got, want)
	}
}

func TestVerdicts(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := func(kind, login, state, commit string, at time.Time) review {
		var v review
		v.Author, v.State, v.Commit.Oid, v.SubmittedAt = actor{kind, login}, state, commit, at
		return v
	}
	got := verdicts([]review{
		r("Bot", "fc-reviewer", "CHANGES_REQUESTED", "a", t0),
		r("Bot", "fc-reviewer", "APPROVED", "b", t0.Add(1*time.Hour)),
		r("User", "fc-reviewer", "CHANGES_REQUESTED", "b", t0.Add(2*time.Hour)), // a person with the App's name
		r("Bot", "fc-reviewer", "COMMENTED", "b", t0.Add(3*time.Hour)),
		r("User", "bob", "CHANGES_REQUESTED", "b", t0.Add(4*time.Hour)),
		r("User", "bob", "APPROVED", "b", t0.Add(5*time.Hour)),
	}, "b")
	if v := got[actor{"Bot", "fc-reviewer"}]; v.State != "APPROVED" || !v.SubmittedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("fc-reviewer: %+v, want the approval of b", v)
	}
	if got[actor{"User", "fc-reviewer"}].State != "CHANGES_REQUESTED" || got[actor{"User", "bob"}].State != "APPROVED" || len(got) != 3 {
		t.Errorf("verdicts %+v", got)
	}
}

func TestRemovals(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 1, 51, 0, 0, time.UTC) // the reviewer's verdict
	failed := func(d time.Duration) removal { return removal{t0.Add(d), "failed_checks"} }
	for _, tc := range []struct {
		name    string
		removed []removal
		head    time.Time
		want    int
	}{
		{"never removed", nil, t0.Add(-time.Hour), 0},
		{"removed before the verdict", []removal{failed(-time.Hour)}, t0.Add(-2 * time.Hour), 0},
		{"removed once since", []removal{failed(-time.Hour), failed(8 * time.Minute)}, t0.Add(-2 * time.Hour), 1},
		{"removed twice since", []removal{failed(8 * time.Minute), failed(time.Hour)}, t0.Add(-time.Hour), 2},
		{"removed twice, then a new commit", []removal{failed(8 * time.Minute), failed(time.Hour)}, t0.Add(2 * time.Hour), 0},
		{"removed, a new commit, removed again", []removal{failed(8 * time.Minute), failed(3 * time.Hour)}, t0.Add(2 * time.Hour), 1},
		{"left by merging", []removal{failed(8 * time.Minute), {t0.Add(time.Hour), "merged"}}, t0.Add(-time.Hour), 1},
		{"commit dated in the future", []removal{failed(8 * time.Minute), failed(time.Hour)}, t0.Add(48 * time.Hour), 2},
	} {
		if got := removals(tc.removed, t0, tc.head, t0.Add(4*time.Hour)); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestClosing(t *testing.T) {
	c := &Config{Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]"}
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	pr := func(state string, merged time.Duration, a *actor, association string) closingPR {
		p := closingPR{State: state, Author: a, AuthorAssociation: association}
		if state == "MERGED" {
			p.MergedAt = now.Add(-merged)
		}
		return p
	}
	agent, mallory := &actor{"Bot", "fc-agent"}, &actor{"User", "mallory"}
	for _, tc := range []struct {
		name string
		prs  []closingPR
		want int
	}{
		{"none", nil, 0},
		{"open", []closingPR{pr("OPEN", 0, agent, "NONE")}, 1},
		{"open, from outside", []closingPR{pr("OPEN", 0, mallory, "CONTRIBUTOR")}, 0},
		{"just merged", []closingPR{pr("MERGED", 4*time.Second, agent, "NONE")}, 1},
		{"just merged, from outside", []closingPR{pr("MERGED", 4*time.Second, mallory, "CONTRIBUTOR")}, 1},
		{"merged long ago", []closingPR{pr("MERGED", time.Hour, agent, "NONE")}, 0},
		{"closed", []closingPR{pr("CLOSED", 0, agent, "NONE")}, 0},
		{"several", []closingPR{pr("CLOSED", 0, agent, "NONE"), pr("OPEN", 0, agent, "NONE"), pr("MERGED", time.Second, agent, "NONE")}, 2},
	} {
		if got := closing(tc.prs, c, now); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestOutside(t *testing.T) {
	c := &Config{Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]", Maintainers: []string{"maint"}}
	for _, tc := range []struct {
		author      *actor
		association string
		want        bool
	}{
		{&actor{"Bot", "fc-agent"}, "NONE", false},
		{&actor{"Bot", "fc-reviewer[bot]"}, "NONE", false},
		{&actor{"User", "fc-agent"}, "NONE", true}, // a person with the App's name
		{&actor{"User", "alice"}, "OWNER", false},
		{&actor{"User", "alice"}, "MEMBER", false},
		{&actor{"User", "alice"}, "COLLABORATOR", false},
		{&actor{"User", "mallory"}, "CONTRIBUTOR", true},
		{&actor{"Bot", "other-app"}, "NONE", true},
		{nil, "NONE", true},
		{&actor{"User", "Maint"}, "NONE", false}, // a maintainer whose membership is private
		{&actor{"Bot", "maint"}, "NONE", true},   // an App with a maintainer's name
	} {
		if got := outside(tc.author, tc.association, c); got != tc.want {
			t.Errorf("%v %s: outside %v, want %v", tc.author, tc.association, got, tc.want)
		}
	}
}

func TestIssueOutside(t *testing.T) {
	c := &Config{Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]", Apps: []string{"github-actions[bot]"}}
	for _, tc := range []struct {
		author      *actor
		association string
		want        bool
	}{
		{&actor{"Bot", "github-actions"}, "NONE", false},
		{&actor{"Bot", "github-actions[bot]"}, "CONTRIBUTOR", false},
		{&actor{"User", "github-actions"}, "NONE", true}, // a person with a listed App's name
		{&actor{"Bot", "other-app"}, "NONE", true},
		{&actor{"User", "mallory"}, "CONTRIBUTOR", true},
		{&actor{"User", "alice"}, "MEMBER", false},
		{nil, "NONE", true},
	} {
		if got := issueOutside(tc.author, tc.association, c); got != tc.want {
			t.Errorf("%v %s: issueOutside %v, want %v", tc.author, tc.association, got, tc.want)
		}
	}
	// A listed App's pull requests and reviews stay outside.
	if !outside(&actor{"Bot", "github-actions"}, "NONE", c) {
		t.Error("a listed App counts as in the project beyond its issues")
	}
}
