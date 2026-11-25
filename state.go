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
	// the label merge-first counts on an issue that needs a job or that an
	// open pull request closes; zero if neither: see survey.
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
	// IssueSince is since when the label merge-first counts on an issue
	// that a pull request closes, the latest if there are several; zero
	// if there is none.
	IssueSince time.Time
}

// Next jobs that the worker does by itself, without an agent.
const (
	autoMerge = "auto-merge"
	park      = "park" // label needs-human
)

// mergeFirst labels a pull request that merges before all others, or an
// issue whose pull request does: see first and leads.
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
// from a maintainer, or that of an issue that it closes, from when it was
// ready for review. Items come in the order of their numbers. While there
// is one, the workers put no other pull request in the merge queue, and
// pick its jobs before all others.
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

// leads reports whether an item's jobs lead to a pull request that can
// merge first: those of an issue whose label merge-first counts, and those
// of the agent's draft that closes such an issue, both within
// firstIssueSpan of the label, after which a pull request no longer begins
// to merge first. The workers pick them before all others but the jobs of
// the pull request that merges first. Without this, the work on a labelled
// issue would wait behind every other job, since work is the last in order,
// and the others would go on merging what the change must then follow. It
// holds nothing back: a draft does not merge first. A label on the draft
// itself gives its jobs nothing.
func leads(it Item, now time.Time) bool {
	since := it.FirstSince
	if it.PR {
		if !it.Draft || !it.Ours {
			return false
		}
		since = it.IssueSince
	}
	return !since.IsZero() && now.Sub(since) < firstIssueSpan
}

// firstNote says, for a pull request with a label merge-first, its own or
// that of an issue that it closes, until when it merges first, or why it
// does not, and for an issue with the label, whether the label counts. It
// is "" for any other item. f is the number that first returned.
func firstNote(it Item, f int, c *Config, now time.Time) string {
	labelled := slices.Contains(it.Labels, mergeFirst)
	what := "labelled " + mergeFirst
	if it.PR && it.FirstIssue != 0 {
		what = fmt.Sprintf("closes #%d, labelled %s", it.FirstIssue, mergeFirst)
	}
	no := what + ", which does not count: "
	until := it.FirstSince.Add(c.MergeFirstLimit.Duration)
	const day = "15:04 UTC on 2 January"
	parked := slices.Contains(it.Labels, "needs-human")
	switch {
	case !labelled && (!it.PR || it.FirstIssue == 0):
		return ""
	case !it.PR && it.FirstSince.IsZero():
		return what + ": the label does not count, or the issue needs no job and no open pull request closes it"
	case !it.PR && !leads(it, now):
		return what + ", which counted until " + it.FirstSince.Add(firstIssueSpan).UTC().Format(day) + ": a pull request that closes it merges first only if it was ready for review before then"
	case !it.PR:
		return what + ", which counts until " + it.FirstSince.Add(firstIssueSpan).UTC().Format(day) + ": its jobs come before the others, and a pull request that closes it and is ready for review before then merges first"
	case it.Number == f:
		return "merges first until " + until.UTC().Format("15:04 UTC")
	case it.Draft && leads(it, now) && !parked:
		return what + ": a draft holds nothing back, but its jobs come before the others"
	case it.Draft:
		return no + "a draft"
	case it.Outside:
		return no + "from outside the project"
	case parked:
		return no + "parked"
	case it.FirstSince.IsZero() && it.FirstIssue != 0:
		return no + "it was ready for review three days or more after the issue got the label"
	case it.FirstSince.IsZero():
		return no + "no maintainer in the config added it, or 20 events came since"
	case !now.Before(until):
		return what + ", which counted until " + until.UTC().Format(day)
	}
	return fmt.Sprintf("%s; #%d goes before it", what, f)
}

// event is the adding of a label to an issue or pull request, or a pull
// request's becoming ready for review or a draft; only the first has a
// label and an actor.
type event struct {
	Typename  string    `json:"__typename"`
	CreatedAt time.Time `json:"createdAt"`
	Label     struct {
		Name string `json:"name"`
	} `json:"label"`
	Actor *actor `json:"actor"`
}

// added returns the last adding of the label merge-first to an issue or
// pull request that has it, from its latest events, oldest first. It is
// nil if the item lacks the label or the adding is not among the events.
func added(labels []string, events []event) *event {
	if !slices.Contains(labels, mergeFirst) {
		return nil
	}
	var last *event
	for i, e := range events {
		if e.Label.Name == mergeFirst {
			last = &events[i]
		}
	}
	return last
}

