package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Item is an open issue or pull request, cut to what decides its next job.
type Item struct {
	PR      bool
	ID      string // GraphQL node ID
	Number  int
	Title   string
	Labels  []string
	Outside bool // opened by someone outside the project
	// FirstSince is since when a pull request merges first, or since when
	// an issue's label merge-first counts; zero if neither: see survey.
	FirstSince time.Time

	// Issues only.
	OpenBlockers  int // open issues that block this one
	OpenSubIssues int
	ClosingPRs    int // pull requests that will close this issue: see closing

	// Pull requests only.
	Draft        bool
	Ours         bool      // opened by the agent App
	Head         string    // the latest commit
	Conflict     bool      // the branch conflicts with the base
	MergeUnknown bool      // GitHub has not yet worked out whether it conflicts
	Checks       string    // on the latest commit: SUCCESS, FAILURE, ERROR, PENDING, EXPECTED or ""
	Review       string    // the reviewer's latest review of the latest commit: APPROVED, CHANGES_REQUESTED or ""
	ReviewedAt   time.Time // when the reviewer gave Review
	Requested    bool      // a person in the project requests changes to the latest commit
	Decision     string    // GitHub's sum of the reviews that count: APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, or "" if none are required
	AutoMerge    bool
	Queued       bool
	Removals     int // times it left the merge queue without merging since the reviewer's verdict and the latest commit, counting at most 2
	FirstIssue   int // the issue it closes whose label merge-first counts, or 0
}

// Next jobs that the worker does by itself, without an agent.
const (
	autoMerge = "auto-merge"
	park      = "park" // label needs-human
)

// mergeFirst labels a pull request that merges before all others, or an
// issue whose pull request does: see first.
const mergeFirst = "merge-first"

// next returns the job an item needs, or "" if it needs none now, and why.
// f is the number of the pull request that merges first, or 0 if there is
// none: see first.
func next(it Item, c *Config, f int) (job, why string) {
	has := func(l string) bool { return slices.Contains(it.Labels, l) }
	if has("needs-human") {
		return "", "labelled needs-human"
	}
	if it.Draft {
		// A work job that runs out of time opens its pull request as a
		// draft, and continue jobs go on with it until one marks it ready.
		// Checks, conflicts and reviews wait until then.
		if it.Ours {
			return "continue", "a draft, so its work is unfinished"
		}
		return "", "draft; its author decides when it is ready"
	}
	if it.Outside {
		return "", "opened by someone outside the project; a maintainer decides"
	}
	if !it.PR {
		switch {
		case has("proposed"):
			return "triage", "proposed by an agent"
		case has("close-proposed"):
			return "triage", "proposed for closing by an agent"
		case it.OpenBlockers > 0:
			return "", "blocked by open issues"
		case it.OpenSubIssues > 0:
			return "", "has open sub-issues"
		case it.ClosingPRs > 0:
			return "", "a pull request will close it"
		}
		return "work", "ready"
	}

	fix := ""
	switch {
	case it.Conflict:
		fix = "conflicts with the base branch"
	case it.Checks == "FAILURE" || it.Checks == "ERROR":
		fix = "checks failed"
	case it.Review == "CHANGES_REQUESTED":
		fix = "the reviewer requested changes"
	case it.Requested:
		fix = "a person requested changes"
	case it.Review == "APPROVED" && it.Removals > 1 && !it.Queued && !it.AutoMerge:
		fix = "removed from the merge queue twice since its approval and its latest commit"
	}
	switch {
	case fix != "" && !it.Ours:
		return "", fix + "; waiting for its author"
	case fix != "":
		return "fix", fix
	case it.MergeUnknown:
		// Right after a merge, GitHub works out again whether each pull
		// request conflicts. Until it knows, a review or auto-merge may be
		// wasted on one that needs a fix.
		return "", "GitHub is still checking whether it conflicts with the base branch"
	case it.Checks != "SUCCESS":
		return "", "waiting for checks"
	case it.Review == "":
		return "review", "latest commit not reviewed"
	case !it.Ours:
		return "", "approved; its author decides when to merge"
	// The reviewer approved. Decision says whether that is enough for
	// GitHub: CHANGES_REQUESTED while anyone's request for changes stands,
	// and REVIEW_REQUIRED if the reviewer's approval does not count, as
	// when its App lacks write access, or the ruleset requires more. Either
	// way the merge queue would never take the pull request.
	case it.Decision == "CHANGES_REQUESTED":
		return park, "approved by the reviewer, but a person's request for changes stands"
	case it.Decision == "REVIEW_REQUIRED":
		return park, "approved by the reviewer, but GitHub still requires a review: the reviewer's approval does not count, or more are required"
	// While another pull request merges first, this one stays out of the
	// merge queue, since its merge could give that one a new conflict.
	case f != 0 && f != it.Number && !it.AutoMerge && !it.Queued:
		return "", fmt.Sprintf("approved; waiting until #%d, which merges first, has merged", f)
	// Removed from the merge queue once since the approval and the latest
	// commit: put it back once before a fix job, since the cause may be
	// outside the code, such as a failed download in CI. A second removal
	// leads to a fix. A commit that the fix pushes starts the count again,
	// even a bare merge of the base branch, onto which GitHub moves the
	// approval; max_rounds bounds these rounds, and idle_rounds parks a pull
	// request whose fixes push nothing.
	case !it.AutoMerge && !it.Queued && it.Removals == 1:
		return autoMerge, "removed from the merge queue once since its approval and its latest commit; it goes back in once before a fix"
	case !it.AutoMerge && !it.Queued:
		return autoMerge, "approved"
	}
	return "", "approved, waiting for the merge queue"
}

