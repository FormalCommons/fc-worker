package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// templates parses the prompt text, named name, together with parts, the
// files beside it named _*.md by name, which the prompt can include with
// {{template "_name.md" .}}.
func templates(name, text string, parts map[string]string) (*template.Template, error) {
	t, err := template.New(name).Parse(text)
	if err != nil {
		return nil, err
	}
	for n, s := range parts {
		if _, err := t.New(n).Parse(s); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// parsePrompt parses the prompt file at path, together with the files
// beside it named _*.md.
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

// promptRepo is a git repository that holds the prompts. The worker reads
// each job's prompt from the latest commit it fetched, so that the prompts
// can change while it runs, and a prompt and the files it includes always
// come from the same commit.
type promptRepo struct {
	url, ref string
	refresh  time.Duration
	dir      string // a local bare repository

	commit  string // the latest commit fetched
	fetched time.Time
}

// init creates the local bare repository, if it does not exist yet.
func (p *promptRepo) init(ctx context.Context) error {
	_, err := runGit(ctx, "", nil, "init", "--quiet", "--bare", p.dir)
	return err
}

// latest returns the commit to read prompts from: the one fetched last, if
// that was less than refresh ago, and otherwise the latest on ref. If a
// fetch fails, it keeps the commit it fetched before, if any.
func (p *promptRepo) latest(ctx context.Context) (string, error) {
	if p.commit != "" && time.Since(p.fetched) < p.refresh {
		return p.commit, nil
	}
	commit, err := p.fetch(ctx)
	switch {
	case err != nil && p.commit == "":
		return "", fmt.Errorf("prompts: %w", err)
	case err != nil:
		log.Printf("prompts: %v; still using %s", err, short(p.commit))
		return p.commit, nil
	case commit != p.commit:
		log.Printf("prompts: now at %s", short(commit))
	}
	p.commit, p.fetched = commit, time.Now()
	return commit, nil
}

func (p *promptRepo) fetch(ctx context.Context) (string, error) {
	if _, err := runGit(ctx, p.dir, nil, "fetch", "--quiet", "--no-tags", "--depth=1", p.url, p.ref); err != nil {
		return "", err
	}
	out, err := runGit(ctx, p.dir, nil, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	return strings.TrimSpace(out), err
}

// parse parses the prompt at file, a path in the repository, together with
// the files beside it named _*.md, as they are in commit.
func (p *promptRepo) parse(ctx context.Context, commit, file string) (*template.Template, error) {
	text, err := runGit(ctx, p.dir, nil, "cat-file", "blob", commit+":"+file)
	if err != nil {
		return nil, err
	}
	args := []string{"ls-tree", "--name-only", commit}
	if dir := path.Dir(file); dir != "." {
		args = append(args, "--", dir+"/")
	}
	out, err := runGit(ctx, p.dir, nil, args...)
	if err != nil {
		return nil, err
	}
	parts := map[string]string{}
	for line := range strings.Lines(out) {
		n := strings.TrimSpace(line)
		if ok, _ := path.Match("_*.md", path.Base(n)); !ok {
			continue
		}
		s, err := runGit(ctx, p.dir, nil, "cat-file", "blob", commit+":"+n)
		if err != nil {
			return nil, err
		}
		parts[path.Base(n)] = s
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
