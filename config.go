package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Config is the worker's JSON config file. String values may contain
// ${NAME}, which is replaced with the environment variable NAME when the file
// is loaded, so secrets can stay in the environment.
type Config struct {
	// Repo is the repository to work on, as "owner/name".
	Repo string `json:"repo"`
	// Name names this worker in claims and comments. Give every worker its
	// own.
	Name string `json:"name"`
	// Broker hands out the worker's own GitHub tokens, of the agent App.
	Broker Broker `json:"broker"`
	// Agent and Reviewer are the logins of the two GitHub Apps, such as
	// "fc-agent[bot]".
	Agent    string `json:"agent"`
	Reviewer string `json:"reviewer"`
	// Jobs holds how to run each job: "work", "fix", "review" and "triage".
	Jobs map[string]Job `json:"jobs"`
	// IdleRounds is how many rounds in a row that change nothing an item
	// may have before the worker labels it needs-human. Default 3.
	IdleRounds int `json:"idle_rounds"`
	// MaxRounds is how many rounds of any job an item may have before the
	// worker labels it needs-human. Both limits count from when the item
	// was last labelled needs-human. Default 20.
	MaxRounds int `json:"max_rounds"`
	// Poll is how long to wait when there is nothing to do. Default 2m.
	Poll duration `json:"poll"`
}

// Broker is a hive-broker and a client token for it.
type Broker struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// Job is how to run one kind of job.
type Job struct {
	// Prompt is the path of the prompt, a Go text/template.
	Prompt string `json:"prompt"`
	// Command runs the job. Each "{prompt}" in it is replaced with the
	// path of the rendered prompt.
	Command []string `json:"command"`
	// Timeout stops a job that takes longer.
	Timeout duration `json:"timeout"`
}

var jobNames = []string{"work", "fix", "review", "triage"}

// duration is a time.Duration written as a string, such as "90m".
type duration struct{ time.Duration }

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	d.Duration = v
	return err
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.expandEnv(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${NAME} in every string value. An unset or empty
// variable is an error, so a missing secret is caught at startup.
func (c *Config) expandEnv() error {
	var missing []string
	expand := func(s *string) {
		*s = envRef.ReplaceAllStringFunc(*s, func(ref string) string {
			name := ref[2 : len(ref)-1]
			v := os.Getenv(name)
			if v == "" {
				missing = append(missing, name)
			}
			return v
		})
	}
	for _, s := range []*string{&c.Repo, &c.Name, &c.Broker.URL, &c.Broker.Token, &c.Agent, &c.Reviewer} {
		expand(s)
	}
	for name, j := range c.Jobs {
		expand(&j.Prompt)
		for i := range j.Command {
			expand(&j.Command[i])
		}
		c.Jobs[name] = j
	}
	if len(missing) > 0 {
		return fmt.Errorf("unset or empty environment variables: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (c *Config) validate() error {
	if !repoName.MatchString(c.Repo) {
		return errors.New(`repo must be "owner/name"`)
	}
	if !workerName.MatchString(c.Name) {
		return errors.New("name must start with an ASCII letter or digit and contain only ASCII letters, digits, '.', '_' and '-'")
	}
	if c.Broker.URL == "" || c.Broker.Token == "" {
		return errors.New("broker: url and token are required")
	}
	c.Broker.URL = strings.TrimSuffix(c.Broker.URL, "/")
	if c.Agent == "" || c.Reviewer == "" {
		return errors.New("agent and reviewer are required")
	}
	if c.Agent == c.Reviewer {
		return errors.New("agent and reviewer must be different Apps, since an author cannot approve its own pull request")
	}
	for name, j := range c.Jobs {
		if !slices.Contains(jobNames, name) {
			return fmt.Errorf("jobs: unknown job %q", name)
		}
		if j.Prompt == "" || len(j.Command) == 0 || j.Timeout.Duration <= 0 {
			return fmt.Errorf("jobs: %s: prompt, command and timeout are required", name)
		}
		if !slices.ContainsFunc(j.Command, func(a string) bool { return strings.Contains(a, "{prompt}") }) {
			return fmt.Errorf("jobs: %s: command does not use {prompt}", name)
		}
	}
	for _, name := range jobNames {
		if _, ok := c.Jobs[name]; !ok {
			return fmt.Errorf("jobs: %s is missing", name)
		}
	}
	if c.IdleRounds == 0 {
		c.IdleRounds = 3
	}
	if c.MaxRounds == 0 {
		c.MaxRounds = 20
	}
	if c.Poll.Duration == 0 {
		c.Poll.Duration = 2 * time.Minute
	}
	if c.IdleRounds < 0 || c.MaxRounds < 0 || c.Poll.Duration < 0 {
		return errors.New("idle_rounds, max_rounds and poll must not be negative")
	}
	return nil
}

var (
	repoName   = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	workerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)