// order is the order in which jobs are picked.
var order = []string{park, autoMerge, "fix", "review", "continue", "triage", "work"}

// first returns the number of the pull request that merges first, or 0 if
// there is none. It is the pull request with the lowest number among those
// from the project that are neither drafts nor parked and for which a
// label merge-first has counted for less than merge_first_limit: its own,
// from a maintainer, or that of an issue that it closes. Items come in the
// order of their numbers. While there is one, the workers put no other
// pull request in the merge queue, and pick its jobs before all others.
// The label is for a pull request that changes so many files that the
// merges of others would give it a new conflict before its checks, its
// review and the merge queue are through, such as a move to a new version
// of a dependency. Pull requests that are in the merge queue already, or
// have auto-merge on, still merge. The limit keeps a pull request that
// never gets through from holding back the others for good.
func first(items []Item, c *Config, now time.Time) int {
	for _, it := range items {
		if it.PR && !it.Draft && !it.Outside && !slices.Contains(it.Labels, "needs-human") &&
			!it.FirstSince.IsZero() && now.Sub(it.FirstSince) < c.MergeFirstLimit.Duration {
			return it.Number
		}
	}
	return 0
}

// firstNote says, for a pull request with a label merge-first, its own or
// that of an issue that it closes, until when it merges first, or why it
// does not, and for an issue with the label, whether the label counts. It
// is "" for any other item. f is the number that first returned.
func firstNote(it Item, f int, c *Config, now time.Time) string {
	labelled := slices.Contains(it.Labels, mergeFirst)
	what := "labelled " + mergeFirst
	if !labelled {
		what = fmt.Sprintf("closes #%d, labelled %s", it.FirstIssue, mergeFirst)
	}
	no := what + ", which does not count: "
	until := it.FirstSince.Add(c.MergeFirstLimit.Duration)
	switch {
	case !labelled && it.FirstIssue == 0:
		return ""
	case !it.PR && it.FirstSince.IsZero():
		return no + "neither a maintainer nor an App in the config added it, or 20 labels were added since"
	case !it.PR:
		return what + ": a pull request that closes it merges first"
	case it.Number == f:
		return "merges first until " + until.UTC().Format("15:04 UTC")
	case it.Draft:
		return no + "a draft"
	case it.Outside:
		return no + "from outside the project"
	case slices.Contains(it.Labels, "needs-human"):
		return no + "parked"
	case it.FirstSince.IsZero():
		return no + "no maintainer in the config added it, or 20 labels were added since"
	case !now.Before(until):
		return what + ", which counted until " + until.UTC().Format("15:04 UTC")
	}
	return fmt.Sprintf("%s; #%d goes before it", what, f)
}

