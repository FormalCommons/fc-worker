package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitRepo makes a repository with files, committed, and returns its path.
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := runGit(context.Background(), dir, nil, "init", "--quiet", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	commit(t, dir, files)
	return dir
}

// commit writes files into the repository at dir and commits them.
func commit(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	ctx := context.Background()
	for name, s := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "--quiet", "-m", "prompts"}} {
		if _, err := runGit(ctx, dir, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
}

func render(t *testing.T, p *promptRepo, file string) (string, string) {
	t.Helper()
	ctx := context.Background()
	commit, err := p.latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := p.parse(ctx, commit, file)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, prompt{Number: 7}); err != nil {
		t.Fatal(err)
	}
	return b.String(), commit
}

func TestPromptRepo(t *testing.T) {
	src := gitRepo(t, map[string]string{
		"work.md":          "work {{.Number}}: {{template \"_checks.md\" .}}",
		"_checks.md":       "v1",
		"jobs/review.md":   "review {{.Number}}: {{template \"_more.md\" .}}",
		"jobs/_more.md":    "more",
		"jobs/_notes.txt":  "not a partial",
		"other/_checks.md": "not beside work.md",
	})
	p := &promptRepo{url: src, ref: "main", refresh: time.Hour, dir: filepath.Join(t.TempDir(), "prompts.git")}
	if err := p.init(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, first := render(t, p, "work.md")
	if got != "work 7: v1" {
		t.Errorf("work.md rendered %q", got)
	}
	if got, _ := render(t, p, "jobs/review.md"); got != "review 7: more" {
		t.Errorf("jobs/review.md rendered %q", got)
	}

	// A new commit is not fetched until refresh has passed.
	commit(t, src, map[string]string{"_checks.md": "v2"})
	if got, c := render(t, p, "work.md"); got != "work 7: v1" || c != first {
		t.Errorf("before refresh: %q from %s, want the first commit", got, c)
	}
	p.refresh = 0
	got, second := render(t, p, "work.md")
	if got != "work 7: v2" || second == first {
		t.Errorf("after refresh: %q from %s, want the new commit", got, second)
	}

	// A failed fetch keeps the commit fetched before.
	p.url = filepath.Join(t.TempDir(), "missing")
	if got, c := render(t, p, "work.md"); got != "work 7: v2" || c != second {
		t.Errorf("after a failed fetch: %q from %s, want the last commit", got, c)
	}

	// With no commit fetched yet, a failed fetch is an error.
	q := &promptRepo{url: p.url, ref: "main", dir: filepath.Join(t.TempDir(), "prompts.git")}
	q.init(context.Background())
	if _, err := q.latest(context.Background()); err == nil {
		t.Error("no error for a repository that cannot be fetched")
	}

	// A prompt missing from the commit is an error.
	if _, err := p.parse(context.Background(), second, "missing.md"); err == nil {
		t.Error("no error for a missing prompt")
	}
}
