package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The same issues and pull requests as GitHub gives them and as a desk
// does. Issue 12 has an open blocker, a closed sub-issue, a pull request
// that merged a minute before deskNow and one of another repository.
// Pull request 20 is the agent's: approved by the reviewer, with a
// person's request for changes, a removal from the merge queue since, and
// a maintainer's label. 22 is a draft with nothing, by a deleted account.
// 23 conflicts, and has reviews of an older commit and by a deleted
// account. The desk also gives 20 an event of a type that this worker does
// not know. Issue 16 is a workflow's, labelled as it was opened; pull
// requests 24 and 25 close it. 24 was opened as a draft by a member whom
// the config does not name, and another such member asks for changes; it
// left the merge queue before its latest commit. Issue 17 is that
// member's.
const (
	githubIssues = `{
  "id": "I_12", "number": 12, "title": "A lemma", "createdAt": "2026-11-24T12:37:00Z", "updatedAt": "2026-11-24T20:04:12Z",
  "authorAssociation": "NONE", "author": {"__typename": "Bot", "login": "github-actions"},
  "labels": {"nodes": [{"name": "merge-first"}]},
  "blockedBy": {"nodes": [{"state": "OPEN"}, {"state": "CLOSED"}]},
  "subIssues": {"nodes": [{"state": "CLOSED"}]},
  "closedByPullRequestsReferences": {"nodes": [
    {"number": 19, "state": "MERGED", "mergedAt": "2026-11-24T22:59:00Z", "authorAssociation": "NONE",
     "author": {"__typename": "Bot", "login": "fc-agent"}, "repository": {"nameWithOwner": "o/r"}},
    {"number": 21, "state": "OPEN", "mergedAt": null, "authorAssociation": "NONE", "author": null, "repository": {"nameWithOwner": "o/fork"}}
  ]}
}, {
  "id": "I_13", "number": 13, "title": "Another", "createdAt": "2026-11-24T13:00:00Z", "updatedAt": "2026-11-24T13:00:00Z",
  "authorAssociation": "NONE", "author": null,
  "labels": {"nodes": []}, "blockedBy": {"nodes": []}, "subIssues": {"nodes": [{"state": "OPEN"}]},
  "closedByPullRequestsReferences": {"nodes": [
    {"number": 20, "state": "OPEN", "mergedAt": null, "authorAssociation": "NONE",
     "author": {"__typename": "Bot", "login": "fc-agent"}, "repository": {"nameWithOwner": "o/r"}}
  ]}
}, {
  "id": "I_16", "number": 16, "title": "Move the pin", "createdAt": "2026-11-24T14:00:00Z", "updatedAt": "2026-11-24T16:00:00Z",
  "authorAssociation": "NONE", "author": {"__typename": "Bot", "login": "github-actions"},
  "labels": {"nodes": [{"name": "merge-first"}]}, "blockedBy": {"nodes": []}, "subIssues": {"nodes": []},
  "closedByPullRequestsReferences": {"nodes": [
    {"number": 24, "state": "OPEN", "mergedAt": null, "authorAssociation": "MEMBER",
     "author": {"__typename": "User", "login": "member"}, "repository": {"nameWithOwner": "o/r"}},
    {"number": 25, "state": "OPEN", "mergedAt": null, "authorAssociation": "NONE",
     "author": {"__typename": "Bot", "login": "fc-agent"}, "repository": {"nameWithOwner": "o/r"}}
  ]}
}, {
  "id": "I_17", "number": 17, "title": "A member's", "createdAt": "2026-11-24T14:30:00Z", "updatedAt": "2026-11-24T14:30:00Z",
  "authorAssociation": "MEMBER", "author": {"__typename": "User", "login": "member"},
  "labels": {"nodes": []}, "blockedBy": {"nodes": []}, "subIssues": {"nodes": []},
  "closedByPullRequestsReferences": {"nodes": []}
}`
	deskIssues = `{
  "id": "I_12", "number": 12, "title": "A lemma", "created_at": "2026-11-24T12:37:00Z", "updated_at": "2026-11-24T20:04:12Z",
  "author": {"type": "Bot", "login": "github-actions"}, "association": "NONE",
  "labels": ["merge-first"],
  "blocked_by": [{"repo": "o/r", "number": 7, "state": "OPEN"}, {"repo": "o/r", "number": 8, "state": "CLOSED"}],
  "sub_issues": [{"repo": "o/r", "number": 14, "state": "CLOSED"}],
  "closing_pulls": [
    {"repo": "o/r", "number": 19, "state": "MERGED", "merged_at": "2026-11-24T22:59:00Z",
     "author": {"type": "Bot", "login": "fc-agent"}, "association": "NONE"},
    {"repo": "o/fork", "number": 21, "state": "OPEN", "author": null, "association": "NONE"}
  ]
}, {
  "id": "I_13", "number": 13, "title": "Another", "created_at": "2026-11-24T13:00:00Z", "updated_at": "2026-11-24T13:00:00Z",
  "author": null, "association": "NONE",
  "labels": [], "blocked_by": [], "sub_issues": [{"repo": "o/r", "number": 15, "state": "OPEN"}],
  "closing_pulls": [
    {"repo": "o/r", "number": 20, "state": "OPEN", "author": {"type": "Bot", "login": "fc-agent"}, "association": "NONE"}
  ]
}, {
  "id": "I_16", "number": 16, "title": "Move the pin", "created_at": "2026-11-24T14:00:00Z", "updated_at": "2026-11-24T16:00:00Z",
  "author": {"type": "Bot", "login": "github-actions"}, "association": "NONE",
  "labels": ["merge-first"], "blocked_by": [], "sub_issues": [],
  "closing_pulls": [
    {"repo": "o/r", "number": 24, "state": "OPEN", "author": {"type": "User", "login": "member"}, "association": "MEMBER"},
    {"repo": "o/r", "number": 25, "state": "OPEN", "author": {"type": "Bot", "login": "fc-agent"}, "association": "NONE"}
  ]
}, {
  "id": "I_17", "number": 17, "title": "A member's", "created_at": "2026-11-24T14:30:00Z", "updated_at": "2026-11-24T14:30:00Z",
  "author": {"type": "User", "login": "member"}, "association": "MEMBER",
  "labels": [], "blocked_by": [], "sub_issues": [], "closing_pulls": []
}`
	githubPulls = `{
  "id": "PR_20", "number": 20, "title": "Prove the lemma", "createdAt": "2026-11-24T19:00:00Z", "updatedAt": "2026-11-24T22:10:00Z",
  "isDraft": false, "headRefOid": "aaa", "mergeable": "MERGEABLE", "reviewDecision": "CHANGES_REQUESTED", "isInMergeQueue": false,
  "authorAssociation": "NONE", "author": {"__typename": "Bot", "login": "fc-agent"},
  "labels": {"nodes": [{"name": "merge-first"}]},
  "autoMergeRequest": {"enabledAt": "2026-11-24T21:31:00Z"},
  "commits": {"nodes": [{"commit": {"committedDate": "2026-11-24T21:00:00Z", "statusCheckRollup": {"state": "SUCCESS"}}}]},
  "reviews": {"nodes": [
    {"author": {"__typename": "Bot", "login": "fc-reviewer"}, "authorAssociation": "NONE", "state": "APPROVED",
     "submittedAt": "2026-11-24T21:30:00Z", "commit": {"oid": "aaa"}},
    {"author": {"__typename": "User", "login": "maint"}, "authorAssociation": "NONE", "state": "CHANGES_REQUESTED",
     "submittedAt": "2026-11-24T21:40:00Z", "commit": {"oid": "aaa"}}
  ]},
  "timelineItems": {"nodes": [
    {"createdAt": "2026-11-24T21:20:00Z", "reason": "failed_checks"},
    {"createdAt": "2026-11-24T21:50:00Z", "reason": "failed_checks"}
  ]},
  "events": {"nodes": [
    {"__typename": "ConvertToDraftEvent", "createdAt": "2026-11-24T19:10:00Z"},
    {"__typename": "ReadyForReviewEvent", "createdAt": "2026-11-24T19:20:00Z"},
    {"__typename": "LabeledEvent", "createdAt": "2026-11-24T20:04:12Z", "label": {"name": "merge-first"}, "actor": {"__typename": "User", "login": "maint"}}
  ]}
}, {
  "id": "PR_22", "number": 22, "title": "Draft", "createdAt": "2026-11-24T23:00:00Z", "updatedAt": "2026-11-24T23:00:00Z",
  "isDraft": true, "headRefOid": "bbb", "mergeable": "UNKNOWN", "reviewDecision": null, "isInMergeQueue": false,
  "authorAssociation": "NONE", "author": null,
  "labels": {"nodes": []}, "autoMergeRequest": null,
  "commits": {"nodes": [{"commit": {"committedDate": "2026-11-24T22:59:00Z", "statusCheckRollup": null}}]},
  "reviews": {"nodes": []}, "timelineItems": {"nodes": []}, "events": {"nodes": []}
}, {
  "id": "PR_23", "number": 23, "title": "Conflicting", "createdAt": "2026-11-24T20:00:00Z", "updatedAt": "2026-11-24T22:00:00Z",
  "isDraft": false, "headRefOid": "ccc", "mergeable": "CONFLICTING", "reviewDecision": "REVIEW_REQUIRED", "isInMergeQueue": true,
  "authorAssociation": "MEMBER", "author": {"__typename": "User", "login": "someone"},
  "labels": {"nodes": [{"name": "needs-human"}, {"name": "merge-first"}]}, "autoMergeRequest": null,
  "commits": {"nodes": [{"commit": {"committedDate": "2026-11-24T21:00:00Z", "statusCheckRollup": {"state": "FAILURE"}}}]},
  "reviews": {"nodes": [
    {"author": {"__typename": "Bot", "login": "fc-reviewer"}, "authorAssociation": "NONE", "state": "APPROVED",
     "submittedAt": "2026-11-24T20:30:00Z", "commit": {"oid": "old"}},
    {"author": null, "authorAssociation": "NONE", "state": "CHANGES_REQUESTED", "submittedAt": "2026-11-24T21:30:00Z", "commit": {"oid": "ccc"}},
    {"author": {"__typename": "Bot", "login": "fc-reviewer"}, "authorAssociation": "NONE", "state": "COMMENTED",
     "submittedAt": "2026-11-24T21:35:00Z", "commit": null}
  ]},
  "timelineItems": {"nodes": [{"createdAt": "2026-11-24T21:50:00Z", "reason": "MERGED"}]},
  "events": {"nodes": [
    {"__typename": "LabeledEvent", "createdAt": "2026-11-24T20:10:00Z", "label": {"name": "merge-first"}, "actor": null},
    {"__typename": "LabeledEvent", "createdAt": "2026-11-24T20:11:00Z", "label": {"name": "needs-human"}, "actor": {"__typename": "Bot", "login": "fc-agent"}}
  ]}
}, {
  "id": "PR_24", "number": 24, "title": "Move the pin", "createdAt": "2026-11-24T15:00:00Z", "updatedAt": "2026-11-24T17:00:00Z",
  "isDraft": false, "headRefOid": "ddd", "mergeable": "MERGEABLE", "reviewDecision": "CHANGES_REQUESTED", "isInMergeQueue": false,
  "authorAssociation": "MEMBER", "author": {"__typename": "User", "login": "member"},
  "labels": {"nodes": []}, "autoMergeRequest": null,
  "commits": {"nodes": [{"commit": {"committedDate": "2026-11-24T17:00:00Z", "statusCheckRollup": {"state": "SUCCESS"}}}]},
  "reviews": {"nodes": [
    {"author": {"__typename": "Bot", "login": "fc-reviewer"}, "authorAssociation": "NONE", "state": "APPROVED",
     "submittedAt": "2026-11-24T16:30:00Z", "commit": {"oid": "ddd"}},
    {"author": {"__typename": "User", "login": "other"}, "authorAssociation": "MEMBER", "state": "CHANGES_REQUESTED",
     "submittedAt": "2026-11-24T16:40:00Z", "commit": {"oid": "ddd"}}
  ]},
  "timelineItems": {"nodes": [{"createdAt": "2026-11-24T16:45:00Z", "reason": "failed_checks"}]},
  "events": {"nodes": [{"__typename": "ReadyForReviewEvent", "createdAt": "2026-11-24T16:00:00Z"}]}
}, {
  "id": "PR_25", "number": 25, "title": "Move the pin again", "createdAt": "2026-11-24T15:30:00Z", "updatedAt": "2026-11-24T15:30:00Z",
  "isDraft": false, "headRefOid": "eee", "mergeable": "MERGEABLE", "reviewDecision": "REVIEW_REQUIRED", "isInMergeQueue": false,
  "authorAssociation": "NONE", "author": {"__typename": "Bot", "login": "fc-agent"},
  "labels": {"nodes": []}, "autoMergeRequest": null,
  "commits": {"nodes": [{"commit": {"committedDate": "2026-11-24T15:30:00Z", "statusCheckRollup": null}}]},
  "reviews": {"nodes": []}, "timelineItems": {"nodes": []}, "events": {"nodes": []}
}`
	deskPulls = `{
  "id": "PR_20", "number": 20, "title": "Prove the lemma", "created_at": "2026-11-24T19:00:00Z", "updated_at": "2026-11-24T22:10:00Z",
  "author": {"type": "Bot", "login": "fc-agent"}, "association": "NONE",
  "labels": ["merge-first"], "draft": false, "head": "aaa", "head_committed_at": "2026-11-24T21:00:00Z",
  "mergeable": "MERGEABLE", "checks": "SUCCESS", "review_decision": "CHANGES_REQUESTED", "auto_merge": true, "queued": false,
  "reviews": [
    {"author": {"type": "Bot", "login": "fc-reviewer"}, "association": "NONE", "state": "APPROVED",
     "submitted_at": "2026-11-24T21:30:00Z", "commit": "aaa"},
    {"author": {"type": "User", "login": "maint"}, "association": "NONE", "state": "CHANGES_REQUESTED",
     "submitted_at": "2026-11-24T21:40:00Z", "commit": "aaa"}
  ],
  "queue_removals": [{"at": "2026-11-24T21:20:00Z", "reason": "failed_checks"}, {"at": "2026-11-24T21:50:00Z", "reason": "failed_checks"}],
  "events": [
    {"type": "convert_to_draft", "at": "2026-11-24T19:10:00Z"},
    {"type": "ready_for_review", "at": "2026-11-24T19:20:00Z"},
    {"type": "labeled", "at": "2026-11-24T20:04:12Z", "label": "merge-first", "actor": {"type": "User", "login": "maint"}},
    {"type": "of a later desk", "at": "2026-11-24T20:30:00Z", "label": "merge-first", "actor": {"type": "User", "login": "maint"}}
  ]
}, {
  "id": "PR_22", "number": 22, "title": "Draft", "created_at": "2026-11-24T23:00:00Z", "updated_at": "2026-11-24T23:00:00Z",
  "author": null, "association": "NONE",
  "labels": [], "draft": true, "head": "bbb", "head_committed_at": "2026-11-24T22:59:00Z",
  "mergeable": "UNKNOWN", "checks": "", "review_decision": "", "auto_merge": false, "queued": false,
  "reviews": [], "queue_removals": [], "events": []
}, {
  "id": "PR_23", "number": 23, "title": "Conflicting", "created_at": "2026-11-24T20:00:00Z", "updated_at": "2026-11-24T22:00:00Z",
  "author": {"type": "User", "login": "someone"}, "association": "MEMBER",
  "labels": ["needs-human", "merge-first"], "draft": false, "head": "ccc", "head_committed_at": "2026-11-24T21:00:00Z",
  "mergeable": "CONFLICTING", "checks": "FAILURE", "review_decision": "REVIEW_REQUIRED", "auto_merge": false, "queued": true,
  "reviews": [
    {"author": {"type": "Bot", "login": "fc-reviewer"}, "association": "NONE", "state": "APPROVED",
     "submitted_at": "2026-11-24T20:30:00Z", "commit": "old"},
    {"author": null, "association": "NONE", "state": "CHANGES_REQUESTED", "submitted_at": "2026-11-24T21:30:00Z", "commit": "ccc"},
    {"author": {"type": "Bot", "login": "fc-reviewer"}, "association": "NONE", "state": "COMMENTED",
     "submitted_at": "2026-11-24T21:35:00Z", "commit": ""}
  ],
  "queue_removals": [{"at": "2026-11-24T21:50:00Z", "reason": "MERGED"}],
  "events": [
    {"type": "labeled", "at": "2026-11-24T20:10:00Z", "label": "merge-first"},
    {"type": "labeled", "at": "2026-11-24T20:11:00Z", "label": "needs-human", "actor": {"type": "Bot", "login": "fc-agent"}}
  ]
}, {
  "id": "PR_24", "number": 24, "title": "Move the pin", "created_at": "2026-11-24T15:00:00Z", "updated_at": "2026-11-24T17:00:00Z",
  "author": {"type": "User", "login": "member"}, "association": "MEMBER",
  "labels": [], "draft": false, "head": "ddd", "head_committed_at": "2026-11-24T17:00:00Z",
  "mergeable": "MERGEABLE", "checks": "SUCCESS", "review_decision": "CHANGES_REQUESTED", "auto_merge": false, "queued": false,
  "reviews": [
    {"author": {"type": "Bot", "login": "fc-reviewer"}, "association": "NONE", "state": "APPROVED",
     "submitted_at": "2026-11-24T16:30:00Z", "commit": "ddd"},
    {"author": {"type": "User", "login": "other"}, "association": "MEMBER", "state": "CHANGES_REQUESTED",
     "submitted_at": "2026-11-24T16:40:00Z", "commit": "ddd"}
  ],
  "queue_removals": [{"at": "2026-11-24T16:45:00Z", "reason": "failed_checks"}],
  "events": [
    {"type": "ready_for_review", "at": "2026-11-24T16:00:00Z"}
  ]
}, {
  "id": "PR_25", "number": 25, "title": "Move the pin again", "created_at": "2026-11-24T15:30:00Z", "updated_at": "2026-11-24T15:30:00Z",
  "author": {"type": "Bot", "login": "fc-agent"}, "association": "NONE",
  "labels": [], "draft": false, "head": "eee", "head_committed_at": "2026-11-24T15:30:00Z",
  "mergeable": "MERGEABLE", "checks": "", "review_decision": "REVIEW_REQUIRED", "auto_merge": false, "queued": false,
  "reviews": [], "queue_removals": [], "events": []
}`
)