// event is the adding of a label to an issue or pull request, or a pull
// request's becoming ready for review; only the first has a label and an
// actor.
type event struct {
	Typename  string    `json:"__typename"`
	CreatedAt time.Time `json:"createdAt"`
	Label     struct {
		Name string `json:"name"`
	} `json:"label"`
	Actor *actor `json:"actor"`
}

// firstSince returns when the label merge-first was last added to an issue
// or pull request that has it, from its latest events, oldest first. The
// result is zero unless the one who added it counts. On a pull request
// that is a maintainer in the config. The agent App can label anything, so
// a misled agent could otherwise hold back every merge. On an issue it is
// a maintainer or an App in the config's apps, so that a workflow can
// label an issue that it opens. An agent could make such an App label an
// issue, through the code that workflows run, so this trusts the agents
// with merge_first_limit for each pull request that closes the issue.
// GitHub does not say how the one who added a label is associated with
// the repository, so its owner, organization members and collaborators
// count only if the config lists them.
func firstSince(labels []string, events []event, c *Config, issue bool) time.Time {
	if !slices.Contains(labels, mergeFirst) {
		return time.Time{}
	}
	var last event
	for _, e := range events {
		if e.Label.Name == mergeFirst {
			last = e
		}
	}
	a := last.Actor
	switch {
	case a == nil:
	case a.Typename == "User" && slices.ContainsFunc(c.Maintainers, func(m string) bool { return strings.EqualFold(m, a.Login) }):
		return last.CreatedAt
	case issue && slices.ContainsFunc(c.Apps, a.is):
		return last.CreatedAt
	}
	return time.Time{}
}

// ready returns when a pull request became ready for review: when it was
// last marked so, if that is among its latest events, oldest first, and
// otherwise when it was opened.
func ready(opened time.Time, events []event) time.Time {
	at := opened
	for _, e := range events {
		if e.Typename == "ReadyForReviewEvent" {
			at = e.CreatedAt
		}
	}
	return at
}

// firstIssue is an open issue whose label merge-first counts.
type firstIssue struct {
	Number int
	Since  time.Time // when the label was added
	PRs    []int     // the pull requests that close it
}

// firstIssuesQuery asks for the open issues labelled merge-first, the
// oldest ten. It costs one point.
const firstIssuesQuery = `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    issues(states: OPEN, first: 10, labels: ["` + mergeFirst + `"], orderBy: {field: CREATED_AT, direction: ASC}) {
      nodes {
        number
        labels(first: 30) { nodes { name } }
        events: timelineItems(last: 20, itemTypes: [LABELED_EVENT]) {
          nodes { __typename ... on LabeledEvent { createdAt label { name } actor { __typename login } } }
        }
        closedByPullRequestsReferences(first: 20) { nodes { number } }
      }
    }
  }
}`

// firstIssues returns the open issues whose label merge-first counts,
// oldest first. Who added a label is not in the survey of all issues,
// where it would cost a point more for each page, so survey asks only
// while an issue has the label.
func firstIssues(ctx context.Context, g *github, c *Config) ([]firstIssue, error) {
	owner, name, _ := strings.Cut(c.Repo, "/")
	var r struct {
		Repository struct {
			Issues struct {
				Nodes []struct {
					Number int   `json:"number"`
					Labels names `json:"labels"`
					Events struct {
						Nodes []event `json:"nodes"`
					} `json:"events"`
					ClosedBy struct {
						Nodes []struct {
							Number int `json:"number"`
						} `json:"nodes"`
					} `json:"closedByPullRequestsReferences"`
				} `json:"nodes"`
			} `json:"issues"`
		} `json:"repository"`
	}
	if err := g.graphql(ctx, firstIssuesQuery, vars(owner, name, ""), &r); err != nil {
		return nil, err
	}
	var out []firstIssue
	for _, n := range r.Repository.Issues.Nodes {
		since := firstSince(n.Labels.list(), n.Events.Nodes, c, true)
		if since.IsZero() {
			continue
		}
		fi := firstIssue{Number: n.Number, Since: since}
		for _, pr := range n.ClosedBy.Nodes {
			fi.PRs = append(fi.PRs, pr.Number)
		}
		out = append(out, fi)
	}
	return out, nil
}

