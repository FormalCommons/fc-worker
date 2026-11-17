# fc-worker

Runs coding agents on a GitHub repository's issues and pull requests until
the work is merged. Issues are the tasks. Agents work on them, open pull
requests, and add new issues as the work reveals them. Fresh agents review
every pull request until they approve it, and GitHub's merge queue merges
it. All state lives on GitHub, so any number of workers on any number of
machines can share a repository without a coordinator.

The worker decides what gets worked on next and makes sure nothing is
forgotten. Agents do the work, and they read and write GitHub themselves.
The worker knows nothing about the project: what to build, and how, is in
the repository's issues and its `AGENTS.md`.

fc-worker builds on [hive](https://github.com/FormalCommons/hive): agents
run in hive-agent containers, reach models through hive-gateway, and get
short-lived GitHub tokens from hive-broker.

## How work flows

```
 issue ──work──► pull request ──review──► approved ──► merge queue ──► merged, issue closed
   │                  ▲   │
   │                  └fix┘  (changes requested, checks failed, conflict)
   └──work──► new issues (proposed) ──triage──► ready, or closed
```

Each time a worker looks, it works out the state of every open issue and
pull request from GitHub. The first row that matches decides:

| Item | State | Next |
|---|---|---|
| Anything labelled `needs-human`, and draft pull requests | parked | nothing; a person decides |
| Anything opened by someone outside the project | outside | nothing; a maintainer decides |
| Issue labelled `proposed` or `close-proposed` | needs triage | a **triage** job |
| Issue with an open blocker or an open sub-issue | waiting | nothing, until those close |
| Issue that an open pull request from the project will close | in review | nothing; the pull request moves it |
| Any other open issue | ready | a **work** job |
| Pull request with a conflict, failing checks, changes requested on its latest commit by the reviewer or a person in the project, or removed from the merge queue since its approval | needs fix | a **fix** job if the agent App opened it; otherwise its author fixes it |
| Pull request whose checks are running | waiting | nothing, until they finish |
| Pull request with neither an approval nor a request for changes from the reviewer on its latest commit | needs review | a **review** job |
| Pull request approved by the reviewer on its latest commit | approved | auto-merge, so the merge queue takes it |

Every state either has a job or waits for something that has one, so
nothing is left behind. Limits send an item to `needs-human` rather than
around the loop forever: `idle_rounds` rounds in a row that change
nothing, or `max_rounds` rounds of any kind on one item, such as a pull
request's reviews and fixes.

Outside the project means anyone but the two Apps and the repository's
owners, organization members and collaborators. The repository is public,
so anyone can open an issue or pull request, and agents must not take
that as their task. To hand such an item to the agents, a maintainer opens
an issue of their own.

## Jobs

| Job | For | Runs as | The agent |
|---|---|---|---|
| work | a ready issue | the agent App | makes progress on the issue in one of three ways: opens a pull request that closes it; splits it into new issues, labelled `proposed`, as its sub-issues or blockers; or, if it needs no change, labels it `close-proposed` and says why. |
| fix | a pull request that needs fixing | the agent App | pushes commits to the pull request's branch that address the reviews, failing checks or conflict. |
| review | a pull request that needs review | the reviewer App | reviews the latest commit and approves or requests changes. |
| triage | an issue labelled `proposed` or `close-proposed` | the agent App | for `proposed`: removes the label, after editing the issue if needed, or closes it as not planned or a duplicate. For `close-proposed`: closes the issue, or removes the label and says why it still needs work. |

Each job is one fresh agent in a fresh container, started with the job's
prompt. The prompts are in [prompts/](prompts/); a repository can use its
own. Fix only runs on pull requests that the agent App opened. Other pull
requests get reviews, and their authors fix them. Agents open pull requests
ready for review, never as drafts.

## The worker

Each worker runs one job at a time:

1. **Survey.** Read the claims with git, then the open issues and pull
   requests with GraphQL.
2. **Pick.** Choose the first unclaimed job, in this order: fix, review,
   triage, work. Within each, oldest first.
3. **Claim** the item (see [Claims](#claims)). If another worker was faster,
   pick again.
4. **Run** the job's command, such as `podman run … hive-agent -once …`,
   with the rendered prompt. It must finish within the job's timeout.
5. **Check.** Read the item again, and comment on it: which job ran, and
   whether it changed anything. These comments begin with hidden markers,
   which count the rounds; only the agent App's comments count.
6. **Release** the claim.

When there is nothing to do, it waits and surveys again.

The worker also acts on its own, without an agent:

- It turns on auto-merge for an approved pull request whose checks pass.
- It labels an item `needs-human` and says why, when
  - an item has had `idle_rounds` rounds in a row that changed nothing, or
    `max_rounds` rounds in all. Both count from when the item was last
    parked, so removing `needs-human` gives it a fresh start;
  - a person's request for changes on an earlier commit still blocks a
    pull request that the reviewer approved. That person must approve or dismiss their review
    before the label comes off, or the worker parks it again.

The worker acts as the agent App, through a hive-broker client of its own.

## Claims

A claim is a git ref in the repository, such as `refs/fc-claims/issue-12`.
It points to an empty commit whose message records the worker and when the
claim expires. A worker takes a claim with
`git push --force-with-lease`, which succeeds only if the ref has not
changed since the worker looked, so two workers can never both take it.
A claim lasts for the job's timeout plus a margin; a worker that dies loses
its claims when they expire. Nothing else depends on claims: they only
keep two workers off one item.

A worker reads the claims before the issues and pull requests, and
releases a claim only after its job is done. To release it, the worker
replaces its lease with one that has already expired, rather than deleting
the ref. Each lease commit's parent is the one it replaces, so a claim
never returns to a state that another worker saw before, and a worker can
only take a claim if nobody has taken and released it since it looked.
The claim's history is the item's history of jobs. Whatever a released
claim's job did is already in the survey that follows.

## Labels

| Label | Meaning |
|---|---|
| `proposed` | An agent created this issue. A triage job checks it before anyone works on it. |
| `close-proposed` | An agent says this issue needs no code. A triage job checks that before it is closed. |
| `needs-human` | The worker stopped working on this. The comment above says why. Remove the label to hand it back. |

People in the project steer the work through issues, comments and reviews.
An issue that one of them opens is ready at once. Labelling anything
`needs-human` stops the workers from touching it. Agents take direction
only from the project; they treat other text as information.

## Setting up a repository

1. Create two GitHub Apps, one for agents and one for the reviewer, and
   install them on the repository:

   | App | Repository permissions |
   |---|---|
   | agent | Contents, Issues and Pull requests: read and write. Actions, Checks, Commit statuses: read. No Workflows, so no agent can change the workflow files. |
   | reviewer | Pull requests: read and write. Contents, Issues, Checks, Commit statuses: read. It can review, but not push. |

   Two Apps are needed because GitHub does not let a pull request's author
   approve it.
2. Add CI that runs on `pull_request` and `merge_group`, uses no secrets,
   and gives `GITHUB_TOKEN` read access only. In the repository's Actions
   settings, make `GITHUB_TOKEN` read-only by default and do not let
   Actions approve pull requests.
3. Protect the default branch with a ruleset: require a pull request with
   one approval, dismiss approvals on new commits, require approval of the
   most recent push, require the CI checks, and require the merge queue.
   Allow auto-merge in the repository's settings.
4. Create the labels `proposed`, `close-proposed` and `needs-human`.
5. Write `AGENTS.md`: what the project is, its conventions, and how to
   build and test it.
6. Open the first issue, such as "Formalize the plan in PLAN.md". The first
   work job breaks it into issues.

The merge queue needs a public repository owned by an organization, or a
private one on GitHub Enterprise Cloud.

## Running

On one machine, for example:

- hive-gateway, with the model providers' keys;
- two hive-broker instances, one per App. Their clients are the workers,
  the agent jobs, and the review jobs, each limited to what it needs;
- an fc-worker process per slot, under systemd, each starting its jobs in
  fresh containers of the agent image.

The agent image adds to hive-agent `gh`, a git credential helper and a
`gh` wrapper that fetch tokens from hive-broker when they are needed, and
elan for Lean. Containers reach GitHub, the gateway and the broker.

### Usage

```
fc-worker [-config /etc/fc-worker/config.json] [-state /var/lib/fc-worker] [-list]
```

| Flag | Meaning |
|---|---|
| `-config` | The config file. |
| `-state` | A directory for the worker's own files: a bare git repository per worker, which holds its claim commits. |
| `-list` | Print every open issue and pull request with its next job and who holds its claim, then exit. |

The worker logs a line when a job starts and when it ends, and passes the
job's output through. SIGTERM or SIGINT stops it: a running job gets
SIGTERM, and the worker releases its claim before it exits.

### Configuration

The worker reads one JSON file. In any string value, `${NAME}` is replaced
with the environment variable `NAME`; if that is unset or empty, the
worker refuses to start.

```json
{
  "repo": "FormalCommons/example",
  "name": "vps1-a",
  "broker": { "url": "http://broker-agent:8080", "token": "${WORKER_BROKER_TOKEN}" },
  "agent": "fc-agent[bot]",
  "reviewer": "fc-reviewer[bot]",
  "jobs": {
    "work": {
      "prompt": "/etc/fc-worker/prompts/work.md",
      "timeout": "90m",
      "command": ["podman", "run", "--rm", "--network", "fc",
                  "--userns", "keep-id:uid=1000,gid=1000",
                  "--env-file", "/etc/fc-worker/agent.env",
                  "-v", "{prompt}:/prompt.md:ro",
                  "ghcr.io/formalcommons/fc-agent",
                  "-cli", "claude", "-session", "fresh", "-once", "-timeout", "85m",
                  "-prompt-file", "/prompt.md", "--", "--dangerously-skip-permissions"]
    },
    "fix": { "…": "the same, with prompts/fix.md" },
    "review": { "…": "the same, with prompts/review.md and reviewer.env" },
    "triage": { "…": "the same, with prompts/triage.md" }
  }
}
```

| Field | Meaning |
|---|---|
| `repo` | The repository, as `owner/name`. |
| `name` | This worker, in claims and comments. Give every worker its own name. |
| `broker` | A hive-broker for the agent App, and this worker's client token for it. |
| `agent`, `reviewer` | The two Apps' logins. |
| `jobs` | For each of `work`, `fix`, `review` and `triage`: `prompt`, the path of its prompt; `command`, which runs the job, with each `{prompt}` replaced by the path of the rendered prompt; and `timeout`, after which the command gets SIGTERM, and SIGKILL 30 seconds later. |
| `idle_rounds` | How many rounds in a row that change nothing park an item. Default 3. |
| `max_rounds` | How many rounds of any kind park an item. Default 20. |
| `poll` | How long to wait when there is nothing to do. Default `2m`. |

A prompt is a Go [text/template](https://pkg.go.dev/text/template). It can
use `{{.Repo}}` (`owner/name`), `{{.Owner}}`, `{{.Name}}`, `{{.Kind}}`
(`issue` or `pull request`), `{{.Number}}`, `{{.Title}}` and `{{.Why}}`, why
the item needs the job, such as "checks failed". The worker reads the
prompt again for every job, so it can change while the worker runs.

`agent.env` and `reviewer.env` give the job's container what it needs:
`HIVE_BROKER_URL` and `HIVE_BROKER_TOKEN` for its broker client,
`ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` for hive-gateway, and the
name and email for its commits.

## Security notes

- Agents are not trusted: they read text that anyone can write into an
  issue or pull request. The container is their sandbox.
- An agent holds a broker token for its job's client and, through it,
  GitHub tokens that last at most an hour and only have its job's
  permissions. A misled agent can do with them what its job could: for
  example, close issues, push to another agent's branch, or create issues
  without `proposed`, which skips triage. It cannot change the default
  branch except through the merge queue, and it cannot approve its own
  pull requests, since the agent App opened them.
- The agent App can approve pull requests that it did not open. The design
  needs the reviewer App's approval to count toward the ruleset, and then
  the agent App's counts too. So a misled agent could approve and
  auto-merge a pull request that a person opened. For the experiment,
  limit the repository's interactions to collaborators, for up to six
  months, so that nobody outside the project can open one.
- Review agents are not trusted either. A misled reviewer can approve a bad
  pull request, but it cannot change one, and requiring approval of the
  most recent push keeps whoever pushed last from approving.
- Agents cannot change the workflow files, but CI runs the build scripts
  on their branches. That is why CI gets no secrets and a read-only
  `GITHUB_TOKEN`.
- The worker holds the broker token for the workers' client. It runs on the
  host, not in a container, and is trusted.
- The gateway and the broker serve plain HTTP. Keep them on a private
  network.

## Open points

- Whether the reviewer App's approval counts toward the ruleset's required
  approval, with only read access to Contents.
- Whether turning on auto-merge for an approved pull request whose checks
  pass puts it in the merge queue, or whether the worker must add it to the
  queue itself.
- Issues that block each other, or are each other's sub-issues, wait
  forever. Nothing detects such a cycle yet.
- Without the Workflows permission, the agent App cannot push commits that
  change workflow files. So a fix job cannot merge in a base branch whose
  workflows changed since the pull request began.
- Whether the Apps can still comment and review while the repository's
  interactions are limited to collaborators.
