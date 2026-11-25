package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"
)

type worker struct {
	c       *Config
	gh      *github
	desk    *desk // nil if the config names none
	claims  *claims
	prompts *promptRepo // nil if the prompts are files
}

// survey returns every open issue and pull request, oldest first: from the
// desk, if the config names one and it answers, and otherwise from GitHub.
// It reports whether the desk gave them, since its copy can be a minute
// old.
func (w *worker) survey(ctx context.Context) (items []Item, fromDesk bool, err error) {
	src := &fallback{desk: w.desk, direct: direct{w.gh, w.c.Repo}}
	items, err = survey(ctx, src, w.gh, w.c)
	return items, src.fromDesk, err
}

// fresh surveys with cand's item as GitHub has it now: the desk reads the
// item again first. If the desk cannot, or the config names none, the
// survey is GitHub's own. Like survey, it reports whether the desk gave it.
func (w *worker) fresh(ctx context.Context, cand candidate) (items []Item, fromDesk bool, err error) {
	if w.desk != nil {
		if err = w.desk.refresh(ctx, cand.Number); err == nil {
			return w.survey(ctx)
		}
		if ctx.Err() != nil {
			// The worker is stopping, and a survey of GitHub would end
			// the same way.
			return nil, false, err
		}
		log.Printf("%s: refresh: %v; surveying GitHub instead", cand.key(), err)
	}
	items, err = survey(ctx, direct{w.gh, w.c.Repo}, w.gh, w.c)
	return items, false, err
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
// pick them: the pull request that merges first, if there is one, then by
// job, then oldest first.
func candidates(items []Item, c *Config) []candidate {
	f := first(items, c, time.Now())
	var out []candidate
	for _, it := range items {
		if job, why := next(it, c, f); job != "" {
			out = append(out, candidate{it, job, why})
		}
	}
	slices.SortStableFunc(out, func(a, b candidate) int {
		if af, bf := f != 0 && a.Number == f, f != 0 && b.Number == f; af != bf {
			if af {
				return -1
			}
			return 1
		}
		return slices.Index(order, a.job) - slices.Index(order, b.job)
	})
	return out
}

// step does the first job that no other worker has claimed. It reports
// whether there was one, and the job's error, if any.
func (w *worker) step(ctx context.Context) (bool, error) {
	// Leave the rest of the rate limits to the jobs that are running.
	graphql, rest, err := w.gh.rateLimits(ctx)
	if err != nil {
		return false, err
	}
	for _, l := range []struct {
		name string
		rateLimit
	}{{"GraphQL", graphql}, {"REST", rest}} {
		if l.Remaining < w.c.RateReserve {
			log.Printf("GitHub's %s rate limit has %d left until %s UTC, less than rate_reserve; waiting",
				l.name, l.Remaining, l.Reset.UTC().Format(time.TimeOnly))
			return false, nil
		}
	}
	// The prompts come before claiming, so that a worker that has none to
	// use claims nothing.
	var prompts string
	if w.prompts != nil {
		if prompts, err = w.prompts.latest(ctx); err != nil {
			return false, err
		}
	}
	// Claims are fetched before the survey. A worker releases its claim
	// only after its job is done, so any claim released since then shows
	// its job's work in a survey of GitHub. A desk's copy can be older
	// than the claims, so an item picked from it is looked at again once
	// it is claimed.
	held, err := w.claims.fetch(ctx)
	if err != nil {
		return false, err
	}
	items, fromDesk, err := w.survey(ctx)
	if err != nil {
		return false, err
	}
	cands := candidates(items, w.c)
	var back []string // the items that this step could not claim, or gave back
	for len(cands) > 0 {
		cand := cands[0]
		cands = cands[1:]
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
			back = append(back, key)
			continue
		}
		if fromDesk {
			// Another worker may have done this job and released its
			// claim since the desk last read the item. Nobody else works
			// on it now, so what it needs now is what to do.
			items, fromDesk, err = w.fresh(ctx, cand)
			if err != nil {
				w.release(ctx, cand, commit)
				return false, err
			}
			now := candidates(items, w.c)
			i := slices.IndexFunc(now, func(c candidate) bool { return c.key() == key && c.job == cand.job })
			if i < 0 {
				log.Printf("%s: no longer needs %s; claim given back", key, cand.job)
				w.release(ctx, cand, commit)
				back = append(back, key)
				if !fromDesk {
					// The desk could not show the item, so this survey
					// is GitHub's and newer than the claims. The step
					// goes on with it, and looks at none of its items
					// again. An item that was not claimed or was given
					// back waits for the next step: its claim is no
					// longer as this step read it.
					cands = slices.DeleteFunc(now, func(c candidate) bool { return slices.Contains(back, c.key()) })
				}
				continue
			}
			cand = now[i]
		}
		err = w.do(ctx, cand, prompts)
		if rateLimited(err) {
			w.logLimits(ctx, key, graphql, rest)
		}
		if w.desk != nil {
			// The next survey of any worker must show this job's work as
			// soon as the claim is gone. This too is done when shutting
			// down.
			if err := w.desk.refresh(context.WithoutCancel(ctx), cand.Number); err != nil {
				log.Printf("%s: refresh: %v", key, err)
			}
		}
		w.release(ctx, cand, commit)
		return true, err
	}
	return false, nil
}

