// Command fc-worker runs coding agents on a GitHub repository's issues and
// pull requests until the work is merged. See README.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"math/rand/v2"
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
	compare := flag.Bool("compare", false, "print, as JSON, the items for which the config's desk and GitHub give different facts, then exit")
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
	if *compare {
		// Before anything touches the claims: comparing reads none.
		if c.Desk == nil {
			log.Fatal("-compare: the config names no desk")
		}
		cmp, err := w.compare(ctx)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(cmp); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := w.claims.init(ctx); err != nil {
		log.Fatal(err)
	}
	if *list {
		if err := w.list(ctx, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if p := c.Prompts; p != nil {
		w.prompts = &promptRepo{url: p.Repo, ref: p.Ref, refresh: p.Refresh.Duration,
			dir: filepath.Join(*stateDir, c.Name, "prompts.git")}
		for _, job := range jobNames {
			w.prompts.files = append(w.prompts.files, c.Jobs[job].Prompt)
		}
		if err := w.prompts.init(ctx); err != nil {
			log.Fatal(err)
		}
	}
	// SIGHUP asks the worker to finish its job, if it has one, and exit, so
	// that its service manager starts it again with a new binary or config.
	hup := make(chan os.Signal, 1)
	if !signal.Ignored(syscall.SIGHUP) {
		signal.Notify(hup, syscall.SIGHUP)
	}
	log.Printf("fc-worker %s working on %s", c.Name, c.Repo)
	for ctx.Err() == nil {
		// After an error, wait before trying again, so that a failure
		// that repeats does not repeat at once.
		found, err := w.step(ctx)
		if err != nil {
			log.Print(err)
		}
		select {
		case <-hup:
			log.Print("SIGHUP: exiting, to be started again")
			return
		default:
		}
		if !found || err != nil {
			select {
			case <-ctx.Done():
			case <-hup:
				log.Print("SIGHUP: exiting, to be started again")
				return
			case <-time.After(jitter(c.Poll.Duration)):
			}
		}
	}
}

// jitter returns d, give or take a quarter at random, so that workers that
// started together do not keep surveying together and racing for the same
// items.
func jitter(d time.Duration) time.Duration {
	return d*3/4 + rand.N(d/2+1)
}