// maintainer reports whether a is a person whom the config lists under
// maintainers. GitHub does not say how the one who added a label is
// associated with the repository, so its owner, organization members and
// collaborators count for the label merge-first only if the config lists
// them.
func (c *Config) maintainer(a *actor) bool {
	return a != nil && a.Typename == "User" &&
		slices.ContainsFunc(c.Maintainers, func(m string) bool { return strings.EqualFold(m, a.Login) })
}

// firstSince returns when the label merge-first was last added to a pull
// request that has it, from its latest events, oldest first. The result
// is zero unless a maintainer in the config added it. The agent App can
// label anything, so a misled agent could otherwise hold back every merge,
// and an App in the config's apps acts on what workflows run.
func firstSince(labels []string, events []event, c *Config) time.Time {
	if e := added(labels, events); e != nil && c.maintainer(e.Actor) {
		return e.CreatedAt
	}
	return time.Time{}
}

// atOpening is how long after an issue was opened a label still counts as
// one that it was opened with. GitHub records the labels that an issue is
// opened with a second or two after the issue.
const atOpening = time.Minute

// issueFirstSince returns when the label merge-first was last added to an
// issue that has it, from its latest events, oldest first. The result is
// zero unless the one who added it counts: a maintainer in the config, or
// an App in the config's apps that opened the issue with the label, as a
// workflow does with gh issue create --label. Who can make such an App
// open an issue can so let the pull requests that close it hold the others
// back, and have the issue's jobs picked before the others, within
// firstIssueSpan; the worker works on the App's issues anyway. The App's label on an issue that exists already does not count,
// nor does one that it adds again later, so that a workflow that only
// labels issues gives nobody this.
func issueFirstSince(labels []string, events []event, author *actor, opened time.Time, c *Config) time.Time {
	e := added(labels, events)
	switch {
	case e == nil:
	case c.maintainer(e.Actor):
		return e.CreatedAt
	case e.CreatedAt.Sub(opened) < atOpening &&
		slices.ContainsFunc(c.Apps, func(app string) bool { return e.Actor.is(app) && author.is(app) }):
		return e.CreatedAt
	}
	return time.Time{}
}

// ready returns when a pull request became ready for review: when it was
// opened, or, if it was opened as a draft, when it was first marked ready.
// A pull request that is made a draft and marked ready again keeps its
// time, so that this cannot begin its merging first anew. The events are
// its latest, oldest first. Once 20 events have come since the first
// marking, the result can be a later marking, or when it was opened; what
// a pull request gets from an issue stays within firstIssueSpan all the
// same.
func ready(opened time.Time, events []event) time.Time {
	for _, e := range events {
		switch e.Typename {
		case "ReadyForReviewEvent":
			return e.CreatedAt
		case "ConvertToDraftEvent":
			return opened
		}
	}
	return opened
}

// firstIssueSpan is how long after an issue got the label merge-first a
// pull request that closes it can begin to merge first. The agents decide
// which pull requests close an issue, and each begins its own
// merge_first_limit, so this bounds what one label on an issue can hold
// back: nothing later than firstIssueSpan plus merge_first_limit after it.
// It is long enough for a pull request that takes many jobs to get ready.
// The jobs that lead to such a pull request come before the others for as
// long: see leads.
const firstIssueSpan = 72 * time.Hour

// firstIssue is an open issue labelled merge-first that needs a job or
// that an open pull request closes.
type firstIssue struct {
	Number int
	Author *actor
	Opened time.Time
	Labels []string
	PRs    []int     // the open pull requests that close it
	Since  time.Time // since when its label counts, or zero: see issueFirstSince
}

// maxFirstIssues is how many issues firstIssues asks about, which keeps
// its query at one point.
const maxFirstIssues = 10

// firstIssuesQuery asks who labelled the given issues. It costs one point.
func firstIssuesQuery(issues []firstIssue) string {
	var b strings.Builder
	b.WriteString("query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n")
	for _, fi := range issues {
		fmt.Fprintf(&b, "    i%d: issue(number: %d) { events: timelineItems(last: 20, itemTypes: [LABELED_EVENT]) { nodes { __typename ... on LabeledEvent { createdAt label { name } actor { __typename login } } } } }\n", fi.Number, fi.Number)
	}
	b.WriteString("  }\n}")
	return b.String()
}

