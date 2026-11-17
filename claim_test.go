package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// testClaims returns two workers' claims on one local bare repository,
// which stands in for GitHub.
func testClaims(t *testing.T) (a, b *claims) {
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	a = &claims{url: remote, dir: filepath.Join(dir, "a.git"), name: "a"}
	b = &claims{url: remote, dir: filepath.Join(dir, "b.git"), name: "b"}
	return a, b
}

func TestClaims(t *testing.T) {
	ctx := context.Background()
	a, b := testClaims(t)

	commit, err := a.take(ctx, "issue-1", "work", time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	// b looked before a took the claim, so it expects none.
	if _, err := b.take(ctx, "issue-1", "work", time.Hour, ""); err == nil {
		t.Error("b took a claim that a holds")
	}
	held, err := b.fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := held["issue-1"]
	if h.Commit != commit || h.Lease.Worker != "a" || h.Lease.Job != "work" || time.Until(h.Lease.Expires) < 59*time.Minute {
		t.Errorf("fetched %+v, want a's claim %s", h, commit)
	}
	if err := b.release(ctx, "issue-1", "work", "0000000000000000000000000000000000000000"); err == nil {
		t.Error("b released a claim it does not hold")
	}
	if err := a.release(ctx, "issue-1", "work", commit); err != nil {
		t.Fatal(err)
	}
	held, _ = b.fetch(ctx)
	if h := held["issue-1"]; h.Commit == commit || time.Now().Before(h.Lease.Expires) {
		t.Errorf("after release: %+v, want an expired lease", h)
	}
	// b saw no claim before a took it. Since a released it, the claim is
	// not as b saw it, so b cannot take it on that old view.
	if _, err := b.take(ctx, "issue-1", "work", time.Hour, ""); err == nil {
		t.Error("b took the claim on a view from before a took it")
	}
	if _, err := b.take(ctx, "issue-1", "work", time.Hour, held["issue-1"].Commit); err != nil {
		t.Errorf("b could not take the released claim: %v", err)
	}
}

func TestTakeExpired(t *testing.T) {
	ctx := context.Background()
	a, b := testClaims(t)
	old, err := a.take(ctx, "pr-2", "review", -time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	held, _ := b.fetch(ctx)
	if h := held["pr-2"]; h.Commit != old || time.Now().Before(h.Lease.Expires) {
		t.Fatalf("fetched %+v, want an expired claim", h)
	}
	commit, err := b.take(ctx, "pr-2", "review", time.Hour, old)
	if err != nil {
		t.Fatal(err)
	}
	// a's release of its expired claim leaves b's alone.
	if err := a.release(ctx, "pr-2", "review", old); err == nil {
		t.Error("a released b's claim")
	}
	if held, _ := a.fetch(ctx); held["pr-2"].Commit != commit {
		t.Errorf("claim is %+v, want b's %s", held["pr-2"], commit)
	}
}
