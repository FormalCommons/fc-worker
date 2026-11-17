// Command fc-worker runs coding agents on a GitHub repository's issues and
// pull requests until the work is merged. See README.md.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "/etc/fc-worker/config.json", "path to the JSON config file")
	stateDir := flag.String("state", "/var/lib/fc-worker", "directory for the worker's own files")
	list := flag.Bool("list", false, "print every open issue and pull request with its next job, then exit")
	flag.Parse()

	c, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	gh := newGitHub(c)
	w := &worker{c: c, gh: gh, claims: &claims{
		url:   "https://github.com/" + c.Repo + ".git",
		dir:   filepath.Join(*stateDir, c.Name, "claims.git"),
		name:  c.Name,
		token: gh.getToken,
	}}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := w.claims.init(ctx); err != nil {
		log.Fatal(err)
	}
	if *list {
		if err := w.list(ctx, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if c.Logs != "" {
		if err := os.MkdirAll(c.Logs, 0o700); err != nil {
			log.Fatal(err)
		}
	}
	log.Printf("fc-worker %s working on %s", c.Name, c.Repo)
	for ctx.Err() == nil {
		// After an error, wait before trying again, so that a failure
		// that repeats does not repeat at once.
		found, err := w.step(ctx)
		if err != nil {
			log.Print(err)
		}
		if !found || err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(c.Poll.Duration):
			}
		}
	}
}
