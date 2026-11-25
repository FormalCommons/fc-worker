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
	// since is when a label merge-first began to count, or when the
	// reviewer first approved a commit.
	since := time.Date(2026, 11, 24, 19, 0, 0, 0, time.UTC)
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
		{"conflict, approved on an earlier commit", ours(Item{Conflict: true, ApprovedSince: since}), "fix"},
		{"label counts, conflict, not reviewed", ours(Item{Conflict: true, FirstSince: since}), "review"},
		{"label counts, conflict, checks running", ours(Item{Conflict: true, FirstSince: since, Checks: "PENDING"}), "fix"},
		{"label counts, conflict, no checks", Item{PR: true, Ours: true, Conflict: true, FirstSince: since}, "fix"},
		{"label counts, conflict, checks failed", ours(Item{Conflict: true, FirstSince: since, Checks: "FAILURE"}), "fix"},
		{"label counts, conflict, changes requested", ours(Item{Conflict: true, FirstSince: since, Review: "CHANGES_REQUESTED"}), "fix"},
		{"label counts, conflict, a person requested changes", ours(Item{Conflict: true, FirstSince: since, Requested: true}), "fix"},
		{"label counts, conflict, approved on an earlier commit", ours(Item{Conflict: true, FirstSince: since, ApprovedSince: since}), "fix"},
		{"label counts, conflict, approved", ours(Item{Conflict: true, FirstSince: since, ApprovedSince: since, Review: "APPROVED"}), "fix"},
		{"label counts, conflict, a draft", Item{PR: true, Draft: true, Ours: true, Conflict: true, FirstSince: since, Checks: "SUCCESS"}, "continue"},
		{"label counts, conflict, parked", ours(Item{Conflict: true, FirstSince: since, Labels: []string{"needs-human"}}), ""},
		{"behind, but it does not merge first", ours(Item{Review: "APPROVED", Behind: 3}), autoMerge},
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
		{"person's PR, label counts, conflict, not reviewed", Item{PR: true, Checks: "SUCCESS", Conflict: true, FirstSince: since}, "review"},
		{"person's PR, label counts, conflict, changes requested", Item{PR: true, Checks: "SUCCESS", Conflict: true, FirstSince: since, Review: "CHANGES_REQUESTED"}, ""},
		{"person's PR, changes requested", Item{PR: true, Checks: "SUCCESS", Review: "CHANGES_REQUESTED"}, ""},
		{"person's PR, approved", Item{PR: true, Checks: "SUCCESS", Review: "APPROVED"}, ""},
	} {
		if got, why := next(tc.it, c, 0); got != tc.want {
			t.Errorf("%s: got %q (%s), want %q", tc.name, got, why, tc.want)
		}
	}
}

