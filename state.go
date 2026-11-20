package main

import (
	"context"
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
	Removals     int // times it left the merge queue without merging since the reviewer's latest verdict, counting at most 2
}

// Next jobs that the worker does by itself, without an agent.
const (
	autoMerge = "auto-merge"
	park      = "park" // label needs-human
)

// next returns the job an item needs, or "" if it needs none now, and why.
func next(it Item, c *Config) (job, why string) {
	has := func(l string) bool { return slices.Contains(it.Labels, l) }
	if has("needs-human") {
		return "", "labelled needs-human"
	}
	if it.Draft {
		return "", "draft"
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
		fix = "removed from the merge queue twice since its approval"
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
	// Removed from the merge queue once since the approval: put it back once
	// before a fix job, since the cause may be outside the code, such as a
	// failed download in CI. A merge of the base branch does not count as a
	// new commit here, as GitHub moves an approval onto it, so only a second
	// removal leads to a fix.
	case !it.AutoMerge && !it.Queued && it.Removals == 1:
		return autoMerge, "removed from the merge queue once since its approval; it goes back in once before a fix"
	case !it.AutoMerge && !it.Queued:
		return autoMerge, "approved"
	}
	return "", "approved, waiting for the merge queue"
}

// order is the order in which jobs are picked.
var order = []string{park, autoMerge, "fix", "review", "triage", "work"}

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
        id number title isDraft headRefOid mergeable reviewDecision isInMergeQueue authorAssociation
        author { __typename login }
        labels(first: 30) { nodes { name } }
        autoMergeRequest { enabledAt }
        commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
        reviews(last: 100) { nodes { author { __typename login } authorAssociation state submittedAt commit { oid } } }
        timelineItems(last: 2, itemTypes: [REMOVED_FROM_MERGE_QUEUE_EVENT]) {
          nodes { ... on RemovedFromMergeQueueEvent { createdAt } }
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
				Outside:      outside(n.Author, n.AuthorAssociation, c),
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
	for after := ""; ; {
		var r struct {
			Repository struct {
				PullRequests struct {
					PageInfo page `json:"pageInfo"`
					Nodes    []struct {
						ID                string    `json:"id"`
						Number            int       `json:"number"`
						Title             string    `json:"title"`
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
							Nodes []struct {
								CreatedAt time.Time `json:"createdAt"`
							} `json:"nodes"`
						} `json:"timelineItems"`
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
			if c := n.Commits.Nodes; len(c) > 0 && c[0].Commit.StatusCheckRollup != nil {
				it.Checks = c[0].Commit.StatusCheckRollup.State
			}
			for _, r := range verdicts(n.Reviews.Nodes, it.Head) {
				switch {
				case r.Author.is(c.Reviewer):
					it.Review, it.ReviewedAt = r.State, r.SubmittedAt
				case r.State == "CHANGES_REQUESTED" && !outside(&r.Author, r.AuthorAssociation, c):
					it.Requested = true
				}
			}
			var removed []time.Time
			for _, t := range n.TimelineItems.Nodes {
				removed = append(removed, t.CreatedAt)
			}
			it.Removals = removals(removed, it.ReviewedAt)
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

// removals counts the times in removed, when a pull request left the merge
// queue without merging, that come after since, the reviewer's verdict.
func removals(removed []time.Time, since time.Time) int {
	n := 0
	for _, t := range removed {
		if t.After(since) {
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
