package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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
	// First is set for the pull request that merges first: the reviewer
	// has approved a commit of it, and every other approved pull request
	// stays out of the merge queue until it has merged. Leads is set for
	// an item whose jobs lead to such a pull request and so come before
	// the others: an issue whose label merge-first counts, the agent's
	// draft that closes one, or a pull request whose label counts and
	// that the reviewer has not approved yet. Both are false for most
	// jobs, and never both true.
	First bool
	Leads bool
	// Deadline is when the job is stopped, such as "2026-11-23 20:29 UTC":
	// its start plus the job's limit.
	Deadline string
}

// deadlineLayout is the form of a prompt's Deadline.
const deadlineLayout = "2006-01-02 15:04 UTC"

// runJob renders the prompt, t, of the job that cand needs into a file and
// runs the job's command with it. When the timeout passes or ctx ends, the command
// gets SIGTERM, and SIGKILL 30 seconds later. The command's standard output
// goes to a new file in the logs directory, if the config names one. It
// reports whether the command started, and how it ended.
func runJob(ctx context.Context, c *Config, t *template.Template, cand candidate) (started bool, err error) {
	job, it := cand.job, cand.Item
	j := c.Jobs[job]
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
	deadline := time.Now().Add(j.limit()).UTC().Format(deadlineLayout)
	err = t.Execute(f, prompt{
		Repo: c.Repo, Owner: owner, Name: name, Kind: kind, Number: it.Number, Title: it.Title,
		Why: cand.why, First: cand.first, Leads: cand.leads, Deadline: deadline,
	})
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
	if c.Logs != "" {
		key := cand.key()
		file := fmt.Sprintf("%s-%s-%s-%s.log", time.Now().UTC().Format("20060102T150405Z"), c.Name, key, job)
		if err := os.MkdirAll(c.Logs, 0o700); err != nil {
			return false, err
		}
		out, err := os.OpenFile(filepath.Join(c.Logs, file), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return false, err
		}
		defer out.Close()
		cmd.Stdout = out
		log.Printf("%s: %s output goes to %s", key, job, out.Name())
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	if err := cmd.Start(); err != nil {
		return false, err
	}
	return true, cmd.Wait()
}
