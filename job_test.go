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

func mustParse(t *testing.T, file string) *template.Template {
	t.Helper()
	tmpl, err := parsePrompt(file)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

func TestRunJob(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "work.md")
	os.WriteFile(prompt, []byte("{{.Kind}} {{.Number}} in {{.Repo}}: {{.Title}} ({{.Why}}){{if .Leads}}, leads{{end}}{{if .First}}, merges first{{end}}"), 0o600)
	out := filepath.Join(dir, "out")
	c := &Config{Repo: "o/r", Jobs: map[string]Job{"work": {
		Prompt:  prompt,
		Command: []string{"sh", "-c", `cp "$1" ` + out, "sh", "{prompt}"},
		Timeout: duration{time.Minute},
	}}}
	if started, err := runJob(context.Background(), c, mustParse(t, prompt), candidate{Item: Item{Number: 7, Title: "Do it"}, job: "work", why: "ready"}); !started || err != nil {
		t.Fatal(started, err)
	}
	got, _ := os.ReadFile(out)
	if want := "issue 7 in o/r: Do it (ready)"; string(got) != want {
		t.Errorf("prompt %q, want %q", got, want)
	}

	// The prompt can tell an item whose jobs lead to a pull request that
	// merges first, and the pull request that merges first.
	for _, tc := range []struct {
		cand candidate
		want string
	}{
		{candidate{Item: Item{Number: 7, Title: "Do it"}, job: "work", why: "ready", leads: true}, "issue 7 in o/r: Do it (ready), leads"},
		{candidate{Item: Item{PR: true, Number: 8, Title: "Did it"}, job: "work", why: "approved", first: true}, "pull request 8 in o/r: Did it (approved), merges first"},
	} {
		if started, err := runJob(context.Background(), c, mustParse(t, prompt), tc.cand); !started || err != nil {
			t.Fatal(started, err)
		}
		if got, _ := os.ReadFile(out); string(got) != tc.want {
			t.Errorf("prompt %q, want %q", got, tc.want)
		}
	}
}

// TestRunJobDeadline checks that a prompt's Deadline is the job's start plus
// its limit, or plus its timeout if it has no limit.
func TestRunJobDeadline(t *testing.T) {
	// The deadline is in UTC wherever the worker runs.
	local := time.Local
	time.Local = time.FixedZone("test", -7*60*60)
	defer func() { time.Local = local }()
	dir := t.TempDir()
	prompt := filepath.Join(dir, "work.md")
	os.WriteFile(prompt, []byte("{{.Deadline}}"), 0o600)
	out := filepath.Join(dir, "out")
	for _, tc := range []struct{ limit, want time.Duration }{{0, 2 * time.Hour}, {30 * time.Minute, 30 * time.Minute}} {
		c := &Config{Repo: "o/r", Jobs: map[string]Job{"work": {
			Prompt:  prompt,
			Command: []string{"sh", "-c", `cp "$1" ` + out, "sh", "{prompt}"},
			Timeout: duration{2 * time.Hour},
			Limit:   duration{tc.limit},
		}}}
		start := time.Now()
		if started, err := runJob(context.Background(), c, mustParse(t, prompt), candidate{Item: Item{Number: 7}, job: "work", why: "ready"}); !started || err != nil {
			t.Fatal(started, err)
		}
		got, _ := os.ReadFile(out)
		deadline, err := time.Parse(deadlineLayout, string(got))
		if err != nil {
			t.Fatalf("limit %v: deadline %q: %v", tc.limit, got, err)
		}
		// The deadline is given to the minute.
		if d := deadline.Sub(start.Add(tc.want)); d < -time.Minute || d > time.Minute {
			t.Errorf("limit %v: deadline %q is %v from the start plus %v", tc.limit, got, d, tc.want)
		}
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
	if started, err := runJob(context.Background(), c, mustParse(t, prompt), candidate{Item: Item{PR: true, Number: 3}, job: "review", why: "latest commit not reviewed"}); !started || err != nil {
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
	if started, err := runJob(context.Background(), c, mustParse(t, prompt), candidate{Item: Item{PR: true, Number: 3}, job: "review", why: "latest commit not reviewed"}); started || err == nil {
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
	if started, err := runJob(context.Background(), c, mustParse(t, prompt), candidate{Item: Item{PR: true, Number: 1}, job: "fix", why: "checks failed"}); !started || err == nil {
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
	if started, err := runJob(context.Background(), c, mustParse(t, prompt), candidate{Item: Item{Number: 1}, job: "work", why: "ready"}); started || err == nil {
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
		tmpl, err := parsePrompt(filepath.Join("prompts", job+".md"))
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		p := prompt{Repo: "o/r", Owner: "o", Name: "r", Kind: "issue", Number: 7, Title: "Do it", Why: "ready", Deadline: "2026-01-01 00:00 UTC"}
		if err := tmpl.Execute(&b, p); err != nil {
			t.Errorf("%s: %v", job, err)
		}
		// What a prompt says of a pull request that merges first, or of
		// one that will, is for that pull request only.
		for _, tc := range []struct {
			job, text string
			set       func(*prompt)
		}{
			{"review", "for it only when you approve it", func(p *prompt) { p.Leads = true }},
			{"fix", "Every other approved pull request waits for this one to merge", func(p *prompt) { p.First = true }},
		} {
			if tc.job != job {
				continue
			}
			if strings.Contains(b.String(), tc.text) {
				t.Errorf("%s: the plain prompt says %q", job, tc.text)
			}
			with := p
			tc.set(&with)
			var w strings.Builder
			if err := tmpl.Execute(&w, with); err != nil {
				t.Errorf("%s: %v", job, err)
			}
			if !strings.Contains(w.String(), tc.text) {
				t.Errorf("%s: the prompt of such a pull request does not say %q", job, tc.text)
			}
		}
		if !strings.Contains(b.String(), "#7") {
			t.Errorf("%s: the prompt does not name the item", job)
		}
		if job != "triage" && !strings.Contains(b.String(), "It still works with the latest base branch") {
			t.Errorf("%s: the prompt does not include _checks.md", job)
		}
	}
}