// issueOutside reports whether an issue's author is outside the project.
// The config's apps count as in it for issues alone: their pull requests and
// reviews would need jobs that nothing runs for them.
func issueOutside(author *actor, association string, c *Config) bool {
	return outside(author, association, c) && !slices.ContainsFunc(c.Apps, author.is)
}

// outside reports whether an author is outside the project: neither of the
// two Apps, nor a maintainer in the config, nor the repository's owner, an
// organization member or a collaborator. A deleted account is outside too.
func outside(author *actor, association string, c *Config) bool {
	if author == nil {
		return true
	}
	if author.is(c.Agent) || author.is(c.Reviewer) {
		return false
	}
	if author.Typename == "User" && slices.ContainsFunc(c.Maintainers, func(m string) bool { return strings.EqualFold(m, author.Login) }) {
		return false
	}
	return association != "OWNER" && association != "MEMBER" && association != "COLLABORATOR"
}

const issuesQuery = `query($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    issues(states: OPEN, first: 50, after: $after, orderBy: {field: CREATED_AT, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id number title authorAssociation
        author { __typename login }
        labels(first: 30) { nodes { name } }
        blockedBy(first: 50) { nodes { state } }
        subIssues(first: 100) { nodes { state } }
        closedByPullRequestsReferences(first: 20) { nodes { state mergedAt authorAssociation author { __typename login } } }
      }
    }
  }
}`

const pullsQuery = `query($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, first: 25, after: $after, orderBy: {field: CREATED_AT, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id number title createdAt isDraft headRefOid mergeable reviewDecision isInMergeQueue authorAssociation
        author { __typename login }
        labels(first: 30) { nodes { name } }
        autoMergeRequest { enabledAt }
        commits(last: 1) { nodes { commit { committedDate statusCheckRollup { state } } } }
        reviews(last: 100) { nodes { author { __typename login } authorAssociation state submittedAt commit { oid } } }
        timelineItems(last: 3, itemTypes: [REMOVED_FROM_MERGE_QUEUE_EVENT]) {
          nodes { ... on RemovedFromMergeQueueEvent { createdAt reason } }
        }
        events: timelineItems(last: 20, itemTypes: [LABELED_EVENT, READY_FOR_REVIEW_EVENT]) {
          nodes { __typename ... on LabeledEvent { createdAt label { name } actor { __typename login } } ... on ReadyForReviewEvent { createdAt } }
        }
      }
    }
  }
}`

type page struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type names struct {
	Nodes []struct {
		Name string `json:"name"`
	} `json:"nodes"`
}

type states struct {
	Nodes []struct {
		State string `json:"state"`
	} `json:"nodes"`
}

func (s states) open() int {
	n := 0
	for _, x := range s.Nodes {
		if x.State == "OPEN" {
			n++
		}
	}
	return n
}

func (l names) list() []string {
	var out []string
	for _, x := range l.Nodes {
		out = append(out, x.Name)
	}
	return out
}