// firstIssues sets Since for the given issues, at most maxFirstIssues of
// them, the first. Who added a label is not in the survey of all issues,
// where it would cost a point more for each page, so survey asks here, and
// only about labelled issues that need a job or that an open pull request
// closes.
func firstIssues(ctx context.Context, g *github, c *Config, issues []firstIssue) error {
	owner, name, _ := strings.Cut(c.Repo, "/")
	if len(issues) > maxFirstIssues {
		issues = issues[:maxFirstIssues]
	}
	var r struct {
		Repository map[string]struct {
			Events struct {
				Nodes []event `json:"nodes"`
			} `json:"events"`
		} `json:"repository"`
	}
	if err := g.graphql(ctx, firstIssuesQuery(issues), vars(owner, name, ""), &r); err != nil {
		return err
	}
	for i := range issues {
		fi := &issues[i]
		fi.Since = issueFirstSince(fi.Labels, r.Repository[fmt.Sprintf("i%d", fi.Number)].Events.Nodes, fi.Author, fi.Opened, c)
	}
	return nil
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
        id number title createdAt authorAssociation updatedAt
        author { __typename login }
        labels(first: 30) { nodes { name } }
        blockedBy(first: 50) { nodes { state } }
        subIssues(first: 100) { nodes { state } }
        closedByPullRequestsReferences(first: 20) { nodes { number state mergedAt authorAssociation author { __typename login } repository { nameWithOwner } } }
      }
    }
  }
}`

const pullsQuery = `query($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, first: 25, after: $after, orderBy: {field: CREATED_AT, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id number title createdAt isDraft headRefOid mergeable reviewDecision isInMergeQueue authorAssociation updatedAt
        author { __typename login }
        labels(first: 30) { nodes { name } }
        autoMergeRequest { enabledAt }
        commits(last: 1) { nodes { commit { committedDate statusCheckRollup { state } } } }
        reviews(last: 100) { nodes { author { __typename login } authorAssociation state submittedAt commit { oid } } }
        timelineItems(last: 3, itemTypes: [REMOVED_FROM_MERGE_QUEUE_EVENT]) {
          nodes { ... on RemovedFromMergeQueueEvent { createdAt reason } }
        }
        events: timelineItems(last: 20, itemTypes: [LABELED_EVENT, READY_FOR_REVIEW_EVENT, CONVERT_TO_DRAFT_EVENT]) {
          nodes { __typename ... on LabeledEvent { createdAt label { name } actor { __typename login } } ... on ReadyForReviewEvent { createdAt } ... on ConvertToDraftEvent { createdAt } }
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
	Nodes []nameNode `json:"nodes"`
}

type nameNode struct {
	Name string `json:"name"`
}

type states struct {
	Nodes []stateNode `json:"nodes"`
}

type stateNode struct {
	State string `json:"state"`
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

// facts are the open issues and pull requests of the repository, as GitHub
// gives them.
type facts struct {
	// AsOf is when the reading of them began.
	AsOf   time.Time
	Issues []issueNode
	Pulls  []pullNode
}

// source reads the facts: from GitHub, or from a desk that follows it.
type source interface {
	facts(ctx context.Context) (*facts, error)
}

type issueNode struct {
	ID                string    `json:"id"`
	Number            int       `json:"number"`
	Title             string    `json:"title"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
	AuthorAssociation string    `json:"authorAssociation"`
	Author            *actor    `json:"author"`
	Labels            names     `json:"labels"`
	BlockedBy         states    `json:"blockedBy"`
	SubIssues         states    `json:"subIssues"`
	ClosedBy          struct {
		Nodes []closingPR `json:"nodes"`
	} `json:"closedByPullRequestsReferences"`
}

type pullNode struct {
	ID                string    `json:"id"`
	Number            int       `json:"number"`
	Title             string    `json:"title"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
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
		Nodes []headCommit `json:"nodes"`
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
}

// headCommit is the latest commit of a pull request.
type headCommit struct {
	Commit struct {
		CommittedDate     time.Time  `json:"committedDate"`
		StatusCheckRollup *stateNode `json:"statusCheckRollup"`
	} `json:"commit"`
}

// direct reads the facts from GitHub.
type direct struct {
	g    *github
	repo string
}

func (d direct) facts(ctx context.Context) (*facts, error) {
	owner, name, _ := strings.Cut(d.repo, "/")
	f := &facts{AsOf: time.Now()}
	for after := ""; ; {
		var r struct {
			Repository struct {
				Issues struct {
					PageInfo page        `json:"pageInfo"`
					Nodes    []issueNode `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		}
		if err := d.g.graphql(ctx, issuesQuery, vars(owner, name, after), &r); err != nil {
			return nil, err
		}
		f.Issues = append(f.Issues, r.Repository.Issues.Nodes...)
		p := r.Repository.Issues.PageInfo
		if !p.HasNextPage {
			break
		}
		after = p.EndCursor
	}
	for after := ""; ; {
		var r struct {
			Repository struct {
				PullRequests struct {
					PageInfo page       `json:"pageInfo"`
					Nodes    []pullNode `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		if err := d.g.graphql(ctx, pullsQuery, vars(owner, name, after), &r); err != nil {
			return nil, err
		}
		f.Pulls = append(f.Pulls, r.Repository.PullRequests.Nodes...)
		p := r.Repository.PullRequests.PageInfo
		if !p.HasNextPage {
			break
		}
		after = p.EndCursor
	}
	return f, nil
}

// survey returns every open issue and pull request, oldest first.
func survey(ctx context.Context, src source, g *github, c *Config) ([]Item, error) {
	f, err := src.facts(ctx)
	if err != nil {
		return nil, err
	}
	return f.items(ctx, g, c, time.Now())
}

// items returns the facts as items, oldest first. Who labelled an issue
// merge-first is not among the facts, so it asks GitHub: see firstIssues.
func (f *facts) items(ctx context.Context, g *github, c *Config, now time.Time) ([]Item, error) {
	var items []Item
	// The issues labelled merge-first that need a job or that an open pull
	// request closes.
	var asks []firstIssue
	for _, n := range f.Issues {
		it := Item{
			ID: n.ID, Number: n.Number, Title: n.Title, Labels: n.Labels.list(),
			Outside:      issueOutside(n.Author, n.AuthorAssociation, c),
			OpenBlockers: n.BlockedBy.open(), OpenSubIssues: n.SubIssues.open(),
			ClosingPRs: closing(n.ClosedBy.Nodes, c, now),
		}
		items = append(items, it)
		if slices.Contains(it.Labels, mergeFirst) {
			fi := firstIssue{Number: n.Number, Author: n.Author, Opened: n.CreatedAt, Labels: it.Labels}
			for _, pr := range n.ClosedBy.Nodes {
				if pr.State == "OPEN" && strings.EqualFold(pr.Repository.NameWithOwner, c.Repo) {
					fi.PRs = append(fi.PRs, pr.Number)
				}
			}
			// An issue's job does not depend on what merges first.
			if job, _ := next(it, c, 0); job != "" || len(fi.PRs) > 0 {
				asks = append(asks, fi)
			}
		}
	}
	// For each pull request, the issues that it closes whose label
	// merge-first counts.
	closes := map[int][]firstIssue{}
	// If there are more than firstIssues asks about, those that a pull
	// request closes go before the others, the oldest first in each: what
	// is held back must not depend on labels on issues without one.
	slices.SortStableFunc(asks, func(a, b firstIssue) int {
		switch ap, bp := len(a.PRs) > 0, len(b.PRs) > 0; {
		case ap == bp:
			return 0
		case ap:
			return -1
		}
		return 1
	})
	if len(asks) > 0 {
		if err := firstIssues(ctx, g, c, asks); err != nil {
			return nil, err
		}
		for _, fi := range asks {
			if fi.Since.IsZero() {
				continue
			}
			if i := slices.IndexFunc(items, func(it Item) bool { return it.Number == fi.Number }); i >= 0 {
				items[i].FirstSince = fi.Since
			}
			for _, pr := range fi.PRs {
				closes[pr] = append(closes[pr], fi)
			}
		}
	}
	for _, n := range f.Pulls {
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
		it.Removals = removals(n.TimelineItems.Nodes, it.ReviewedAt, headAt, now)
		// It merges first since a maintainer labelled it, or since it
		// was ready for review and an issue that it closes had the
		// label, if no more than firstIssueSpan lay between the two;
		// the latest of these counts.
		it.FirstSince = firstSince(it.Labels, n.Events.Nodes, c)
		at := ready(n.CreatedAt, n.Events.Nodes)
		for _, fi := range closes[it.Number] {
			since := fi.Since
			if at.After(since) {
				since = at
			}
			if it.FirstIssue == 0 {
				it.FirstIssue = fi.Number
			}
			if fi.Since.After(it.IssueSince) {
				it.IssueSince = fi.Since
			}
			if since.Sub(fi.Since) < firstIssueSpan && since.After(it.FirstSince) {
				it.FirstSince, it.FirstIssue = since, fi.Number
			}
		}
		items = append(items, it)
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
	Number            int       `json:"number"`
	State             string    `json:"state"`
	MergedAt          time.Time `json:"mergedAt"`
	AuthorAssociation string    `json:"authorAssociation"`
	Author            *actor    `json:"author"`
	Repository        struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
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