// TestNextMergeFirst checks what waits while pull request 7 merges first:
// only the auto-merge of the others, and its own if the base branch has
// commits that it lacks.
func TestNextMergeFirst(t *testing.T) {
	c := &Config{Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]"}
	pr := func(number int, it Item) Item {
		it.PR, it.Ours, it.Number, it.Checks = true, true, number, "SUCCESS"
		return it
	}
	const waits = "approved; waiting until #7, which merges first, has merged"
	const lacks = "approved, and the others now wait for it to merge, but the base branch has "
	person := pr(7, Item{Review: "APPROVED", Behind: 83})
	person.Ours = false
	for _, tc := range []struct {
		name string
		it   Item
		want string
		why  string // if not "", the reason wanted
	}{
		{"it is approved", pr(7, Item{Review: "APPROVED"}), autoMerge, "approved"},
		{"it left the queue once", pr(7, Item{Review: "APPROVED", Removals: 1}), autoMerge, ""},
		{"it is behind", pr(7, Item{Review: "APPROVED", Behind: 83}), "fix", lacks + "83 commits that it lacks"},
		{"it is one commit behind", pr(7, Item{Review: "APPROVED", Behind: 1}), "fix", lacks + "1 commit that it lacks"},
		{"it is behind and left the queue once", pr(7, Item{Review: "APPROVED", Behind: 83, Removals: 1}), "fix", lacks + "83 commits that it lacks"},
		{"it is behind, in the queue", pr(7, Item{Review: "APPROVED", Behind: 83, Queued: true}), "", "approved, waiting for the merge queue"},
		{"it is behind, auto-merge on", pr(7, Item{Review: "APPROVED", Behind: 83, AutoMerge: true}), "", "approved, waiting for the merge queue"},
		{"it is behind, its approval does not count", pr(7, Item{Review: "APPROVED", Behind: 83, Decision: "REVIEW_REQUIRED"}), park, ""},
		{"it is behind, a person's", person, "", "approved; its author decides when to merge"},
		{"another is behind", pr(8, Item{Review: "APPROVED", Behind: 83}), "", waits},
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
	// Labelled an hour ago, and approved half an hour ago.
	since, approved := now.Add(-time.Hour), now.Add(-30*time.Minute)
	held := Item{Number: 7, PR: true, Labels: []string{mergeFirst}, FirstSince: since, ApprovedSince: approved}
	closes := Item{Number: 7, PR: true, FirstIssue: 5, FirstSince: since, ApprovedSince: approved}
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
		{"labelled and approved", []Item{{Number: 1}, {Number: 2, PR: true}, held}, 7},
		{"labelled, not approved yet", []Item{with(func(it *Item) { it.ApprovedSince = time.Time{} })}, 0},
		{"approved, the label does not count", []Item{with(func(it *Item) { it.FirstSince = time.Time{} })}, 0},
		{"approved before it was labelled", []Item{with(func(it *Item) { it.ApprovedSince = now.Add(-5 * time.Hour) })}, 7},
		{"labelled long before it was approved", []Item{with(func(it *Item) { it.FirstSince = now.Add(-5 * time.Hour) })}, 7},
		{"labelled and approved too long ago", []Item{with(func(it *Item) { it.FirstSince, it.ApprovedSince = now.Add(-5*time.Hour), now.Add(-4*time.Hour) })}, 0},
		{"approved and labelled too long ago", []Item{with(func(it *Item) { it.FirstSince, it.ApprovedSince = now.Add(-4*time.Hour), now.Add(-5*time.Hour) })}, 0},
		{"approved a second less than the limit ago", []Item{with(func(it *Item) {
			it.FirstSince, it.ApprovedSince = now.Add(-5*time.Hour), now.Add(time.Second-4*time.Hour)
		})}, 7},
		{"draft", []Item{with(func(it *Item) { it.Draft = true })}, 0},
		{"parked", []Item{with(func(it *Item) { it.Labels = []string{mergeFirst, "needs-human"} })}, 0},
		{"from outside", []Item{with(func(it *Item) { it.Outside = true })}, 0},
		{"two: the older", []Item{held, with(func(it *Item) { it.Number = 9 })}, 7},
		{"two: the older is a draft", []Item{with(func(it *Item) { it.Draft = true }), with(func(it *Item) { it.Number = 9 })}, 9},
		{"two: the older is not approved yet", []Item{with(func(it *Item) { it.ApprovedSince = time.Time{} }), with(func(it *Item) { it.Number = 9 })}, 9},
		{"an issue whose label counts", []Item{{Number: 5, Labels: []string{mergeFirst}, FirstSince: since}}, 0},
		{"it closes a labelled issue", []Item{{Number: 5, Labels: []string{mergeFirst}, FirstSince: since}, closes}, 7},
		{"it closes a labelled issue, as a draft", []Item{with(func(it *Item) { *it = closes; it.Draft = true })}, 0},
		{"it closes a labelled issue, not approved yet", []Item{with(func(it *Item) { *it = closes; it.ApprovedSince = time.Time{} })}, 0},
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
		{"it merges first", held, 7, "merges first until 23:30 UTC"},
		{"approved before it was labelled", with(func(it *Item) { it.ApprovedSince = now.Add(-5 * time.Hour) }), 7, "merges first until 23:00 UTC"},
		{"not approved yet", with(func(it *Item) { it.ApprovedSince = time.Time{} }), 0, "labelled merge-first: it holds nothing back until the reviewer has approved it, but its jobs come before the others"},
		{"not approved yet, another merges first", with(func(it *Item) { it.ApprovedSince = time.Time{} }), 9, "labelled merge-first: it holds nothing back until the reviewer has approved it, but its jobs come before the others"},
		{"not approved, labelled three days ago", with(func(it *Item) { it.FirstSince, it.ApprovedSince = now.Add(-firstIssueSpan), time.Time{} }), 0, "labelled merge-first: it holds nothing back until the reviewer has approved it; its jobs came before the others until 20:00 UTC on 24 November"},
		{"another goes before it", with(func(it *Item) { it.Number = 9 }), 7, "labelled merge-first; #7 goes before it"},
		{"the label does not count", with(func(it *Item) { it.FirstSince = time.Time{} }), 0, no + "no maintainer in the config added it, or 20 events came since"},
		{"labelled and approved too long ago", with(func(it *Item) { it.FirstSince, it.ApprovedSince = now.Add(-5*time.Hour), now.Add(-4*time.Hour) }), 0, "labelled merge-first, which counted until 20:00 UTC on 24 November"},
		{"approved and labelled too long ago", with(func(it *Item) { it.FirstSince, it.ApprovedSince = now.Add(-4*time.Hour), now.Add(-5*time.Hour) }), 0, "labelled merge-first, which counted until 20:00 UTC on 24 November"},
		{"draft", with(func(it *Item) { it.Draft = true }), 0, no + "a draft"},
		{"the agent's draft that closes a labelled issue", with(func(it *Item) { *it = closes; it.Draft, it.Ours, it.IssueSince = true, true, since }), 0, "closes an issue labelled merge-first: a draft holds nothing back, but its jobs come before the others"},
		{"the agent's parked draft that closes a labelled issue", with(func(it *Item) {
			*it = closes
			it.Draft, it.Ours, it.IssueSince = true, true, since
			it.Labels = []string{"needs-human"}
		}), 0, "closes #5, labelled merge-first, which does not count: a draft"},
		{"the agent's draft that closes an issue labelled three days ago", with(func(it *Item) { *it = closes; it.Draft, it.Ours, it.IssueSince = true, true, now.Add(-firstIssueSpan) }), 0, "closes #5, labelled merge-first, which does not count: a draft"},
		{"another's draft that closes a labelled issue", with(func(it *Item) { *it = closes; it.Draft, it.IssueSince = true, since }), 0, "closes #5, labelled merge-first, which does not count: a draft"},
		{"from outside", with(func(it *Item) { it.Outside = true }), 0, no + "from outside the project"},
		{"parked", with(func(it *Item) { it.Labels = []string{mergeFirst, "needs-human"} }), 0, no + "parked"},
		{"an issue whose label counts", Item{Number: 5, Labels: []string{mergeFirst}, FirstSince: since}, 7, "labelled merge-first, which counts until 19:00 UTC on 27 November: a job of its own comes before the others, and a pull request that closes it and is ready for review before then merges first once the reviewer has approved it"},
		{"an issue whose label counted", Item{Number: 5, Labels: []string{mergeFirst}, FirstSince: now.Add(-firstIssueSpan)}, 7, "labelled merge-first, which counted until 20:00 UTC on 24 November: a pull request that closes it merges first only if it was ready for review before then"},
		{"an issue whose label does not count", Item{Number: 5, Labels: []string{mergeFirst}}, 0, "labelled merge-first: the label does not count, or the issue needs no job and no open pull request closes it, or more than ten such issues come before it"},
		{"it closes a labelled issue, ready too late", with(func(it *Item) { *it = closes; it.FirstSince = time.Time{} }), 0, "closes #5, labelled merge-first, which does not count: it was ready for review three days or more after the issue got the label"},
		{"it closes a labelled issue, with a label of its own", with(func(it *Item) { it.FirstIssue = 5; it.Number = 9 }), 7, "closes #5, labelled merge-first; #7 goes before it"},
		{"it closes a labelled issue", closes, 7, "merges first until 23:30 UTC"},
		{"it closes a labelled issue, not approved yet", with(func(it *Item) { *it = closes; it.ApprovedSince = time.Time{} }), 0, "closes #5, labelled merge-first: it holds nothing back until the reviewer has approved it, but its jobs come before the others"},
		{"it closes a labelled issue, as a draft", with(func(it *Item) { *it = closes; it.Draft = true }), 0, "closes #5, labelled merge-first, which does not count: a draft"},
		{"it closes a labelled issue, too long ago", with(func(it *Item) {
			*it = closes
			it.FirstSince, it.ApprovedSince = now.Add(-5*time.Hour), now.Add(-6*time.Hour)
		}), 0, "closes #5, labelled merge-first, which counted until 19:00 UTC on 24 November"},
		{"it closes a labelled issue, another goes before it", with(func(it *Item) { *it = closes; it.Number = 9 }), 7, "closes #5, labelled merge-first; #7 goes before it"},
	} {
		if got := firstNote(tc.it, tc.f, c, now); got != tc.want {
			t.Errorf("note, %s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLeads(t *testing.T) {
	now := time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)
	edge := now.Add(-firstIssueSpan)
	for _, tc := range []struct {
		name string
		it   Item
		want bool
	}{
		{"an issue", Item{}, false},
		{"an issue whose label counts", Item{FirstSince: now}, true},
		{"an issue labelled a second less than three days ago", Item{FirstSince: edge.Add(time.Second)}, true},
		{"an issue labelled three days ago", Item{FirstSince: edge}, false},
		{"the agent's draft that closes a labelled issue", Item{PR: true, Draft: true, Ours: true, FirstIssue: 5, IssueSince: now}, true},
		{"the same, labelled a second less than three days ago", Item{PR: true, Draft: true, Ours: true, FirstIssue: 5, IssueSince: edge.Add(time.Second)}, true},
		{"the same, labelled three days ago", Item{PR: true, Draft: true, Ours: true, FirstIssue: 5, IssueSince: edge}, false},
		{"the same, with a label of its own since", Item{PR: true, Draft: true, Ours: true, FirstIssue: 5, IssueSince: edge, FirstSince: now}, false},
		{"the agent's draft with a label of its own", Item{PR: true, Draft: true, Ours: true, FirstSince: now}, false},
		{"another's draft that closes a labelled issue", Item{PR: true, Draft: true, FirstIssue: 5, IssueSince: now}, false},
		{"the agent's pull request, ready, that closes a labelled issue", Item{PR: true, Ours: true, FirstIssue: 5, IssueSince: now, FirstSince: now}, true},
		{"the same, ready too late for the label to count", Item{PR: true, Ours: true, FirstIssue: 5, IssueSince: now}, false},
		{"the same, approved", Item{PR: true, Ours: true, FirstIssue: 5, IssueSince: now, FirstSince: now, ApprovedSince: now}, false},
		{"a pull request whose label counts", Item{PR: true, Ours: true, FirstSince: now}, true},
		{"a person's pull request whose label counts", Item{PR: true, FirstSince: now}, true},
		{"a pull request whose label counts, approved", Item{PR: true, Ours: true, FirstSince: now, ApprovedSince: now}, false},
		{"a pull request approved before its label counted", Item{PR: true, Ours: true, FirstSince: now, ApprovedSince: edge}, false},
		{"a pull request labelled a second less than three days ago", Item{PR: true, Ours: true, FirstSince: edge.Add(time.Second)}, true},
		{"a pull request labelled three days ago", Item{PR: true, Ours: true, FirstSince: edge}, false},
		{"a pull request without a label, approved or not", Item{PR: true, Ours: true}, false},
	} {
		if got := leads(tc.it, now); got != tc.want {
			t.Errorf("%s: leads %v, want %v", tc.name, got, tc.want)
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
	var fails bool     // whether GitHub answers those with an error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		q := body.Query
		switch {
		case strings.Contains(q, "issue(number: "):
			asked = append(asked, q)
			if fails {
				fmt.Fprint(w, `{"data": {"repository": {"i3": null}}, "errors": [{"type": "NOT_FOUND", "path": ["repository", "i3"], "message": "Could not resolve to an Issue with the number of 3."}]}`)
				return
			}
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
	items, err := survey(context.Background(), direct{g, c.Repo}, g, c)
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
	// closes these. Nothing open closes 5, which the workflow opened with
	// the label too, so it needs work; nor 25, which is parked and needs no
	// job. A maintainer labelled 22 at 20:00, which 9 and 14 close; 23 more
	// than three days before 13, which closes it, was opened; and 24 less
	// than three days before 16.
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
		fmt.Sprintf(issue, 25, opened, "github-actions", label+`, {"name": "needs-human"}`, ""),
	}, ",")
	addings = strings.Join([]string{
		fmt.Sprintf(adding, 3, "2026-11-24T17:00:02Z", "Bot", "github-actions"),
		fmt.Sprintf(adding, 4, "2026-11-24T17:00:02Z", "Bot", "fc-agent"),
		fmt.Sprintf(adding, 5, "2026-11-24T17:00:02Z", "Bot", "github-actions"),
		fmt.Sprintf(adding, 6, "2026-11-24T18:00:00Z", "Bot", "github-actions"),
		fmt.Sprintf(adding, 21, "2026-11-24T17:00:02Z", "Bot", "github-actions"),
		fmt.Sprintf(adding, 22, "2026-11-24T20:00:00Z", "User", "maint"),
		fmt.Sprintf(adding, 23, "2026-11-21T18:30:00Z", "User", "maint"),
		fmt.Sprintf(adding, 24, "2026-11-21T18:30:01Z", "User", "maint"),
	}, ",")
	items, err = survey(context.Background(), direct{g, c.Repo}, g, c)
	if err != nil || len(items) != 20 || len(asked) != 1 {
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
	if want := []string{"i3", "i4", "i6", "i21", "i22", "i23", "i24", "i5"}; !slices.Equal(aliases, want) {
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
		{"a labelled issue that needs work, which nothing open closes", 5, at(17, 0, 2), 0},
		{"a labelled issue that needs no job, which nothing open closes", 25, time.Time{}, 0},
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
	// Since when the label of an issue that a pull request closes counts,
	// whatever the pull request's own label says.
	for number, want := range map[int]time.Time{
		7: at(17, 0, 2), 8: {}, 9: at(20, 0, 0), 12: {}, 14: at(20, 0, 0), 15: {},
		13: at(18, 30, 0).AddDate(0, 0, -3), 16: at(18, 30, 1).AddDate(0, 0, -3),
	} {
		if got := m[number].IssueSince; !got.Equal(want) {
			t.Errorf("#%d closes an issue whose label counts since %v, want %v", number, got, want)
		}
	}

	// If GitHub does not say who labelled the issues, the survey fails, so
	// that no worker acts as if nothing merged first.
	fails, asked = true, nil
	if items, err := survey(context.Background(), direct{g, c.Repo}, g, c); err == nil || len(asked) != 1 {
		t.Errorf("survey without an answer about the labelled issues: %d items, asked %d times, %v; want an error", len(items), len(asked), err)
	}
	fails = false

	// It asks about ten issues at most, the oldest.
	var many []string
	for n := 31; n <= 42; n++ {
		many = append(many, fmt.Sprintf(issue, n, opened, "github-actions", label, refs("o/r", "OPEN", 9)))
	}
	issues, addings, asked = strings.Join(many, ","), fmt.Sprintf(adding, 41, "2026-11-24T17:00:02Z", "Bot", "github-actions"), nil
	items, err = survey(context.Background(), direct{g, c.Repo}, g, c)
	if err != nil || len(asked) != 1 {
		t.Fatalf("survey with many labelled issues: asked %d times, %v", len(asked), err)
	}
	if n := strings.Count(asked[0], ": issue(number: "); n != 10 || !strings.Contains(asked[0], "i40: issue(number: 40)") {
		t.Errorf("asked about %d issues, want the first 10:\n%s", n, asked[0])
	}
	if it := byNumber(items)[9]; !it.FirstSince.IsZero() || it.FirstIssue != 0 {
		t.Errorf("an issue past the tenth: since %v, issue %d; want zero and 0", it.FirstSince, it.FirstIssue)
	}

	// Of more than ten, those that a pull request closes go first: ten
	// older issues that only need work do not push out the one whose pull
	// request holds the others back. A proposed issue needs a job too; a
	// blocked one and one from outside do not.
	many = nil
	for n := 31; n <= 40; n++ {
		many = append(many, fmt.Sprintf(issue, n, opened, "github-actions", label, ""))
	}
	many = append(many,
		fmt.Sprintf(issue, 41, opened, "github-actions", label, refs("o/r", "OPEN", 9)),
		fmt.Sprintf(issue, 42, opened, "github-actions", label, ""))
	issues, addings, asked = strings.Join(many, ","), fmt.Sprintf(adding, 41, "2026-11-24T17:00:02Z", "Bot", "github-actions"), nil
	items, err = survey(context.Background(), direct{g, c.Repo}, g, c)
	if err != nil || len(asked) != 1 {
		t.Fatalf("survey with many labelled issues, one with a pull request: asked %d times, %v", len(asked), err)
	}
	if n := strings.Count(asked[0], ": issue(number: "); n != 10 || !strings.Contains(asked[0], "i41: issue(number: 41)") || strings.Contains(asked[0], "i40: issue(number: 40)") {
		t.Errorf("asked about %d issues, want #41 and the first 9 others:\n%s", n, asked[0])
	}
	if it := byNumber(items)[9]; !it.FirstSince.Equal(at(18, 30, 0)) || it.FirstIssue != 41 {
		t.Errorf("the pull request that closes the eleventh issue: since %v, issue %d; want %v, 41", it.FirstSince, it.FirstIssue, at(18, 30, 0))
	}
	issues, asked = strings.Join([]string{
		fmt.Sprintf(issue, 51, opened, "github-actions", label+`, {"name": "proposed"}`, ""),
		fmt.Sprintf(issue, 52, opened, "someone", label, ""),
	}, ","), nil
	if _, err = survey(context.Background(), direct{g, c.Repo}, g, c); err != nil || len(asked) != 1 || strings.Count(asked[0], ": issue(number: ") != 1 || !strings.Contains(asked[0], "i51: issue(number: 51)") {
		t.Errorf("survey with a proposed issue and one from outside, both labelled: asked %v, %v; want one question, about #51", asked, err)
	}
}

// TestSurveyBehind checks that survey asks GitHub how far the pull request
// that merges first is behind its base branch only when that decides its
// job: when it would get auto-merge on a commit made before it began to
// hold the others back.
func TestSurveyBehind(t *testing.T) {
	// Pull request 7, the agent's, was labelled by a maintainer at 19:00.
	const pr = `{"id": "PR_%[1]d", "number": %[1]d, "title": "t", "mergeable": %[5]q, "createdAt": "2026-11-24T18:30:00Z",
		"headRefOid": "h%[1]d", "isInMergeQueue": %[6]t,
		"author": {"__typename": %[7]q, "login": "fc-agent"},
		"labels": {"nodes": [%[2]s]},
		"commits": {"nodes": [{"commit": {"committedDate": %[3]q, "statusCheckRollup": {"state": "SUCCESS"}}}]},
		"reviews": {"nodes": [%[4]s]},
		"timelineItems": {"nodes": []},
		"events": {"nodes": [
			{"__typename": "LabeledEvent", "createdAt": "2026-11-24T19:00:00Z", "label": {"name": "merge-first"}, "actor": {"__typename": "User", "login": "maint"}}]}}`
	const label = `{"name": "merge-first"}`
	const review = `{"author": {"__typename": "Bot", "login": %q}, "authorAssociation": "NONE", "state": %q, "submittedAt": %q, "commit": {"oid": %q}}`
	approved := fmt.Sprintf(review, "fc-reviewer", "APPROVED", "2026-11-24T20:00:00Z", "h7")
	dismissed := fmt.Sprintf(review, "fc-reviewer", "DISMISSED", "2026-11-24T19:30:00Z", "old")
	var pulls string   // the open pull requests
	var asked []string // the REST requests
	var behindBy int   // GitHub's answer to the comparison
	var failing string // the path of a REST request that fails, if any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			asked = append(asked, r.Method+" "+r.URL.EscapedPath())
			if strings.Contains(r.URL.Path, "/compare/") && r.URL.RawQuery != "per_page=1" {
				http.Error(w, "the comparison asks for every commit", http.StatusBadRequest)
				return
			}
			switch {
			case r.URL.EscapedPath() == failing:
				http.Error(w, "GitHub is down", http.StatusBadGateway)
			case r.URL.Path == "/repos/o/r/pulls/7":
				fmt.Fprint(w, `{"number": 7, "base": {"ref": "release/1"}, "head": {"ref": "work"}}`)
			case r.URL.EscapedPath() == "/repos/o/r/compare/release%2F1...h7":
				// GitHub compares base...head: behind_by counts the
				// commits of the base that the head lacks.
				fmt.Fprintf(w, `{"status": "diverged", "ahead_by": 4, "behind_by": %d}`, behindBy)
			default:
				http.NotFound(w, r)
			}
			return
		}
		var body struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(body.Query, "pullRequests(") {
			fmt.Fprintf(w, `{"data": {"repository": {"pullRequests": {"nodes": [%s]}}}}`, pulls)
			return
		}
		fmt.Fprint(w, `{"data": {"repository": {"issues": {"nodes": []}}}}`)
	}))
	defer srv.Close()
	g := &github{api: srv.URL, client: srv.Client(), token: "t", expires: time.Now().Add(time.Hour)}
	c := &Config{Repo: "o/r", Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]", Maintainers: []string{"maint"}, MergeFirstLimit: duration{100000 * time.Hour}}
	at := func(hour, min int) time.Time { return time.Date(2026, 11, 24, hour, min, 0, 0, time.UTC) }
	const before, after = "2026-11-24T19:45:00Z", "2026-11-24T20:30:00Z"
	both := []string{"GET /repos/o/r/pulls/7", "GET /repos/o/r/compare/release%2F1...h7"}
	for _, tc := range []struct {
		name     string
		pulls    string
		behindBy int
		asks     []string
		job      string // of pull request 7
		approved time.Time
	}{
		{"approved on a commit made before the approval, behind",
			fmt.Sprintf(pr, 7, label, before, approved, "MERGEABLE", false, "Bot"), 83, both, "fix", at(20, 0)},
		{"the same, not behind",
			fmt.Sprintf(pr, 7, label, before, approved, "MERGEABLE", false, "Bot"), 0, both, autoMerge, at(20, 0)},
		{"approved before on an older commit: the first approval counts",
			fmt.Sprintf(pr, 7, label, "2026-11-24T19:15:00Z", dismissed+","+approved, "MERGEABLE", false, "Bot"), 2, both, "fix", at(19, 30)},
		{"approved on a commit made since the first approval",
			fmt.Sprintf(pr, 7, label, before, dismissed+","+approved, "MERGEABLE", false, "Bot"), 83, nil, autoMerge, at(19, 30)},
		{"approved before the label, on a commit made before the label",
			fmt.Sprintf(pr, 7, label, "2026-11-24T18:45:00Z", fmt.Sprintf(review, "fc-reviewer", "APPROVED", "2026-11-24T18:50:00Z", "h7"), "MERGEABLE", false, "Bot"), 83, both, "fix", at(18, 50)},
		{"not approved yet",
			fmt.Sprintf(pr, 7, label, before, "", "MERGEABLE", false, "Bot"), 83, nil, "review", time.Time{}},
		{"approved by another App only",
			fmt.Sprintf(pr, 7, label, before, fmt.Sprintf(review, "other-app", "APPROVED", "2026-11-24T20:00:00Z", "h7"), "MERGEABLE", false, "Bot"), 83, nil, "review", time.Time{}},
		{"approved on an older commit, the latest not reviewed",
			fmt.Sprintf(pr, 7, label, before, dismissed, "MERGEABLE", false, "Bot"), 83, nil, "review", at(19, 30)},
		{"approved, with a conflict",
			fmt.Sprintf(pr, 7, label, before, approved, "CONFLICTING", false, "Bot"), 83, nil, "fix", at(20, 0)},
		{"approved, in the merge queue",
			fmt.Sprintf(pr, 7, label, before, approved, "MERGEABLE", true, "Bot"), 83, nil, "", at(20, 0)},
		{"approved, a person's",
			fmt.Sprintf(pr, 7, label, before, approved, "MERGEABLE", false, "User"), 83, nil, "", at(20, 0)},
		{"approved, without the label",
			fmt.Sprintf(pr, 7, "", before, approved, "MERGEABLE", false, "Bot"), 83, nil, autoMerge, at(20, 0)},
		{"another is approved too: only the one that merges first is asked about",
			fmt.Sprintf(pr, 7, label, before, approved, "MERGEABLE", false, "Bot") + "," +
				fmt.Sprintf(pr, 8, label, before, fmt.Sprintf(review, "fc-reviewer", "APPROVED", "2026-11-24T20:00:00Z", "h8"), "MERGEABLE", false, "Bot"), 83, both, "fix", at(20, 0)},
	} {
		pulls, behindBy, asked = tc.pulls, tc.behindBy, nil
		items, err := survey(context.Background(), direct{g, c.Repo}, g, c)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !slices.Equal(asked, tc.asks) {
			t.Errorf("%s: asked %v, want %v", tc.name, asked, tc.asks)
		}
		f := first(items, c, time.Now())
		it := items[0]
		if job, why := next(it, c, f); job != tc.job {
			t.Errorf("%s: job %q (%s), want %q", tc.name, job, why, tc.job)
		}
		if want := len(tc.asks) / 2 * tc.behindBy; it.Behind != want || !it.ApprovedSince.Equal(tc.approved) {
			t.Errorf("%s: behind %d, approved since %v; want %d, %v", tc.name, it.Behind, it.ApprovedSince, want, tc.approved)
		}
		for _, other := range items[1:] {
			if job, why := next(other, c, f); other.Behind != 0 || job != "" || !strings.Contains(why, "waiting until #7") {
				t.Errorf("%s: #%d is %d behind and gets %q (%s); want 0 and to wait for #7", tc.name, other.Number, other.Behind, job, why)
			}
		}
	}

	// The fix pushes a commit made after the pull request began to hold
	// the others back, and the reviewer approves it: nothing is asked
	// again, and it gets auto-merge.
	pulls, asked = fmt.Sprintf(pr, 7, label, after, dismissed+","+fmt.Sprintf(review, "fc-reviewer", "APPROVED", "2026-11-24T21:00:00Z", "h7"), "MERGEABLE", false, "Bot"), nil
	items, err := survey(context.Background(), direct{g, c.Repo}, g, c)
	if err != nil || len(asked) != 0 {
		t.Fatalf("survey after the fix: asked %v, %v", asked, err)
	}
	if job, why := next(items[0], c, first(items, c, time.Now())); job != autoMerge || !items[0].HeadAt.Equal(at(20, 30)) {
		t.Errorf("after the fix: job %q (%s), latest commit made at %v; want auto-merge, %v", job, why, items[0].HeadAt, at(20, 30))
	}

	// After merge_first_limit it holds nothing back, and nothing is asked.
	pulls, asked = fmt.Sprintf(pr, 7, label, before, approved, "MERGEABLE", false, "Bot"), nil
	short := *c
	short.MergeFirstLimit = duration{time.Minute}
	if _, err := survey(context.Background(), direct{g, c.Repo}, g, &short); err != nil || len(asked) != 0 {
		t.Errorf("survey after merge_first_limit: asked %v, %v", asked, err)
	}

	// If GitHub does not say how far it is behind, the survey fails, so
	// that no worker gives it auto-merge on a commit that may need to
	// catch up.
	for _, path := range []string{"/repos/o/r/pulls/7", "/repos/o/r/compare/release%2F1...h7"} {
		failing, asked = path, nil
		if items, err := survey(context.Background(), direct{g, c.Repo}, g, c); err == nil {
			t.Errorf("survey without an answer from %s: %d items, asked %v; want an error", path, len(items), asked)
		}
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
		{"by the App, just under a minute after the opening", has, []event{add(mergeFirst, app, 59*time.Second)}, time.Time{}, t0.Add(59 * time.Second)},
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

	// The jobs of a pull request whose label counts come before all
	// others. Until the reviewer has approved a commit of it, it holds
	// nothing back: 7 gets auto-merge.
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
	want = []string{"review 8", "park 9", "auto-merge 7", "fix 5", "review 4", "continue 3", "continue 6", "triage 2", "work 1"}
	if !slices.Equal(got, want) {
		t.Errorf("candidates with a labelled pull request, not approved yet %v, want %v", got, want)
	}
	// Once the reviewer has approved a commit of it, it merges first, and
	// no other pull request gets auto-merge.
	items[len(items)-2].ApprovedSince = time.Now().Add(-30 * time.Minute)
	got = nil
	for _, cand := range candidates(items, c) {
		got = append(got, fmt.Sprintf("%s %d", cand.job, cand.Number))
	}
	want = []string{"review 8", "park 9", "fix 5", "review 4", "continue 3", "continue 6", "triage 2", "work 1"}
	if !slices.Equal(got, want) {
		t.Errorf("candidates with a pull request that merges first %v, want %v", got, want)
	}
	items[len(items)-2].ApprovedSince = time.Now().Add(-5 * time.Hour)
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
	items[len(items)-2] = Item{Number: 8, PR: true, Ours: true, Checks: "SUCCESS", FirstIssue: 1, FirstSince: time.Now().Add(-time.Hour), ApprovedSince: time.Now().Add(-30 * time.Minute)}
	items[0] = Item{Number: 1, Labels: []string{mergeFirst}, FirstSince: time.Now().Add(-2 * time.Hour), ClosingPRs: 1}
	got = nil
	for _, cand := range candidates(items, c) {
		got = append(got, fmt.Sprintf("%s %d", cand.job, cand.Number))
	}
	want = []string{"review 8", "park 9", "fix 5", "review 4", "continue 3", "continue 6", "triage 2"}
	if !slices.Equal(got, want) {
		t.Errorf("candidates with a pull request that closes a labelled issue %v, want %v", got, want)
	}

	// The jobs that lead to a pull request that merges first come next,
	// in the order of the jobs: those of an issue whose label counts,
	// those of the agent's draft that closes such an issue, and those of
	// a pull request whose label counts and that is not approved yet (16),
	// for three days from the label. Another's draft (11), a draft with a
	// label of its own (12) and a pull request that was ready too late for
	// the issue's label to count (15) get nothing from this.
	hour := time.Now().Add(-time.Hour)
	items = []Item{
		{Number: 1},
		{Number: 2, Labels: []string{"proposed"}},
		{Number: 3, PR: true, Draft: true, Ours: true},
		{Number: 4, PR: true, Ours: true, Checks: "SUCCESS"},
		{Number: 5, Labels: []string{mergeFirst}, FirstSince: hour},
		{Number: 6, PR: true, Draft: true, Ours: true, FirstIssue: 10, FirstSince: hour, IssueSince: hour},
		{Number: 7, Labels: []string{mergeFirst}},
		{Number: 8, Labels: []string{mergeFirst}, FirstSince: time.Now().Add(-firstIssueSpan)},
		{Number: 9, PR: true, Ours: true, Checks: "SUCCESS", Labels: []string{mergeFirst}, FirstSince: hour, ApprovedSince: hour},
		{Number: 11, PR: true, Draft: true, FirstIssue: 10, FirstSince: hour, IssueSince: hour},
		{Number: 12, PR: true, Draft: true, Ours: true, Labels: []string{mergeFirst}, FirstSince: hour},
		{Number: 13, PR: true, Draft: true, Ours: true, FirstIssue: 10, FirstSince: hour, IssueSince: time.Now().Add(-firstIssueSpan)},
		{Number: 14, Labels: []string{mergeFirst, "proposed"}, FirstSince: hour},
		{Number: 15, PR: true, Ours: true, Checks: "SUCCESS", FirstIssue: 10, IssueSince: hour},
		{Number: 16, PR: true, Ours: true, Checks: "SUCCESS", Labels: []string{mergeFirst}, FirstSince: hour},
	}
	// Each candidate says which of the two it is, for its job's prompt.
	var firsts, leading []int
	got = nil
	for _, cand := range candidates(items, c) {
		got = append(got, fmt.Sprintf("%s %d", cand.job, cand.Number))
		if cand.first {
			firsts = append(firsts, cand.Number)
		}
		if cand.leads {
			leading = append(leading, cand.Number)
		}
	}
	want = []string{"review 9", "review 16", "continue 6", "triage 14", "work 5", "review 4", "review 15", "continue 3", "continue 12", "continue 13", "triage 2", "work 1", "work 7", "work 8"}
	if !slices.Equal(got, want) {
		t.Errorf("candidates with jobs that lead to a pull request that merges first %v, want %v", got, want)
	}
	if !slices.Equal(firsts, []int{9}) || !slices.Equal(leading, []int{16, 6, 14, 5}) {
		t.Errorf("merges first: %v, leads: %v; want [9] and [16 6 14 5]", firsts, leading)
	}
	// Without a pull request that merges first, they come before all.
	got = nil
	for _, cand := range candidates(items[:8], c) {
		got = append(got, fmt.Sprintf("%s %d", cand.job, cand.Number))
	}
	want = []string{"continue 6", "work 5", "review 4", "continue 3", "triage 2", "work 1", "work 7", "work 8"}
	if !slices.Equal(got, want) {
		t.Errorf("candidates with jobs that lead to a pull request that will merge first %v, want %v", got, want)
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