// deskNow is a minute after pull request 19 merged.
var deskNow = time.Date(2026, 11, 24, 23, 0, 0, 0, time.UTC)

// deskFakes serves GitHub's answers and a desk's. survey points at what
// the desk answers with.
func deskFakes(t *testing.T, survey *string) (*worker, *int) {
	asked := 0
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Query string }
		json.NewDecoder(r.Body).Decode(&q)
		switch {
		case r.Method != http.MethodPost || r.URL.Path != "/graphql" || strings.Contains(q.Query, "mutation"):
			t.Errorf("%s %s %q: comparing only reads", r.Method, r.URL.Path, q.Query)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		case strings.Contains(q.Query, "i16: issue(number: 16)") && !strings.Contains(q.Query, "i12:"):
			// Only issue 16 is labelled and closed by an open pull
			// request of o/r.
			asked++
			fmt.Fprint(w, `{"data": {"repository": {"i16": {"events": {"nodes": [
				{"__typename": "LabeledEvent", "createdAt": "2026-11-24T14:00:02Z", "label": {"name": "merge-first"}, "actor": {"__typename": "Bot", "login": "github-actions"}}]}}}}}`)
		case strings.Contains(q.Query, "pullRequests(states: OPEN"):
			fmt.Fprintf(w, `{"data": {"repository": {"pullRequests": {"nodes": [%s]}}}}`, githubPulls)
		case strings.Contains(q.Query, "issues(states: OPEN"):
			fmt.Fprintf(w, `{"data": {"repository": {"issues": {"nodes": [%s]}}}}`, githubIssues)
		default:
			t.Errorf("a query that a survey does not make: %q", q.Query)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(gh.Close)
	dk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer desk-token":
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case r.Method != http.MethodGet || r.URL.Path != "/survey" || r.URL.Query().Get("repo") != "o/r":
			http.NotFound(w, r)
		case strings.HasPrefix(*survey, "503 "):
			http.Error(w, (*survey)[4:], http.StatusServiceUnavailable)
		default:
			fmt.Fprint(w, *survey)
		}
	}))
	t.Cleanup(dk.Close)
	c := &Config{
		Repo: "o/r", Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]", Maintainers: []string{"maint"},
		Apps: []string{"github-actions[bot]"}, MergeFirstLimit: duration{4 * time.Hour},
		Desk: &Desk{URL: dk.URL, Token: "desk-token"},
	}
	g := &github{api: gh.URL, client: gh.Client(), token: "t", expires: time.Now().Add(time.Hour)}
	return &worker{c: c, gh: g}, &asked
}

