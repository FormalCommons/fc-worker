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
	OpenPRs       int // open pull requests that will close this issue

	// Pull requests only.
	Draft      bool
	Author     string
	Head       string    // the latest commit
	Conflict   bool      // the branch conflicts with the base
	Checks     string    // on the latest commit: SUCCESS, FAILURE, ERROR, PENDING, EXPECTED or ""
	Review     string    // the reviewer's latest review of the latest commit: APPROVED, CHANGES_REQUESTED or ""
	ReviewedAt time.Time // when the reviewer gave Review
	Requested  bool      // a person in the project requests changes to the latest commit
	Blocked    bool      // a person's request for changes stands
	AutoMerge  bool
	Queued     bool
	RemovedAt  time.Time // when it last left the merge queue without merging
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
		case it.OpenPRs > 0:
			return "", "an open pull request will close it"
		}
		return "work", "ready"
	}

	ours := sameLogin(it.Author, c.Agent)
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
	case it.Review == "APPROVED" && it.RemovedAt.After(it.ReviewedAt):
		fix = "removed from the merge queue"
	}
	switch {
	case fix != "" && !ours:
		return "", fix + "; waiting for its author"
	case fix != "":
		return "fix", fix
	case it.Checks != "SUCCESS":
		return "", "waiting for checks"
	case it.Review == "":
		return "review", "latest commit not reviewed"
	case !ours:
		return "", "approved; its author decides when to merge"
	case it.Blocked:
		return park, "approved by the reviewer, but a person's request for changes stands"
	case !it.AutoMerge && !it.Queued:
		return autoMerge, "approved"
	}
	return "", "approved, waiting for the merge queue"
}

// order is the order in which jobs are picked.
var order = []string{park, autoMerge, "fix", "review", "triage", "work"}

// outside reports whether an author is outside the project: neither of the
// two Apps, nor the repository's owner, an organization member or a
// collaborator. A deleted account is outside too.
func outside(author *actor, association string, c *Config) bool {
	if author == nil {
		return true
	}
	if sameLogin(author.Login, c.Agent) || sameLogin(author.Login, c.Reviewer) {
		return false
	}
	return association != "OWNER" && association != "MEMBER" && association != "COLLABORATOR"
}

func sameLogin(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "[bot]"), strings.TrimSuffix(b, "[bot]"))
}

const issuesQuery = `query($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    issues(states: OPEN, first: 50, after: $after, orderBy: {field: CREATED_AT, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id number title authorAssociation
        author { login }
        labels(first: 30) { nodes { name } }
        blockedBy(first: 50) { nodes { state } }
        subIssues(first: 100) { nodes { state } }
        closedByPullRequestsReferences(first: 20) { nodes { state authorAssociation author { login } } }
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
        author { login }
        labels(first: 30) { nodes { name } }
        autoMergeRequest { enabledAt }
        commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
        reviews(last: 100) { nodes { author { login } authorAssociation state submittedAt commit { oid } } }
        timelineItems(last: 1, itemTypes: [REMOVED_FROM_MERGE_QUEUE_EVENT]) {
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
							Nodes []struct {
								State             string `json:"state"`
								AuthorAssociation string `json:"authorAssociation"`
								Author            *actor `json:"author"`
							} `json:"nodes"`
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
			}
			// A pull request from outside the project does not hold up an
			// issue, since nothing moves it.
			for _, pr := range n.ClosedBy.Nodes {
				if pr.State == "OPEN" && !outside(pr.Author, pr.AuthorAssociation, c) {
					it.OpenPRs++
				}
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
				Conflict: n.Mergeable == "CONFLICTING", AutoMerge: n.AutoMergeRequest != nil, Queued: n.IsInMergeQueue,
			}
			if n.Author != nil {
				it.Author = n.Author.Login
			}
			if c := n.Commits.Nodes; len(c) > 0 && c[0].Commit.StatusCheckRollup != nil {
				it.Checks = c[0].Commit.StatusCheckRollup.State
			}
			if t := n.TimelineItems.Nodes; len(t) > 0 {
				it.RemovedAt = t[0].CreatedAt
			}
			for login, r := range verdicts(n.Reviews.Nodes, it.Head) {
				switch {
				case sameLogin(login, c.Reviewer):
					it.Review, it.ReviewedAt = r.State, r.SubmittedAt
				case r.State == "CHANGES_REQUESTED" && !outside(&r.Author, r.AuthorAssociation, c):
					it.Requested = true
				}
			}
			// reviewDecision sums up every review that counts, so it says
			// CHANGES_REQUESTED while anyone's request for changes stands.
			it.Blocked = n.ReviewDecision == "CHANGES_REQUESTED" && it.Review == "APPROVED"
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

// actor is the author of an issue, pull request or review.
type actor struct {
	Login string `json:"login"`
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

// verdicts returns each reviewer's latest verdict on the head commit, by
// login. Reviews come oldest first. Reviews that only comment are not
// verdicts.
func verdicts(reviews []review, head string) map[string]review {
	latest := map[string]review{}
	for _, r := range reviews {
		if r.Commit.Oid == head && (r.State == "APPROVED" || r.State == "CHANGES_REQUESTED") {
			latest[r.Author.Login] = r
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
