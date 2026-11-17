package main

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"text/template"
	"time"
)

// prompt is what a prompt template can use.
type prompt struct {
	Repo   string // "owner/name"
	Owner  string
	Name   string
	Kind   string // "issue" or "pull request"
	Number int
	Title  string
	Why    string // why the item needs this job, such as "checks failed"
}

// runJob renders the job's prompt into a file and runs the job's command
// with it. The prompt is read again for every job, so it can be changed
// while the worker runs. When the timeout passes or ctx ends, the command
// gets SIGTERM, and SIGKILL 30 seconds later. It reports whether the
// command started, and how it ended.
func runJob(ctx context.Context, c *Config, job string, it Item, why string) (started bool, err error) {
	j := c.Jobs[job]
	t, err := template.ParseFiles(j.Prompt)
	if err != nil {
		return false, err
	}
	f, err := os.CreateTemp("", "fc-prompt-*.md")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	kind := "issue"
	if it.PR {
		kind = "pull request"
	}
	owner, name, _ := strings.Cut(c.Repo, "/")
	err = t.Execute(f, prompt{c.Repo, owner, name, kind, it.Number, it.Title, why})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}

	args := slices.Clone(j.Command)
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "{prompt}", f.Name())
	}
	ctx, cancel := context.WithTimeout(ctx, j.Timeout.Duration)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	if err := cmd.Start(); err != nil {
		return false, err
	}
	return true, cmd.Wait()
}