// survey returns every open issue and pull request, oldest first.
func survey(ctx context.Context, g *github, c *Config) ([]Item, error) {
	owner, name, _ := strings.Cut(c.Repo, "/")
	var items []Item
	for after := ""; ; {
		var r struct {
			Repository struct {
				Issues struct {
					PageInfo page `json:"pageInfo"`
					Nodes    []struct {
						ID                string `json:"id"`
						Number            int    `json:"number"`
						Title             string `json:"title"`
						AuthorAssociation string `json:"authorAssociation"`
						Author            *actor `json:"author"`
						Labels            names  `json:"labels"`
						BlockedBy         states `json:"blockedBy"`
						SubIssues         states `json:"subIssues"`
						ClosedBy          struct {
							Nodes []closingPR `json:"nodes"`
						} `json:"closedByPullRequestsReferences"`
					} `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		}
		if err := g.graphql(ctx, issuesQuery, vars(owner, name, after), &r); err != nil {
			return nil, err
		}
		for _, n := range r.Repository.Issues.Nodes {
			it := Item{
				ID: n.ID, Number: n.Number, Title: n.Title, Labels: n.Labels.list(),
				Outside:      issueOutside(n.Author, n.AuthorAssociation, c),
				OpenBlockers: n.BlockedBy.open(), OpenSubIssues: n.SubIssues.open(),
				ClosingPRs: closing(n.ClosedBy.Nodes, c, time.Now()),
			}
			items = append(items, it)
		}
		p := r.Repository.Issues.PageInfo
		if !p.HasNextPage {
			break
		}
		after = p.EndCursor
	}
	// The pull requests that close an issue whose label merge-first
	// counts, with the oldest such issue of each.
	closes := map[int]firstIssue{}
	if slices.ContainsFunc(items, func(it Item) bool { return slices.Contains(it.Labels, mergeFirst) }) {
		issues, err := firstIssues(ctx, g, c)
		if err != nil {
			return nil, err
		}
		for _, fi := range issues {
			if i := slices.IndexFunc(items, func(it Item) bool { return it.Number == fi.Number }); i >= 0 {
				items[i].FirstSince = fi.Since
			}
			for _, pr := range fi.PRs {
				if _, ok := closes[pr]; !ok {
					closes[pr] = fi
				}
			}
		}
	}
	for after := ""; ; {
		var r struct {
			Repository struct {
				PullRequests struct {
					PageInfo page `json:"pageInfo"`
					Nodes    []struct {
						ID                string    `json:"id"`
						Number            int       `json:"number"`
						Title             string    `json:"title"`
						CreatedAt         time.Time `json:"createdAt"`
						IsDraft           bool      `json:"isDraft"`
						HeadRefOid        string    `json:"headRefOid"`
						Mergeable         string    `json:"mergeable"`
						ReviewDecision    string    `json:"reviewDecision"`
						IsInMergeQueue    bool      `json:"isInMergeQueue"`
						AuthorAssociation string    `json:"authorAssociation"`
						Author            *actor    `json:"author"`
						Labels            names     `json:"labels"`
						AutoMergeRequest  *struct{} `json:"autoMergeRequest"`
						Commits           struct {
							Nodes []struct {
								Commit struct {
									CommittedDate     time.Time `json:"committedDate"`
									StatusCheckRollup *struct {
										State string `json:"state"`
									} `json:"statusCheckRollup"`
								} `json:"commit"`
							} `json:"nodes"`
						} `json:"commits"`
						Reviews struct {
							Nodes []review `json:"nodes"`
						} `json:"reviews"`
						TimelineItems struct {
							Nodes []removal `json:"nodes"`
						} `json:"timelineItems"`
						Events struct {
							Nodes []event `json:"nodes"`
						} `json:"events"`
					} `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		if err := g.graphql(ctx, pullsQuery, vars(owner, name, after), &r); err != nil {
			return nil, err
		}
		for _, n := range r.Repository.PullRequests.Nodes {
			it := Item{
				PR: true, ID: n.ID, Number: n.Number, Title: n.Title, Labels: n.Labels.list(),
				Outside: outside(n.Author, n.AuthorAssociation, c),
				Draft:   n.IsDraft, Head: n.HeadRefOid,
				Conflict: n.Mergeable == "CONFLICTING", MergeUnknown: n.Mergeable == "UNKNOWN",
				Decision: n.ReviewDecision, AutoMerge: n.AutoMergeRequest != nil, Queued: n.IsInMergeQueue,
			}
			it.Ours = n.Author.is(c.Agent)
			var headAt time.Time
			if c := n.Commits.Nodes; len(c) > 0 {
				headAt = c[0].Commit.CommittedDate
				if c[0].Commit.StatusCheckRollup != nil {
					it.Checks = c[0].Commit.StatusCheckRollup.State
				}
			}
			for _, r := range verdicts(n.Reviews.Nodes, it.Head) {
				switch {
				case r.Author.is(c.Reviewer):
					it.Review, it.ReviewedAt = r.State, r.SubmittedAt
				case r.State == "CHANGES_REQUESTED" && !outside(&r.Author, r.AuthorAssociation, c):
					it.Requested = true
				}
			}
			it.Removals = removals(n.TimelineItems.Nodes, it.ReviewedAt, headAt, time.Now())
			// It merges first since a maintainer labelled it, or since it
			// closes a labelled issue and is ready for review, whichever
			// came later.
			it.FirstSince = firstSince(it.Labels, n.Events.Nodes, c, false)
			if fi, ok := closes[it.Number]; ok {
				it.FirstIssue = fi.Number
				since := fi.Since
				if at := ready(n.CreatedAt, n.Events.Nodes); at.After(since) {
					since = at
				}
				if since.After(it.FirstSince) {
					it.FirstSince = since
				}
			}
			items = append(items, it)
		}
		p := r.Repository.PullRequests.PageInfo
		if !p.HasNextPage {
			break
		}
		after = p.EndCursor
	}
	slices.SortFunc(items, func(a, b Item) int { return a.Number - b.Number })
	return items, nil
}

// removal is a pull request's leaving the merge queue. Its reason is
// "merged" when it left by merging, and otherwise something like
// "failed_checks" or "merge_conflict".
type removal struct {
	CreatedAt time.Time `json:"createdAt"`
	Reason    string    `json:"reason"`
}

// removals counts the removals that were not merges and came after both
// the reviewer's verdict and the latest commit. A commit dated after now,
// by a wrong clock, does not count, since it would hide every removal and
// so re-queue the pull request without end.
func removals(removed []removal, verdict, head, now time.Time) int {
	since := verdict
	if head.After(since) && !head.After(now) {
		since = head
	}
	n := 0
	for _, r := range removed {
		if r.CreatedAt.After(since) && !strings.EqualFold(r.Reason, "merged") {
			n++
		}
	}
	return n
}

// closingPR is a pull request that closes an issue when it merges.
type closingPR struct {
	State             string    `json:"state"`
	MergedAt          time.Time `json:"mergedAt"`
	AuthorAssociation string    `json:"authorAssociation"`
	Author            *actor    `json:"author"`
}

// closeDelay is how long an issue may still look open after a pull request
// that closes it merges. GitHub closes it within seconds; an issue that is
// still open long after is open for another reason, such as a person
// reopening it.
const closeDelay = 5 * time.Minute

// closing counts the pull requests that will close an issue: open ones from
// the project, and ones that merged less than closeDelay ago. An open pull
// request from outside the project does not hold up an issue, since nothing
// moves it.
func closing(prs []closingPR, c *Config, now time.Time) int {
	n := 0
	for _, pr := range prs {
		switch {
		case pr.State == "OPEN" && !outside(pr.Author, pr.AuthorAssociation, c):
			n++
		case pr.State == "MERGED" && now.Sub(pr.MergedAt) < closeDelay:
			n++
		}
	}
	return n
}

// actor is the author of an issue, pull request, review or comment.
type actor struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

// is reports whether a is the GitHub App whose login is app, such as
// "fc-agent[bot]". GitHub gives an App's login with or without "[bot]",
// and a person may have the same login without it, so only a Bot counts.
func (a *actor) is(app string) bool {
	return a != nil && a.Typename == "Bot" &&
		strings.EqualFold(strings.TrimSuffix(a.Login, "[bot]"), strings.TrimSuffix(app, "[bot]"))
}

type review struct {
	Author            actor     `json:"author"`
	AuthorAssociation string    `json:"authorAssociation"`
	State             string    `json:"state"`
	SubmittedAt       time.Time `json:"submittedAt"`
	Commit            struct {
		Oid string `json:"oid"`
	} `json:"commit"`
}

// verdicts returns each reviewer's latest verdict on the head commit.
// Reviews come oldest first. Reviews that only comment are not verdicts.
func verdicts(reviews []review, head string) map[actor]review {
	latest := map[actor]review{}
	for _, r := range reviews {
		if r.Commit.Oid == head && (r.State == "APPROVED" || r.State == "CHANGES_REQUESTED") {
			latest[r.Author] = r
		}
	}
	return latest
}

func vars(owner, name, after string) map[string]any {
	v := map[string]any{"owner": owner, "name": name}
	if after != "" {
		v["after"] = after
	}
	return v
}
