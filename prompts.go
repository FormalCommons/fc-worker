package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
	"time"
)

// templates parses the prompt text, named name, together with parts, the
// files beside it named _*.md by name, which the prompt can include with
// {{template "_name.md" .}}. The parts are parsed in the order of their
// names, so that the same files always give the same templates.
func templates(name, text string, parts map[string]string) (*template.Template, error) {
	t, err := template.New(name).Parse(text)
	if err != nil {
		return nil, err
	}
	for _, n := range slices.Sorted(maps.Keys(parts)) {
		if _, err := t.New(n).Parse(parts[n]); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// parsePrompt parses the prompt file, together with the files beside it
// named _*.md.
func parsePrompt(file string) (*template.Template, error) {
	text, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	names, err := filepath.Glob(filepath.Join(filepath.Dir(file), "_*.md"))
	if err != nil {
		return nil, err
	}
	parts := map[string]string{}
	for _, n := range names {
		s, err := os.ReadFile(n)
		if err != nil {
			return nil, err
		}
		parts[filepath.Base(n)] = string(s)
	}
	return templates(filepath.Base(file), string(text), parts)
}

// Refs in a worker's prompts repository: the commit last fetched, and the
// commit in use, which every job's prompt renders from. The commit in use
// outlasts a restart.
const (
	fetchedRef = "refs/fc-prompts/fetched"
	currentRef = "refs/fc-prompts/current"
)

// promptRepo is a git repository that holds the prompts. The worker reads
// each job's prompt from the commit in use, so that the prompts can change
// while it runs, and a prompt and the files it includes always come from
// the same commit.
type promptRepo struct {
	url, ref string
	refresh  time.Duration
	dir      string   // a local bare repository
	files    []string // the jobs' prompts, which a new commit must render

	commit   string    // the commit in use
	fetched  time.Time // when the worker last fetched, or tried to
	err      error     // why the last fetch gave no commit to use
	rejected string    // the latest commit fetched whose prompts do not render
}

// init creates the local bare repository, if it does not exist yet, and
// takes up the commit that was in use before a restart, if any.
func (p *promptRepo) init(ctx context.Context) error {
	if _, err := runGit(ctx, "", nil, "init", "--quiet", "--bare", p.dir); err != nil {
		return err
	}
	// A fetch killed before it finished can leave this lock behind, which
	// would make every later fetch fail. Only this worker uses the
	// repository.
	os.Remove(filepath.Join(p.dir, "shallow.lock"))
	if out, err := runGit(ctx, p.dir, nil, "rev-parse", "--verify", "--quiet", currentRef+"^{commit}"); err == nil {
		p.commit = strings.TrimSpace(out)
	}
	return nil
}

// latest returns the commit to read prompts from. If the worker last
// fetched refresh ago or more, it fetches ref first, and takes up its commit
// if every job's prompt renders from it. Otherwise, or if the fetch fails,
// it keeps the commit it had, if any.
func (p *promptRepo) latest(ctx context.Context) (string, error) {
	if p.fetched.IsZero() || time.Since(p.fetched) >= p.refresh {
		p.err = p.update(ctx)
		p.fetched = time.Now()
		if p.err != nil && p.commit != "" {
			log.Printf("prompts: %v; still using %s", p.err, short(p.commit))
		}
	}
	if p.commit == "" {
		return "", fmt.Errorf("prompts: no commit to use: %w", p.err)
	}
	return p.commit, nil
}

// update fetches ref, and takes up its commit if every job's prompt renders.
func (p *promptRepo) update(ctx context.Context) error {
	commit, err := p.fetch(ctx)
	switch {
	case err != nil:
		return err
	case commit == p.commit:
		return nil
	case commit == p.rejected:
		return fmt.Errorf("the prompts at %s do not render", short(commit))
	}
	if err := p.check(ctx, commit); err != nil {
		p.rejected = commit
		return fmt.Errorf("the prompts at %s do not render: %w", short(commit), err)
	}
	if _, err := runGit(ctx, p.dir, nil, "update-ref", currentRef, commit); err != nil {
		log.Printf("prompts: %v", err)
	}
	log.Printf("prompts: now at %s", short(commit))
	p.commit = commit
	return nil
}

// fetch fetches ref, shallow, into fetchedRef, and returns its commit. Git
// then needs only what changed since the last fetch, and the commit stays
// in the repository until the next one.
func (p *promptRepo) fetch(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := runGit(ctx, p.dir, nil, "fetch", "--quiet", "--no-tags", "--depth=1", "--", p.url, "+"+p.ref+":"+fetchedRef); err != nil {
		return "", err
	}
	out, err := runGit(ctx, p.dir, nil, "rev-parse", "--verify", fetchedRef+"^{commit}")
	return strings.TrimSpace(out), err
}

// check renders every job's prompt from commit, with made-up values, so
// that a commit whose prompts do not parse or render is never used.
func (p *promptRepo) check(ctx context.Context, commit string) error {
	for _, f := range p.files {
		t, err := p.parse(ctx, commit, f)
		if err == nil {
			err = t.Execute(io.Discard, prompt{"o/r", "o", "r", "issue", 1, "A title", "ready"})
		}
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
	}
	return nil
}

// parse parses the prompt at file, a path in the repository, together with
// the files beside it named _*.md, as they are in commit.
func (p *promptRepo) parse(ctx context.Context, commit, file string) (*template.Template, error) {
	text, err := runGit(ctx, p.dir, nil, "cat-file", "blob", commit+":"+file)
	if err != nil {
		return nil, err
	}
	args := []string{"ls-tree", "-z", "--format=%(objecttype) %(path)", commit}
	if dir := path.Dir(file); dir != "." {
		args = append(args, "--", dir+"/")
	}
	// Names outside ASCII come out quoted unless core.quotePath is off,
	// even with -z, when the output has a format.
	noQuote := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.quotePath", "GIT_CONFIG_VALUE_0=false"}
	out, err := runGit(ctx, p.dir, noQuote, args...)
	if err != nil {
		return nil, err
	}
	parts := map[string]string{}
	for _, entry := range strings.Split(out, "\x00") {
		kind, name, _ := strings.Cut(entry, " ")
		if ok, _ := path.Match("_*.md", path.Base(name)); kind != "blob" || !ok {
			continue
		}
		s, err := runGit(ctx, p.dir, nil, "cat-file", "blob", commit+":"+name)
		if err != nil {
			return nil, err
		}
		parts[path.Base(name)] = s
	}
	return templates(path.Base(file), text, parts)
}

// short shortens a commit hash for logs and comments.
func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