func deskAnswer(issues, pulls string) string {
	return fmt.Sprintf(`{"repo": "o/r", "as_of": "2026-11-24T22:59:30Z", "issues": [%s], "pulls": [%s], "later": "a field this worker does not know"}`, issues, pulls)
}

// A desk's facts give the same items as GitHub's.
func TestDeskFacts(t *testing.T) {
	answer := deskAnswer(deskIssues, deskPulls)
	w, asked := deskFakes(t, &answer)
	ctx := context.Background()
	fg, err := direct{w.gh, "o/r"}.facts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := newDesk(w.c).facts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 24, 22, 59, 30, 0, time.UTC); !fd.AsOf.Equal(want) {
		t.Errorf("as of %v, want %v", fd.AsOf, want)
	}
	fromGitHub, err := fg.items(ctx, w.gh, w.c, deskNow)
	if err != nil {
		t.Fatal(err)
	}
	fromDesk, err := fd.items(ctx, w.gh, w.c, deskNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromGitHub) != 9 || len(fromDesk) != 9 {
		t.Fatalf("%d items from GitHub and %d from the desk, want 9", len(fromGitHub), len(fromDesk))
	}
	if *asked != 2 {
		t.Errorf("asked %d times who labelled issue 16, want once for each", *asked)
	}
	for i, g := range fromGitHub {
		if diff := differing(g, fromDesk[i]); len(diff) != 0 {
			t.Errorf("#%d differs: %v", g.Number, diff)
		}
	}
	// The items are what the facts say, so that the two do not agree by
	// both being empty.
	at := func(hour, min, sec int) time.Time { return time.Date(2026, 11, 24, hour, min, sec, 0, time.UTC) }
	want := []Item{
		{ID: "I_12", Number: 12, Title: "A lemma", Labels: []string{"merge-first"}, OpenBlockers: 1, ClosingPRs: 1},
		{ID: "I_13", Number: 13, Title: "Another", Outside: true, OpenSubIssues: 1, ClosingPRs: 1},
		// Labelled two seconds after the workflow opened it. A member's
		// pull request closes it, as the agent's does.
		{ID: "I_16", Number: 16, Title: "Move the pin", Labels: []string{"merge-first"}, FirstSince: at(14, 0, 2), ClosingPRs: 2},
		{ID: "I_17", Number: 17, Title: "A member's"},
		{PR: true, ID: "PR_20", Number: 20, Title: "Prove the lemma", Labels: []string{"merge-first"}, FirstSince: at(20, 4, 12),
			Ours: true, Head: "aaa", Checks: "SUCCESS", Review: "APPROVED", ReviewedAt: at(21, 30, 0), Requested: true,
			Decision: "CHANGES_REQUESTED", AutoMerge: true, Removals: 1},
		{PR: true, ID: "PR_22", Number: 22, Title: "Draft", Outside: true, Draft: true, Head: "bbb", MergeUnknown: true},
		{PR: true, ID: "PR_23", Number: 23, Title: "Conflicting", Labels: []string{"needs-human", "merge-first"},
			Head: "ccc", Conflict: true, Checks: "FAILURE", Decision: "REVIEW_REQUIRED", Queued: true},
		// First since it was marked ready, not since it was opened. Its
		// removal from the queue came before its latest commit.
		{PR: true, ID: "PR_24", Number: 24, Title: "Move the pin", FirstSince: at(16, 0, 0), FirstIssue: 16,
			Head: "ddd", Checks: "SUCCESS", Review: "APPROVED", ReviewedAt: at(16, 30, 0), Requested: true,
			Decision: "CHANGES_REQUESTED"},
		// First since it was opened.
		{PR: true, ID: "PR_25", Number: 25, Title: "Move the pin again", FirstSince: at(15, 30, 0), FirstIssue: 16,
			Ours: true, Head: "eee", Decision: "REVIEW_REQUIRED"},
	}
	for i, it := range fromDesk {
		if diff := differing(want[i], it); len(diff) != 0 {
			t.Errorf("#%d from the desk: [want, got] %v", it.Number, diff)
		}
	}
}

