package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// claimRefs is where claims live in the repository. A claim is a ref, such
// as refs/fc-claims/issue-12, that points to an empty commit whose message
// is a lease: who holds the claim, and until when.
const claimRefs = "refs/fc-claims/"

type lease struct {
	Worker  string    `json:"worker"`
	Job     string    `json:"job"`
	Expires time.Time `json:"expires"`
}

// claim is a claim as fetched: its commit and its lease.
type claim struct {
	Commit string
	Lease  lease
}

// claims takes and releases claims with git. Pushes use
// --force-with-lease, so a push succeeds only if the claim has not changed
// since it was fetched, and two workers can never both take one.
type claims struct {
	url   string // the repository
	dir   string // a local bare repository that holds the claim commits
	name  string // this worker
	token func(context.Context) (string, error)
}

// fetch returns the current claims by key, such as "issue-12".
func (c *claims) fetch(ctx context.Context) (map[string]claim, error) {
	if _, err := c.git(ctx, "fetch", "--quiet", "--prune", "--no-tags", c.url, "+"+claimRefs+"*:"+claimRefs+"*"); err != nil {
		return nil, err
	}
	out, err := c.git(ctx, "for-each-ref", "--format=%(refname) %(objectname) %(contents:subject)", claimRefs)
	if err != nil {
		return nil, err
	}
	all := map[string]claim{}
	for line := range strings.Lines(out) {
		ref, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		commit, msg, _ := strings.Cut(rest, " ")
		var l lease
		json.Unmarshal([]byte(msg), &l) // a lease that cannot be read has expired
		all[strings.TrimPrefix(ref, claimRefs)] = claim{commit, l}
	}
	return all, nil
}

// take claims key until ttl from now. old is the claim's commit as fetched,
// or "" if there was no claim. It returns the new claim's commit, whose
// parent is old, so that a claim's commits never repeat, and its history
// is the item's claims.
func (c *claims) take(ctx context.Context, key, job string, ttl time.Duration, old string) (string, error) {
	msg, _ := json.Marshal(lease{c.name, job, time.Now().Add(ttl).UTC().Truncate(time.Second)})
	tree, err := c.git(ctx, "hash-object", "-w", "-t", "tree", "/dev/null")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", strings.TrimSpace(tree), "-m", string(msg)}
	if old != "" {
		args = append(args, "-p", old)
	}
	commit, err := c.git(ctx, args...)
	if err != nil {
		return "", err
	}
	commit = strings.TrimSpace(commit)
	ref := claimRefs + key
	_, err = c.git(ctx, "push", "--quiet", "--force-with-lease="+ref+":"+old, c.url, commit+":"+ref)
	return commit, err
}

// release ends a claim, if it is still the one this worker took, by
// replacing it with a lease that has expired. Deleting the ref instead would
// let a worker that saw no claim before this one was taken take it now.
func (c *claims) release(ctx context.Context, key, job, commit string) error {
	_, err := c.take(ctx, key, job, 0, commit)
	return err
}

// git runs git in the local bare repository, creating it first if needed.
// Fetches and pushes authenticate with a header, which keeps the token out
// of the command line.
func (c *claims) git(ctx context.Context, args ...string) (string, error) {
	if _, err := os.Stat(c.dir); os.IsNotExist(err) {
		if out, err := exec.CommandContext(ctx, "git", "init", "--quiet", "--bare", c.dir).CombinedOutput(); err != nil {
			return "", fmt.Errorf("git init: %v: %s", err, out)
		}
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = c.dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=fc-worker", "GIT_AUTHOR_EMAIL=fc-worker@invalid",
		"GIT_COMMITTER_NAME=fc-worker", "GIT_COMMITTER_EMAIL=fc-worker@invalid",
	)
	if c.token != nil && (args[0] == "fetch" || args[0] == "push") {
		token, err := c.token(ctx)
		if err != nil {
			return "", err
		}
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+basic)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
