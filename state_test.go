package main

import (
	"fmt"
	"slices"
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
	for _, tc := range []struct {
		name string
		it   Item
		want string
	}{
		{"it is approved", pr(7, Item{Review: "APPROVED"}), autoMerge},
		{"it left the queue once", pr(7, Item{Review: "APPROVED", Removals: 1}), autoMerge},
		{"another is approved", pr(8, Item{Review: "APPROVED"}), ""},
		{"another left the queue once", pr(8, Item{Review: "APPROVED", Removals: 1}), ""},
		{"another left the queue twice", pr(8, Item{Review: "APPROVED", Removals: 2}), "fix"},
		{"another's approval does not count", pr(8, Item{Review: "APPROVED", Decision: "REVIEW_REQUIRED"}), park},
		{"another is not reviewed", pr(8, Item{}), "review"},
		{"another has a conflict", pr(8, Item{Conflict: true}), "fix"},
		{"an issue", Item{Number: 9}, "work"},
	} {
		if got, why := next(tc.it, c, 7); got != tc.want {
			t.Errorf("%s: got %q (%s), want %q", tc.name, got, why, tc.want)
		}
	}
}

func TestFirst(t *testing.T) {
	c := &Config{MergeFirstLimit: duration{4 * time.Hour}}
	now := time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)
	held := Item{Number: 7, PR: true, Labels: []string{mergeFirst}, FirstSince: since}
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
	} {
		if got := first(tc.items, c, now); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestFirstSince(t *testing.T) {
	c := &Config{Agent: "fc-agent[bot]", Reviewer: "fc-reviewer[bot]", Maintainers: []string{"maint"}, Apps: []string{"github-actions[bot]"}}
	t0 := time.Date(2026, 11, 24, 19, 0, 0, 0, time.UTC)
	add := func(name string, a *actor, d time.Duration) labeled {
		l := labeled{CreatedAt: t0.Add(d), Actor: a}
		l.Label.Name = name
		return l
	}
	maint, agent := &actor{"User", "Maint"}, &actor{"Bot", "fc-agent"}
	has := []string{"other", mergeFirst}
	for _, tc := range []struct {
		name   string
		labels []string
		added  []labeled
		want   time.Time
	}{
		{"not labelled", []string{"other"}, []labeled{add("other", maint, 0)}, time.Time{}},
		{"label removed again", []string{"other"}, []labeled{add(mergeFirst, maint, 0)}, time.Time{}},
		{"by a maintainer", has, []labeled{add(mergeFirst, maint, 0), add("other", agent, time.Hour)}, t0},
		{"by a listed App", has, []labeled{add(mergeFirst, &actor{"Bot", "github-actions"}, 0)}, t0},
		{"by the agent App", has, []labeled{add(mergeFirst, agent, 0)}, time.Time{}},
		{"by the reviewer App", has, []labeled{add(mergeFirst, &actor{"Bot", "fc-reviewer"}, 0)}, time.Time{}},
		{"by a person outside the config", has, []labeled{add(mergeFirst, &actor{"User", "mallory"}, 0)}, time.Time{}},
		{"by an App with a maintainer's name", has, []labeled{add(mergeFirst, &actor{"Bot", "maint"}, 0)}, time.Time{}},
		{"by a deleted account", has, []labeled{add(mergeFirst, nil, 0)}, time.Time{}},
		{"added again by a maintainer", has, []labeled{add(mergeFirst, agent, 0), add(mergeFirst, maint, time.Hour)}, t0.Add(time.Hour)},
		{"added again by an agent", has, []labeled{add(mergeFirst, maint, 0), add(mergeFirst, agent, time.Hour)}, time.Time{}},
		{"its adding is not among the latest", has, []labeled{add("other", maint, 0)}, time.Time{}},
	} {
		if got := firstSince(tc.labels, tc.added, c); !got.Equal(tc.want) {
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