func TestDeskErrors(t *testing.T) {
	answer := ""
	w, _ := deskFakes(t, &answer)
	ctx := context.Background()
	for _, tc := range []struct{ name, answer, want string }{
		{"not JSON", "<html>", "desk: "},
		{"no as_of", `{"repo": "o/r", "issues": [], "pulls": []}`, "how old"},
		{"another repository", `{"repo": "o/other", "as_of": "2026-11-24T22:59:30Z", "issues": [], "pulls": []}`, `the answer is for "o/other"`},
		{"no read yet", "503 no read of o/r has worked yet", "desk: 503 Service Unavailable: no read of o/r has worked yet"},
	} {
		answer = tc.answer
		if _, err := newDesk(w.c).facts(ctx); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", tc.name, err, tc.want)
		}
	}
	d := newDesk(w.c)
	d.token = "wrong"
	if _, err := d.facts(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("wrong token: %v", err)
	}
	if strings.Contains(fmt.Sprint(d.facts(ctx)), "wrong") {
		t.Error("the error shows the token")
	}
	d = newDesk(w.c)
	d.repo = "o/other"
	if _, err := d.facts(ctx); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a repository that the desk does not follow: %v", err)
	}
}

// A desk gives the issues by number. The facts have them as GitHub does,
// the oldest first, which for a moved issue is another order.
func TestDeskOrder(t *testing.T) {
	var v deskSurvey
	err := json.Unmarshal([]byte(`{"issues": [
		{"number": 5, "created_at": "2026-11-24T12:00:00Z"}, {"number": 9, "created_at": "2026-11-24T11:00:00Z"},
		{"number": 10, "created_at": "2026-11-24T12:00:00Z"}],
		"pulls": [{"number": 11, "created_at": "2026-11-24T12:00:00Z"}, {"number": 12, "created_at": "2026-11-24T11:00:00Z"}]}`), &v)
	if err != nil {
		t.Fatal(err)
	}
	f := v.facts()
	if len(f.Issues) != 3 || f.Issues[0].Number != 9 || f.Issues[1].Number != 5 || f.Issues[2].Number != 10 {
		t.Errorf("issues %+v", f.Issues)
	}
	if len(f.Pulls) != 2 || f.Pulls[0].Number != 12 || f.Pulls[1].Number != 11 {
		t.Errorf("pulls %+v", f.Pulls)
	}
}

