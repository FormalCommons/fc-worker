package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testConfig = `{
  "repo": "o/r",
  "name": "w1",
  "broker": {"url": "http://broker:8080/", "token": "${TEST_BROKER_TOKEN}"},
  "agent": "fc-agent[bot]",
  "reviewer": "fc-reviewer[bot]",
  "jobs": {
    "work":   {"prompt": "work.md",   "command": ["run", "{prompt}"], "timeout": "90m"},
    "fix":    {"prompt": "fix.md",    "command": ["run", "{prompt}"], "timeout": "90m"},
    "review": {"prompt": "review.md", "command": ["run", "--prompt={prompt}"], "timeout": "30m"},
    "triage": {"prompt": "triage.md", "command": ["run", "{prompt}"], "timeout": "10m"}
  }
}`

func writeConfig(t *testing.T, s string) string {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("TEST_BROKER_TOKEN", "secret")
	c, err := loadConfig(writeConfig(t, testConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Broker.Token != "secret" || c.Broker.URL != "http://broker:8080" {
		t.Errorf("broker %+v", c.Broker)
	}
	if c.Jobs["review"].Timeout.Duration != 30*time.Minute {
		t.Errorf("review timeout %v", c.Jobs["review"].Timeout)
	}
	if c.IdleRounds != 3 || c.MaxRounds != 20 || c.Poll.Duration != 2*time.Minute {
		t.Errorf("defaults %d %d %v", c.IdleRounds, c.MaxRounds, c.Poll)
	}
}

func TestConfigErrors(t *testing.T) {
	t.Setenv("TEST_BROKER_TOKEN", "secret")
	for _, tc := range []struct{ old, new, want string }{
		{"${TEST_BROKER_TOKEN}", "${TEST_UNSET}", "TEST_UNSET"},
		{`"o/r"`, `"r"`, "owner/name"},
		{`"fc-reviewer[bot]"`, `"fc-agent[bot]"`, "different Apps"},
		{`"--prompt={prompt}"`, `"--prompt"`, "{prompt}"},
		{`"triage"`, `"other"`, "unknown job"},
		{`"10m"`, `"ten"`, "duration"},
		{`"name": "w1"`, `"name": "w 1"`, "name"},
		{`"repo"`, `"repository"`, "unknown field"},
	} {
		_, err := loadConfig(writeConfig(t, strings.Replace(testConfig, tc.old, tc.new, 1)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s -> %s: %v, want an error about %q", tc.old, tc.new, err, tc.want)
		}
	}
}
