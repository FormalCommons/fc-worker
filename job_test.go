package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"
)

func TestRunJob(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "work.md")
	os.WriteFile(prompt, []byte("{{.Kind}} {{.Number}} in {{.Repo}}: {{.Title}} ({{.Why}})"), 0o600)
	out := filepath.Join(dir, "out")
	c := &Config{Repo: "o/r", Jobs: map[string]Job{"work": {
		Prompt:  prompt,
		Command: []string{"sh", "-c", `cp "$1" ` + out, "sh", "{prompt}"},
		Timeout: duration{time.Minute},
	}}}
	if started, err := runJob(context.Background(), c, "work", Item{Number: 7, Title: "Do it"}, "ready"); !started || err != nil {
		t.Fatal(started, err)
	}
	got, _ := os.ReadFile(out)
	if want := "issue 7 in o/r: Do it (ready)"; string(got) != want {
		t.Errorf("prompt %q, want %q", got, want)
	}
}

func TestRunJobLogs(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "p.md")
	os.WriteFile(prompt, []byte("p"), 0o600)
	logs := filepath.Join(dir, "logs") // the worker creates it
	c := &Config{Name: "w1", Logs: logs, Jobs: map[string]Job{"review": {
		Prompt:  prompt,
		Command: []string{"sh", "-c", "echo transcript; echo status >&2", "{prompt}"},
		Timeout: duration{time.Minute},
	}}}
	if started, err := runJob(context.Background(), c, "review", Item{PR: true, Number: 3}, "latest commit not reviewed"); !started || err != nil {
		t.Fatal(started, err)
	}
	files, _ := filepath.Glob(filepath.Join(logs, "*-w1-pr-3-review.log"))
	if len(files) != 1 {
		t.Fatalf("log files %v, want one", files)
	}
	if got, _ := os.ReadFile(files[0]); string(got) != "transcript\n" {
		t.Errorf("log %q, want only the standard output", got)
	}

	// A log that cannot be created stops the job before it starts.
	ran := filepath.Join(dir, "ran")
	c.Logs = filepath.Join(prompt, "logs") // under a file
	c.Jobs["review"] = Job{Prompt: prompt, Command: []string{"touch", ran, "{prompt}"}, Timeout: duration{time.Minute}}
	if started, err := runJob(context.Background(), c, "review", Item{PR: true, Number: 3}, "latest commit not reviewed"); started || err == nil {
		t.Errorf("started %v, error %v; want a job that did not start", started, err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the command ran")
	}
}

func TestRunJobTimeout(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "p.md")
	os.WriteFile(prompt, []byte("p"), 0o600)
	stopped := filepath.Join(dir, "stopped")
	c := &Config{Jobs: map[string]Job{"fix": {
		Prompt: prompt,
		// The command notes SIGTERM, as podman run passes it on to the
		// container.
		Command: []string{"sh", "-c", `trap "touch ` + stopped + `; exit 143" TERM; sleep 30 >/dev/null 2>&1 & wait`, "{prompt}"},
		Timeout: duration{500 * time.Millisecond},
	}}}
	start := time.Now()
	if started, err := runJob(context.Background(), c, "fix", Item{PR: true, Number: 1}, "checks failed"); !started || err == nil {
		t.Errorf("started %v, error %v; want a job that started and timed out", started, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("job took %s", d)
	}
	if _, err := os.Stat(stopped); err != nil {
		t.Error("the command did not get SIGTERM")
	}
}

func TestRunJobNotStarted(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "p.md")
	os.WriteFile(prompt, []byte("p"), 0o600)
	c := &Config{Jobs: map[string]Job{"work": {
		Prompt:  prompt,
		Command: []string{filepath.Join(dir, "missing"), "{prompt}"},
		Timeout: duration{time.Minute},
	}}}
	if started, err := runJob(context.Background(), c, "work", Item{Number: 1}, "ready"); started || err == nil {
		t.Errorf("started %v, error %v; want a job that did not start", started, err)
	}
}

func TestProgress(t *testing.T) {
	it := Item{PR: true, Labels: []string{"b", "a"}, Head: "h", Checks: "PENDING", Conflict: true}
	same := it
	same.Labels, same.Checks, same.Conflict = []string{"a", "b"}, "SUCCESS", false
	if progress(it) != progress(same) {
		t.Error("label order, checks or a conflict counted as progress")
	}
	moved := it
	moved.Head = "h2"
	if progress(it) == progress(moved) {
		t.Error("a new commit did not count as progress")
	}
}

// TestPrompts renders the prompts in prompts/, to catch mistakes in them.
func TestPrompts(t *testing.T) {
	for _, job := range jobNames {
		tmpl, err := template.ParseFiles(filepath.Join("prompts", job+".md"))
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		if err := tmpl.Execute(&b, prompt{"o/r", "o", "r", "issue", 7, "Do it", "ready"}); err != nil {
			t.Errorf("%s: %v", job, err)
		}
		if !strings.Contains(b.String(), "#7") {
			t.Errorf("%s: the prompt does not name the item", job)
		}
	}
}