func TestCompare(t *testing.T) {
	answer := deskAnswer(deskIssues, deskPulls)
	w, asked := deskFakes(t, &answer)
	ctx := context.Background()
	cmp, err := w.compare(ctx, deskNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Differ) != 0 || cmp.GitHub != (counts{4, 5}) || cmp.Desk != (counts{4, 5}) {
		t.Fatalf("the same facts: %+v", cmp)
	}
	if cmp.AsOf.IsZero() || cmp.At.Before(cmp.AsOf) || cmp.At.Nanosecond() != 0 || cmp.At.Location() != time.UTC {
		t.Errorf("at %v, as of %v", cmp.At, cmp.AsOf)
	}
	if *asked != 2 {
		t.Errorf("asked %d times who labelled issue 16, want once for each side", *asked)
	}

	// On the desk, issue 13 is not there, issue 30 is, and pull request 20
	// is in the merge queue with no request for changes.
	first, rest, _ := strings.Cut(deskIssues, "\n}, {\n")
	_, rest, _ = strings.Cut(rest, "\n}, {\n")
	extra := `{"id": "I_30", "number": 30, "title": "Gone", "author": {"type": "Bot", "login": "fc-agent"}, "updated_at": "2026-11-24T10:00:00Z", "labels": [], "blocked_by": [], "sub_issues": [], "closing_pulls": []}`
	pulls := strings.Replace(deskPulls, `"auto_merge": true, "queued": false`, `"auto_merge": true, "queued": true`, 1)
	pulls = strings.Replace(pulls, `"review_decision": "CHANGES_REQUESTED"`, `"review_decision": "APPROVED"`, 1)
	pulls = strings.Replace(pulls, `"state": "CHANGES_REQUESTED",
     "submitted_at": "2026-11-24T21:40:00Z"`, `"state": "COMMENTED",
     "submitted_at": "2026-11-24T21:40:00Z"`, 1)
	pulls = strings.Replace(pulls, `"updated_at": "2026-11-24T22:10:00Z"`, `"updated_at": "2026-11-24T22:05:00Z"`, 1)
	answer = deskAnswer(first+"\n}, {\n"+rest+", "+extra, pulls)
	cmp, err = w.compare(ctx, deskNow)
	if err != nil {
		t.Fatal(err)
	}
	if cmp.GitHub != (counts{4, 5}) || cmp.Desk != (counts{4, 5}) || len(cmp.Differ) != 3 {
		t.Fatalf("%+v", cmp)
	}
	d := cmp.Differ
	if d[0].Item != "issue-13" || d[0].Only != "github" || d[0].Updated[0] == nil || d[0].Updated[1] != nil {
		t.Errorf("issue 13: %+v", d[0])
	}
	if d[1].Item != "issue-30" || d[1].Only != "desk" || d[1].Jobs != [2]string{"", "work"} || d[1].Updated[0] != nil ||
		d[1].Updated[1] == nil || !d[1].Updated[1].Equal(time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("issue 30: %+v", d[1])
	}
	want := map[string][2]any{"Queued": {false, true}, "Requested": {true, false}, "Decision": {"CHANGES_REQUESTED", "APPROVED"}}
	if d[2].Item != "pr-20" || d[2].Only != "" || !reflect.DeepEqual(d[2].Fields, want) {
		t.Errorf("pull request 20: %+v", d[2])
	}
	// GitHub's facts ask for a fix job, and the desk's for none.
	if d[2].Jobs != [2]string{"fix", ""} {
		t.Errorf("pull request 20: jobs %q", d[2].Jobs)
	}
	at := func(min int) time.Time { return time.Date(2026, 11, 24, 22, min, 0, 0, time.UTC) }
	if u := d[2].Updated; u[0] == nil || u[1] == nil || !u[0].Equal(at(10)) || !u[1].Equal(at(5)) {
		t.Errorf("pull request 20: updated %v", u)
	}
}
