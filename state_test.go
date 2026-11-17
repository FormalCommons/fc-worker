package main

import (
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	c := &Config{Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]"}
	t0 := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	// ours makes it a pull request that the agent App opened, with checks
	// passing unless the case says otherwise.
	ours := func(it Item) Item {
		it.PR, it.Author = true, "fc-agent"
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
		{"in review", Item{OpenPRs: 1}, ""},
		{"outside issue", Item{Outside: true}, ""},
		{"outside PR", Item{PR: true, Outside: true, Author: "mallory", Checks: "SUCCESS"}, ""},

		{"draft", Item{PR: true, Draft: true, Author: "fc-agent", Conflict: true}, ""},
		{"parked PR", ours(Item{Labels: []string{"needs-human"}, Conflict: true}), ""},
		{"conflict", ours(Item{Conflict: true}), "fix"},
		{"checks failed", ours(Item{Checks: "FAILURE"}), "fix"},
		{"checks errored", ours(Item{Checks: "ERROR"}), "fix"},
		{"changes requested", ours(Item{Review: "CHANGES_REQUESTED"}), "fix"},
		{"changes requested, checks running", ours(Item{Review: "CHANGES_REQUESTED", Checks: "PENDING"}), "fix"},
		{"a person requested changes", ours(Item{Requested: true, Review: "APPROVED"}), "fix"},
		{"person's PR, a person requested changes", Item{PR: true, Author: "alice", Checks: "SUCCESS", Requested: true}, ""},
		{"conflict after approval", ours(Item{Conflict: true, Review: "APPROVED"}), "fix"},
		{"removed from queue", ours(Item{Review: "APPROVED", ReviewedAt: t0, RemovedAt: t0.Add(time.Hour)}), "fix"},
		{"removed before approval", ours(Item{Review: "APPROVED", ReviewedAt: t0, RemovedAt: t0.Add(-time.Hour)}), autoMerge},
		{"checks running", ours(Item{Checks: "PENDING"}), ""},
		{"no checks yet", Item{PR: true, Author: "fc-agent"}, ""},
		{"not reviewed", ours(Item{}), "review"},
		{"approved", ours(Item{Review: "APPROVED"}), autoMerge},
		{"approved, auto-merge on", ours(Item{Review: "APPROVED", AutoMerge: true}), ""},
		{"approved, queued", ours(Item{Review: "APPROVED", Queued: true}), ""},
		{"approved, blocked by a person", ours(Item{Review: "APPROVED", Blocked: true}), park},

		{"person's PR, not reviewed", Item{PR: true, Author: "alice", Checks: "SUCCESS"}, "review"},
		{"person's PR, conflict", Item{PR: true, Author: "alice", Conflict: true}, ""},
		{"person's PR, changes requested", Item{PR: true, Author: "alice", Checks: "SUCCESS", Review: "CHANGES_REQUESTED"}, ""},
		{"person's PR, approved", Item{PR: true, Author: "alice", Checks: "SUCCESS", Review: "APPROVED"}, ""},
	} {
		if got, why := next(tc.it, c); got != tc.want {
			t.Errorf("%s: got %q (%s), want %q", tc.name, got, why, tc.want)
		}
	}
}

func TestVerdicts(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := func(login, state, commit string, at time.Time) review {
		var v review
		v.Author.Login, v.State, v.Commit.Oid, v.SubmittedAt = login, state, commit, at
		return v
	}
	got := verdicts([]review{
		r("fc-reviewer", "CHANGES_REQUESTED", "a", t0),
		r("fc-reviewer", "APPROVED", "b", t0.Add(1*time.Hour)),
		r("alice", "CHANGES_REQUESTED", "b", t0.Add(2*time.Hour)),
		r("fc-reviewer", "COMMENTED", "b", t0.Add(3*time.Hour)),
		r("bob", "CHANGES_REQUESTED", "b", t0.Add(4*time.Hour)),
		r("bob", "APPROVED", "b", t0.Add(5*time.Hour)),
	}, "b")
	if v := got["fc-reviewer"]; v.State != "APPROVED" || !v.SubmittedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("fc-reviewer: %+v, want the approval of b", v)
	}
	if got["alice"].State != "CHANGES_REQUESTED" || got["bob"].State != "APPROVED" || len(got) != 3 {
		t.Errorf("verdicts %+v", got)
	}
}

func TestOutside(t *testing.T) {
	c := &Config{Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]"}
	for _, tc := range []struct {
		author      *actor
		association string
		want        bool
	}{
		{&actor{"fc-agent"}, "NONE", false},
		{&actor{"fc-reviewer[bot]"}, "NONE", false},
		{&actor{"alice"}, "OWNER", false},
		{&actor{"alice"}, "MEMBER", false},
		{&actor{"alice"}, "COLLABORATOR", false},
		{&actor{"mallory"}, "CONTRIBUTOR", true},
		{&actor{"mallory"}, "NONE", true},
		{nil, "NONE", true},
	} {
		if got := outside(tc.author, tc.association, c); got != tc.want {
			t.Errorf("%v %s: outside %v, want %v", tc.author, tc.association, got, tc.want)
		}
	}
}