// release ends the claim on cand's item. It does so even when shutting
// down, so that others need not wait for the claim to expire.
func (w *worker) release(ctx context.Context, cand candidate, commit string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := w.claims.release(ctx, cand.key(), cand.job, commit); err != nil {
		log.Printf("%s: release: %v", cand.key(), err)
	}
}

// do does one job and logs it. prompts is the commit of the prompts
// repository to read the job's prompt from, if the config names one.
func (w *worker) do(ctx context.Context, cand candidate, prompts string) error {
	log.Printf("%s: %s (%s)", cand.key(), cand.job, cand.why)
	var err error
	switch cand.job {
	case park:
		err = w.park(ctx, cand.Item, cand.why)
	case autoMerge:
		err = w.autoMerge(ctx, cand.Item)
	default:
		t, perr := w.prompt(ctx, cand.job, prompts)
		if perr != nil {
			err = perr
			break
		}
		if prompts != "" {
			log.Printf("%s: %s prompt from %s", cand.key(), cand.job, short(prompts))
		}
		start := time.Now()
		var started bool
		started, err = runJob(ctx, w.c, cand.job, t, cand.Item, cand.why)
		log.Printf("%s: %s ended after %s", cand.key(), cand.job, time.Since(start).Round(time.Second))
		// A job whose command started is a round, whether or not it
		// succeeded.
		if started && ctx.Err() == nil {
			err = errors.Join(err, w.record(ctx, cand, prompts, err))
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %s: %w", cand.key(), cand.job, err)
	}
	return nil
}

// prompt parses the job's prompt: from commit of the prompts repository,
// if the config names one, and otherwise from its file.
func (w *worker) prompt(ctx context.Context, job, commit string) (*template.Template, error) {
	file := w.c.Jobs[job].Prompt
	if w.prompts == nil {
		return parsePrompt(file)
	}
	return w.prompts.parse(ctx, commit, file)
}

// record looks at the item again after a job and comments on it: which job
// ran, with which prompts, and whether it changed anything or its command
// failed. These comments count the rounds, and the worker parks the item
// after idle_rounds rounds in a row that changed nothing, or max_rounds
// rounds in all. A failed command does not count toward idle_rounds, so that
// an outage, such as of the gateway, does not park item after item.
func (w *worker) record(ctx context.Context, cand candidate, version string, jobErr error) error {
	// A desk's copy from before the job would make a round that changed
	// something count as one that changed nothing.
	items, _, err := w.fresh(ctx, cand)
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
	with := ""
	if version != "" {
		with = " with prompts at " + short(version)
	}
	msg := fmt.Sprintf("%s\nfc-worker %s ran a %s round (%s)%s. %s Round %d of at most %d.",
		marker, w.c.Name, cand.job, cand.why, with, result, rounds, w.c.MaxRounds)
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
	return fmt.Sprint(labels, it.OpenBlockers, it.OpenSubIssues, it.ClosingPRs, it.Draft, it.Head, it.Review, it.ReviewedAt)
}

// settle is how long the worker gives GitHub to show a merge everywhere.
var settle = 10 * time.Second

// autoMerge turns on auto-merge for a pull request, and parks it if that
// fails. Auto-merge comes first in order, so a failure that lasts would hold
// up every other job. It parks only when GitHub refused for a reason of
// the pull request's, and the pull request, read again, is open and still
// needs auto-merge. A failure that says nothing of the pull request, such
// as a rate limit or a fault of GitHub's or of the network, is returned,
// and the next step tries again.
func (w *worker) autoMerge(ctx context.Context, it Item) error {
	err := w.gh.graphql(ctx, `mutation($id: ID!) { enablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId } }`,
		map[string]any{"id": it.ID}, &struct{}{})
	if err == nil {
		return nil
	}
	// For a few seconds after it leaves the merge queue as merged, a pull
	// request still looks open and not queued, and turning on auto-merge
	// fails. So look at it again once GitHub has settled, and park it only
	// if it still needs auto-merge.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(settle):
	}
	owner, name, _ := strings.Cut(w.c.Repo, "/")
	var r struct {
		Repository struct {
			PullRequest struct {
				State            string    `json:"state"`
				IsInMergeQueue   bool      `json:"isInMergeQueue"`
				AutoMergeRequest *struct{} `json:"autoMergeRequest"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	rerr := w.gh.graphql(ctx, `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { state isInMergeQueue autoMergeRequest { enabledAt } }
  }
}`, map[string]any{"owner": owner, "name": name, "number": it.Number}, &r)
	pr := r.Repository.PullRequest
	switch {
	case rerr != nil:
		return fmt.Errorf("turning it on failed: %w; reading the pull request again: %w", err, rerr)
	case pr.State == "MERGED" || pr.State == "CLOSED" || pr.IsInMergeQueue || pr.AutoMergeRequest != nil:
		log.Printf("pr-%d: turning on auto-merge failed, but it no longer needs it: %v", it.Number, err)
		return nil
	case passing(err):
		return fmt.Errorf("turning it on failed: %w", err)
	case pr.State != "OPEN":
		return fmt.Errorf("turning it on failed: %w; read again, its state is %q", err, pr.State)
	}
	return w.park(ctx, it, "turning on auto-merge failed: "+err.Error())
}

// passing reports whether err says nothing of what the request was about,
// so that the same request may succeed later. That is every error but a
// refusal by GitHub's GraphQL, and of those the ones for a rate limit and
// the one GitHub gives for a fault of its own.
func passing(err error) bool {
	var r *refusal
	return !errors.As(err, &r) || rateLimited(err) || strings.Contains(r.message, "Something went wrong")
}

// rateLimited reports whether err is GitHub's refusal of a request for a
// rate limit: the hourly one, or a secondary one, of which one says only
// that the request "was submitted too quickly". It goes by GitHub's
// message, which is all that the client keeps of an answer.
func rateLimited(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "rate limit") || strings.Contains(text, "submitted too quickly")
}

// logLimits logs what GitHub gave as left of its rate limits when a step
// began, and what it gives now, after it refused a request of the step's
// job on key for a rate limit. The step began only because GitHub gave
// rate_reserve or more.
func (w *worker) logLimits(ctx context.Context, key string, graphql, rest rateLimit) {
	form := func(graphql, rest rateLimit) string {
		return fmt.Sprintf("GraphQL %d until %s UTC, REST %d until %s UTC", graphql.Remaining, graphql.Reset.UTC().Format(time.TimeOnly),
			rest.Remaining, rest.Reset.UTC().Format(time.TimeOnly))
	}
	now := "cannot be asked now"
	if g, r, err := w.gh.rateLimits(context.WithoutCancel(ctx)); err != nil {
		now += ": " + err.Error()
	} else {
		now = "gives " + form(g, r) + " now"
	}
	log.Printf("%s: refused for GitHub's rate limit, which gave %s left when the step began and %s", key, form(graphql, rest), now)
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
	items, _, err := w.survey(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ITEM\tNEXT\tWHY\tCLAIMED BY\tTITLE")
	now := time.Now()
	f := first(items, w.c, now)
	for _, it := range items {
		cand := candidate{Item: it}
		cand.job, cand.why = next(it, w.c, f)
		if cand.job == "" {
			cand.job = "-"
		}
		if note := firstNote(it, f, w.c, now); note != "" {
			cand.why = strings.TrimPrefix(cand.why+"; "+note, "; ")
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
	graphql, rest, err := w.gh.rateLimits(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "GitHub rate limits: GraphQL %d left until %s UTC, REST %d left until %s UTC\n",
		graphql.Remaining, graphql.Reset.UTC().Format(time.TimeOnly), rest.Remaining, rest.Reset.UTC().Format(time.TimeOnly))
	return nil
}
