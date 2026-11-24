package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"
)

// desk reads the facts from a desk: a service that follows the repository
// on GitHub and answers from its own copy, which costs nothing of GitHub's
// rate limits.
type desk struct {
	url, token, repo string
	client           *http.Client
}

func newDesk(c *Config) *desk {
	return &desk{url: c.Desk.URL, token: c.Desk.Token, repo: c.Repo, client: &http.Client{Timeout: time.Minute}}
}

// The form of a desk's answer to GET /survey. Values such as OPEN and
// APPROVED are GitHub's own.
type (
	deskSurvey struct {
		Repo   string      `json:"repo"`
		AsOf   time.Time   `json:"as_of"`
		Issues []deskIssue `json:"issues"`
		Pulls  []deskPull  `json:"pulls"`
	}
	deskActor struct {
		Type  string `json:"type"`
		Login string `json:"login"`
	}
	deskRef struct {
		State string `json:"state"`
	}
	deskIssue struct {
		ID           string     `json:"id"`
		Number       int        `json:"number"`
		Title        string     `json:"title"`
		CreatedAt    time.Time  `json:"created_at"`
		UpdatedAt    time.Time  `json:"updated_at"`
		Author       *deskActor `json:"author"`
		Association  string     `json:"association"`
		Labels       []string   `json:"labels"`
		BlockedBy    []deskRef  `json:"blocked_by"`
		SubIssues    []deskRef  `json:"sub_issues"`
		ClosingPulls []struct {
			Repo        string     `json:"repo"`
			Number      int        `json:"number"`
			State       string     `json:"state"`
			MergedAt    time.Time  `json:"merged_at"`
			Author      *deskActor `json:"author"`
			Association string     `json:"association"`
		} `json:"closing_pulls"`
	}
	deskPull struct {
		ID              string     `json:"id"`
		Number          int        `json:"number"`
		Title           string     `json:"title"`
		CreatedAt       time.Time  `json:"created_at"`
		UpdatedAt       time.Time  `json:"updated_at"`
		Author          *deskActor `json:"author"`
		Association     string     `json:"association"`
		Labels          []string   `json:"labels"`
		Draft           bool       `json:"draft"`
		Head            string     `json:"head"`
		HeadCommittedAt time.Time  `json:"head_committed_at"`
		Mergeable       string     `json:"mergeable"`
		Checks          string     `json:"checks"`
		ReviewDecision  string     `json:"review_decision"`
		AutoMerge       bool       `json:"auto_merge"`
		Queued          bool       `json:"queued"`
		Reviews         []struct {
			Author      *deskActor `json:"author"`
			Association string     `json:"association"`
			State       string     `json:"state"`
			SubmittedAt time.Time  `json:"submitted_at"`
			Commit      string     `json:"commit"`
		} `json:"reviews"`
		QueueRemovals []struct {
			At     time.Time `json:"at"`
			Reason string    `json:"reason"`
		} `json:"queue_removals"`
		Events []struct {
			Type  string     `json:"type"`
			At    time.Time  `json:"at"`
			Label string     `json:"label"`
			Actor *deskActor `json:"actor"`
		} `json:"events"`
	}
)

func (a *deskActor) actor() *actor {
	if a == nil {
		return nil
	}
	return &actor{Typename: a.Type, Login: a.Login}
}

func nameNodes(list []string) names {
	var l names
	for _, name := range list {
		l.Nodes = append(l.Nodes, nameNode{name})
	}
	return l
}

func stateNodes(refs []deskRef) states {
	var l states
	for _, r := range refs {
		l.Nodes = append(l.Nodes, stateNode{r.State})
	}
	return l
}

// eventTypenames gives GitHub's name for each type of event in a desk's
// answer.
var eventTypenames = map[string]string{
	"labeled":          "LabeledEvent",
	"ready_for_review": "ReadyForReviewEvent",
	"convert_to_draft": "ConvertToDraftEvent",
}

