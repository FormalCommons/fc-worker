package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

type worker struct {
	c      *Config
	gh     *github
	claims *claims
}

// candidate is an item that needs a job now.
type candidate struct {
	Item
	job, why string
}

func (c candidate) key() string {
	if c.PR {
		return fmt.Sprintf("pr-%d", c.Number)
	}
	return fmt.Sprintf("issue-%d", c.Number)
}

// candidates returns the items that need a job, in the order in which to
// pick them: by job, then oldest first.
func candidates(items []Item, c *Config) []candidate {
	var out []candidate
	for _, it := range items {
		if job, why := next(it, c); job != "" {
			out = append(out, candidate{it, job, why})
		}
	}
	slices.SortStableFunc(out, func(a, b candidate) int {
		return slices.Index(order, a.job) - slices.Index(order, b.job)
	})
	return out
}

// step does the first job that no other worker has claimed. It reports
// whether there was one, and the job's error, if any.
func (w *worker) step(ctx context.Context) (bool, error) {
	// Claims are fetched before the survey. A worker releases its claim
	// only after its job is done, so any claim released since then shows
	// its job's work in the survey.
	held, err := w.claims.fetch(ctx)
	if err != nil {
		return false, err
	}
	items, err := survey(ctx, w.gh, w.c)
	if err != nil {
		return false, err
	}
	if r, l, ok := w.gh.low(w.c.RateReserve); ok {
		log.Printf("GitHub's %s rate limit has %d left until %s, less than rate_reserve; starting no job",
			r, l.Remaining, l.Reset.UTC().Format(time.TimeOnly))
		return false, nil
	}
	for _, cand := range candidates(items, w.c) {
		key, old := cand.key(), ""
		if h, ok := held[key]; ok {
			if time.Now().Before(h.Lease.Expires) {
				continue
			}
			old = h.Commit
		}
		ttl := w.c.Jobs[cand.job].Timeout.Duration + 10*time.Minute
		commit, err := w.claims.take(ctx, key, cand.job, ttl, old)
		if err != nil {
			log.Printf("%s: not claimed: %v", key, err)
			continue
		}
		err = w.do(ctx, cand)
		// Release even when shutting down, so that others need not wait
		// for the claim to expire.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		if err := w.claims.release(rctx, key, cand.job, commit); err != nil {
			log.Printf("%s: release: %v", key, err)
		}
		cancel()
		return true, err
	}
	return false, nil
}