func (d *desk) facts(ctx context.Context) (*facts, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url+"/survey?repo="+url.QueryEscape(d.repo), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("User-Agent", "fc-worker")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("desk: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("desk: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The desk says why in a line of text.
		why, _, _ := strings.Cut(strings.TrimSpace(string(data[:min(len(data), 200)])), "\n")
		if why == "" {
			return nil, fmt.Errorf("desk: %s", resp.Status)
		}
		return nil, fmt.Errorf("desk: %s: %s", resp.Status, why)
	}
	var v deskSurvey
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("desk: %w", err)
	}
	if !strings.EqualFold(v.Repo, d.repo) {
		return nil, fmt.Errorf("desk: the answer is for %q, not for %s", v.Repo, d.repo)
	}
	if v.AsOf.IsZero() {
		return nil, errors.New("desk: the answer does not say how old it is")
	}
	return v.facts(), nil
}

// facts returns a desk's answer in the form of GitHub's, and in GitHub's
// order, the oldest first: items depends on the order of the issues, and
// an issue's number can be out of the order of its opening.
func (v *deskSurvey) facts() *facts {
	f := &facts{AsOf: v.AsOf}
	slices.SortStableFunc(v.Issues, func(a, b deskIssue) int { return a.CreatedAt.Compare(b.CreatedAt) })
	slices.SortStableFunc(v.Pulls, func(a, b deskPull) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for _, it := range v.Issues {
		n := issueNode{
			ID: it.ID, Number: it.Number, Title: it.Title, CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
			AuthorAssociation: it.Association, Author: it.Author.actor(), Labels: nameNodes(it.Labels),
			BlockedBy: stateNodes(it.BlockedBy), SubIssues: stateNodes(it.SubIssues),
		}
		for _, p := range it.ClosingPulls {
			pr := closingPR{Number: p.Number, State: p.State, MergedAt: p.MergedAt, AuthorAssociation: p.Association, Author: p.Author.actor()}
			pr.Repository.NameWithOwner = p.Repo
			n.ClosedBy.Nodes = append(n.ClosedBy.Nodes, pr)
		}
		f.Issues = append(f.Issues, n)
	}
	for _, it := range v.Pulls {
		n := pullNode{
			ID: it.ID, Number: it.Number, Title: it.Title, CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
			IsDraft: it.Draft, HeadRefOid: it.Head, Mergeable: it.Mergeable, ReviewDecision: it.ReviewDecision,
			IsInMergeQueue: it.Queued, AuthorAssociation: it.Association, Author: it.Author.actor(), Labels: nameNodes(it.Labels),
		}
		if it.AutoMerge {
			n.AutoMergeRequest = &struct{}{}
		}
		var head headCommit
		head.Commit.CommittedDate = it.HeadCommittedAt
		if it.Checks != "" {
			head.Commit.StatusCheckRollup = &stateNode{it.Checks}
		}
		n.Commits.Nodes = []headCommit{head}
		for _, r := range it.Reviews {
			rv := review{AuthorAssociation: r.Association, State: r.State, SubmittedAt: r.SubmittedAt}
			if a := r.Author.actor(); a != nil {
				rv.Author = *a
			}
			rv.Commit.Oid = r.Commit
			n.Reviews.Nodes = append(n.Reviews.Nodes, rv)
		}
		for _, r := range it.QueueRemovals {
			n.TimelineItems.Nodes = append(n.TimelineItems.Nodes, removal{CreatedAt: r.At, Reason: r.Reason})
		}
		for _, e := range it.Events {
			// A type that this worker does not know is left out: with
			// a label's name, it would pass for the adding of the label.
			ev := event{Typename: eventTypenames[e.Type], CreatedAt: e.At, Actor: e.Actor.actor()}
			if ev.Typename == "" {
				continue
			}
			ev.Label.Name = e.Label
			n.Events.Nodes = append(n.Events.Nodes, ev)
		}
		f.Pulls = append(f.Pulls, n)
	}
	return f
}

// comparison is what -compare prints: the items for which GitHub's facts
// and the desk's give something different.
type comparison struct {
	// At is when GitHub was asked, and AsOf how old the desk's answer
	// said it was. The desk was asked first, so everything that changed
	// on GitHub between AsOf and the end of GitHub's answer can differ.
	At     time.Time    `json:"at"`
	AsOf   time.Time    `json:"as_of"`
	GitHub counts       `json:"github"`
	Desk   counts       `json:"desk"`
	Differ []difference `json:"differ"`
}

type counts struct {
	Issues int `json:"issues"`
	Pulls  int `json:"pulls"`
}

// difference is an item that differs. Each pair is GitHub's, then the
// desk's.
type difference struct {
	Item string `json:"item"` // as in a claim: issue-12 or pr-20
	// Only is "github" or "desk" for an item that only one of them has.
	Only string `json:"only,omitempty"`
	// Fields are the fields of Item that differ, with both values.
	Fields map[string][2]any `json:"fields,omitempty"`
	// Jobs are the jobs that the item needs next; "" is none.
	Jobs [2]string `json:"jobs"`
	// Updated is when GitHub says that the item last changed. If the
	// desk's is older, it had not yet read the change.
	Updated [2]*time.Time `json:"updated"`
}

// compare surveys the repository from GitHub and from the desk and returns
// what differs, with the items of both worked out as of now. It reads no
// claims and changes nothing.
func (w *worker) compare(ctx context.Context, now time.Time) (*comparison, error) {
	fd, err := newDesk(w.c).facts(ctx)
	if err != nil {
		return nil, err
	}
	fg, err := direct{w.gh, w.c.Repo}.facts(ctx)
	if err != nil {
		return nil, err
	}
	var sides [2]map[string]Item
	var jobs [2]map[string]string
	var updated [2]map[string]time.Time
	for i, f := range []*facts{fg, fd} {
		items, err := f.items(ctx, w.gh, w.c, now)
		if err != nil {
			return nil, err
		}
		sides[i], jobs[i], updated[i] = map[string]Item{}, map[string]string{}, map[string]time.Time{}
		held := first(items, w.c, now)
		for _, it := range items {
			key := candidate{Item: it}.key()
			sides[i][key] = it
			jobs[i][key], _ = next(it, w.c, held)
		}
		for _, n := range f.Issues {
			updated[i][fmt.Sprintf("issue-%d", n.Number)] = n.UpdatedAt
		}
		for _, n := range f.Pulls {
			updated[i][fmt.Sprintf("pr-%d", n.Number)] = n.UpdatedAt
		}
	}
	cmp := &comparison{
		At: fg.AsOf.UTC().Truncate(time.Second), AsOf: fd.AsOf, Differ: []difference{},
		GitHub: counts{len(fg.Issues), len(fg.Pulls)}, Desk: counts{len(fd.Issues), len(fd.Pulls)},
	}
	var keys []string
	for _, it := range mergeItems(sides[0], sides[1]) {
		keys = append(keys, candidate{Item: it}.key())
	}
	for _, key := range keys {
		g, onGitHub := sides[0][key]
		d, onDesk := sides[1][key]
		diff := difference{Item: key, Jobs: [2]string{jobs[0][key], jobs[1][key]}}
		for i := range updated {
			if at, ok := updated[i][key]; ok {
				diff.Updated[i] = &at
			}
		}
		switch {
		case !onDesk:
			diff.Only = "github"
		case !onGitHub:
			diff.Only = "desk"
		default:
			// Equal fields can still give two jobs: which item merges
			// first depends on the others.
			if diff.Fields = differing(g, d); len(diff.Fields) == 0 && diff.Jobs[0] == diff.Jobs[1] {
				continue
			}
		}
		cmp.Differ = append(cmp.Differ, diff)
	}
	return cmp, nil
}

// mergeItems returns the items of both, each once, pull requests after
// issues and each by number.
func mergeItems(a, b map[string]Item) []Item {
	seen := map[string]Item{}
	for _, m := range []map[string]Item{a, b} {
		for key, it := range m {
			if _, ok := seen[key]; !ok {
				seen[key] = it
			}
		}
	}
	items := make([]Item, 0, len(seen))
	for _, it := range seen {
		items = append(items, it)
	}
	slices.SortFunc(items, func(x, y Item) int {
		if x.PR != y.PR {
			if y.PR {
				return -1
			}
			return 1
		}
		return x.Number - y.Number
	})
	return items
}

// differing returns the fields in which two items differ, with both
// values.
func differing(a, b Item) map[string][2]any {
	fields := map[string][2]any{}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := range va.NumField() {
		if !va.Type().Field(i).IsExported() {
			continue
		}
		x, y := va.Field(i).Interface(), vb.Field(i).Interface()
		if tx, ok := x.(time.Time); ok {
			if tx.Equal(y.(time.Time)) {
				continue
			}
		} else if reflect.DeepEqual(x, y) {
			continue
		}
		fields[va.Type().Field(i).Name] = [2]any{x, y}
	}
	return fields
}