// do does one job and logs it.
func (w *worker) do(ctx context.Context, cand candidate) error {
	log.Printf("%s: %s (%s)", cand.key(), cand.job, cand.why)
	var err error
	switch cand.job {
	case park:
		err = w.park(ctx, cand.Item, cand.why)
	case autoMerge:
		err = w.gh.graphql(ctx, `mutation($id: ID!) { enablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId } }`,
			map[string]any{"id": cand.ID}, &struct{}{})
		if err != nil {
			// Auto-merge comes first in order, so a failure that lasts
			// would hold up every other job.
			err = w.park(ctx, cand.Item, "turning on auto-merge failed: "+err.Error())
		}
	default:
		start := time.Now()
		var started bool
		started, err = runJob(ctx, w.c, cand.job, cand.Item, cand.why)
		log.Printf("%s: %s ended after %s", cand.key(), cand.job, time.Since(start).Round(time.Second))
		// A job whose command started is a round, whether or not it
		// succeeded.
		if started && ctx.Err() == nil {
			err = errors.Join(err, w.record(ctx, cand, err))
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %s: %w", cand.key(), cand.job, err)
	}
	return nil
}

// record looks at the item again after a job and comments on it: which job
// ran, and whether it changed anything or its command failed. These
// comments count the rounds, and the worker parks the item after
// idle_rounds rounds in a row that changed nothing, or max_rounds rounds in
// all. A failed command does not count toward idle_rounds, so that an
// outage, such as of the gateway, does not park item after item.
func (w *worker) record(ctx context.Context, cand candidate, jobErr error) error {
	items, err := survey(ctx, w.gh, w.c)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(items, func(it Item) bool { return it.Number == cand.Number })
	if i < 0 {
		return nil // closed or merged
	}
	rounds, idle, err := w.rounds(ctx, cand.Number)
	if err != nil {
		return err
	}
	rounds++
	marker, result := roundMarker, "It changed something."
	switch {
	case jobErr != nil:
		idle, result = 0, fmt.Sprintf("Its command failed: %v.", jobErr)
	case progress(items[i]) == progress(cand.Item):
		idle++
		marker, result = idleMarker, fmt.Sprintf("It changed nothing (%d of %d times allowed).", idle, w.c.IdleRounds)
	default:
		idle = 0
	}
	msg := fmt.Sprintf("%s\nfc-worker %s ran a %s round (%s). %s Round %d of at most %d.",
		marker, w.c.Name, cand.job, cand.why, result, rounds, w.c.MaxRounds)
	if err := w.comment(ctx, cand.Number, msg); err != nil {
		return err
	}
	switch {
	case idle >= w.c.IdleRounds:
		return w.park(ctx, items[i], fmt.Sprintf("%d rounds changed nothing", idle))
	case rounds >= w.c.MaxRounds:
		return w.park(ctx, items[i], fmt.Sprintf("it had %d rounds, which reaches max_rounds", rounds))
	}
	return nil
}

// progress sums up what a job can change about an item. A round whose item
// has the same progress before and after changed nothing. Comments do not
// count, and neither do checks and conflicts, which change by themselves.
func progress(it Item) string {
	labels := slices.Sorted(slices.Values(it.Labels))
	return fmt.Sprint(labels, it.OpenBlockers, it.OpenSubIssues, it.OpenPRs, it.Draft, it.Head, it.Review, it.ReviewedAt)
}

// Markers that begin the worker's comments. Only a marker at the start
// counts, so that a comment quoting one does not.
const (
	roundMarker = "<!-- fc-worker: round -->"
	idleMarker  = "<!-- fc-worker: round, no change -->"
	parkMarker  = "<!-- fc-worker: parked -->"
)

// comment is a comment on an issue or pull request.
type comment struct {
	Body   string `json:"body"`
	Author *actor `json:"author"`
}

// rounds reads an item's last 100 comments and counts its rounds.
func (w *worker) rounds(ctx context.Context, number int) (rounds, idle int, err error) {
	owner, name, _ := strings.Cut(w.c.Repo, "/")
	var r struct {
		Repository struct {
			Item struct {
				Comments struct {
					Nodes []comment `json:"nodes"`
				} `json:"comments"`
			} `json:"issueOrPullRequest"`
		} `json:"repository"`
	}
	err = w.gh.graphql(ctx, `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issueOrPullRequest(number: $number) {
      ... on Issue { comments(last: 100) { nodes { body author { __typename login } } } }
      ... on PullRequest { comments(last: 100) { nodes { body author { __typename login } } } }
    }
  }
}`, map[string]any{"owner": owner, "name": name, "number": number}, &r)
	rounds, idle = countRounds(r.Repository.Item.Comments.Nodes, w.c.Agent)
	return rounds, idle, err
}

// countRounds counts, from an item's comments, oldest first, the rounds it
// has had since it was last parked, and how many of the latest of them in
// a row changed nothing. Only the worker's comments count, since anyone can
// write a marker into a comment; the worker comments as the agent App.
func countRounds(comments []comment, agent string) (rounds, idle int) {
	for _, c := range comments {
		if !c.Author.is(agent) {
			continue
		}
		switch {
		case strings.HasPrefix(c.Body, parkMarker):
			rounds, idle = 0, 0
		case strings.HasPrefix(c.Body, idleMarker):
			rounds, idle = rounds+1, idle+1
		case strings.HasPrefix(c.Body, roundMarker):
			rounds, idle = rounds+1, 0
		}
	}
	return rounds, idle
}

// park labels an item needs-human and says why.
func (w *worker) park(ctx context.Context, it Item, why string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/labels", w.c.Repo, it.Number)
	if err := w.gh.rest(ctx, "POST", path, map[string]any{"labels": []string{"needs-human"}}, nil); err != nil {
		return err
	}
	return w.comment(ctx, it.Number, fmt.Sprintf("%s\nfc-worker %s stopped working on this: %s. "+
		"Remove the needs-human label to hand it back.", parkMarker, w.c.Name, why))
}

func (w *worker) comment(ctx context.Context, number int, body string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", w.c.Repo, number)
	return w.gh.rest(ctx, "POST", path, map[string]any{"body": body}, nil)
}

// list prints every open issue and pull request with its next job, and who
// holds its claim.
func (w *worker) list(ctx context.Context, out io.Writer) error {
	held, err := w.claims.fetch(ctx)
	if err != nil {
		return err
	}
	items, err := survey(ctx, w.gh, w.c)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ITEM\tNEXT\tWHY\tCLAIMED BY\tTITLE")
	for _, it := range items {
		cand := candidate{Item: it}
		cand.job, cand.why = next(it, w.c)
		if cand.job == "" {
			cand.job = "-"
		}
		claimed := ""
		if h, ok := held[cand.key()]; ok && time.Now().Before(h.Lease.Expires) {
			claimed = h.Lease.Worker + " (" + h.Lease.Job + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", cand.key(), cand.job, cand.why, claimed, it.Title)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, r := range slices.Sorted(maps.Keys(w.gh.limits)) {
		l := w.gh.limits[r]
		fmt.Fprintf(out, "GitHub's %s rate limit: %d left until %s\n", r, l.Remaining, l.Reset.UTC().Format(time.TimeOnly))
	}
	return nil
}
