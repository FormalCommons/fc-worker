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
pull request from GitHub:

| Item | State | Next |
|---|---|---|
| Issue labelled `proposed` or `close-proposed` | needs triage | a **triage** job |
| Issue with an open blocker or an open sub-issue | waiting | nothing, until those close |
| Issue that an open pull request will close | in review | nothing; the pull request moves it |
| Any other open issue | ready | a **work** job |
| Pull request with a conflict, failing checks, changes requested on its latest commit, or removed from the merge queue since its approval | needs fix | a **fix** job |
| Pull request whose checks are running | waiting | nothing, until they finish |
| Pull request whose latest commit has no review from the reviewer | needs review | a **review** job |
| Pull request approved on its latest commit, with checks passing | approved | auto-merge, so the merge queue takes it |
| Anything labelled `needs-human`, and draft pull requests | parked | nothing; a person decides |

Every state either has a job or waits for something that has one, so
nothing is left behind. Limits send an item to `needs-human` rather than
around the loop forever.

## Jobs

| Job | For | Runs as | The agent |
|---|---|---|---|
| work | a ready issue | the agent App | makes progress on the issue: opens a pull request that closes it, adds issues for what it needs first (as sub-issues or blockers, labelled `proposed`), or both. If the issue needs no code, it labels it `close-proposed` and says why. |
| fix | a pull request that needs fixing | the agent App | pushes commits to the pull request's branch that address the reviews, failing checks or conflict. |
| review | a pull request that needs review | the reviewer App | reviews the latest commit and approves or requests changes. |
| triage | an issue labelled `proposed` or `close-proposed` | the agent App | for `proposed`: removes the label, after editing the issue if needed, or closes it as not planned or a duplicate. For `close-proposed`: closes the issue, or removes the label and says why it still needs work. |

Each job is one fresh agent in a fresh container, started with the job's
prompt. The prompts are in [prompts/](prompts/); a repository can use its
own. Fix only runs on pull requests that the agent App opened. Other pull
requests get reviews, and their authors fix them.

## The worker

Each worker runs one job at a time:

1. **Survey.** Read the open issues and pull requests, and the claims, with
   two GraphQL queries.
2. **Pick.** Choose the first unclaimed job, in this order: fix, review,
   triage, work. Within each, oldest first.
3. **Claim** the item (see [Claims](#claims)). If another worker was faster,
   pick again.
4. **Run** the job's command, such as `podman run … hive-agent -once …`,
   with the rendered prompt. It must finish within the job's timeout.
5. **Check.** Read the item again. If nothing changed, comment that the
   round made no progress.
6. **Release** the claim.

When there is nothing to do, it waits and surveys again.

The worker also acts on its own, without an agent:

- It turns on auto-merge for an approved pull request whose checks pass.
- It labels an item `needs-human` and says why, when
  - a pull request has had changes requested `review_rounds` times;
  - an item has had `idle_rounds` rounds without progress;
  - a person's request for changes still blocks a pull request that the
    reviewer approved.

## Claims

A claim is a git ref in the repository, such as `refs/fc-claims/issue-12`.
It points to an empty commit whose message records the worker and when the
claim expires. A worker takes a claim with
`git push --force-with-lease`, which succeeds only if the ref has not
changed since the worker looked, so two workers can never both take it.
A claim lasts for the job's timeout plus a margin; a worker that dies loses
its claims when they expire. Nothing else depends on claims: they only
keep two workers off one item.

## Labels

| Label | Meaning |
|---|---|
| `proposed` | An agent created this issue. A triage job checks it before anyone works on it. |
| `close-proposed` | An agent says this issue needs no code. A triage job checks that before it is closed. |
| `needs-human` | The worker stopped working on this. The comment above says why. Remove the label to hand it back. |

People steer the work through issues, comments and reviews. An issue a
person opens is ready at once. Labelling anything `needs-human` stops the
workers from touching it.

## Setting up a repository

1. Create two GitHub Apps, one for agents and one for the reviewer, and
   install them on the repository:

   | App | Repository permissions |
   |---|---|
   | agent | Contents, Issues and Pull requests: read and write. Actions, Checks, Commit statuses: read. No Workflows, so no agent can change CI. |
   | reviewer | Pull requests and Contents: read and write. Issues, Checks, Commit statuses: read. |

   Two Apps are needed because GitHub does not let a pull request's author
   approve it.
2. Add CI that runs on `pull_request` and `merge_group`.
3. Protect the default branch with a ruleset: require a pull request with
   one approval, dismiss approvals on new commits, require the CI checks,
   and require the merge queue. Allow auto-merge in the repository's
   settings.
4. Write `AGENTS.md`: what the project is, its conventions, and how to
   build and test it.
5. Open the first issue, such as "Formalize the plan in PLAN.md". The first
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

## Security notes

- Agents are not trusted: they read text that anyone can write into an
  issue or pull request. The container is their sandbox.
- An agent holds a broker token for its job's client and, through it,
  GitHub tokens that last at most an hour and only have its job's
  permissions. A misled agent can do with them what its job could: for
  example, close issues or push to another agent's branch. It cannot merge
  without an approval from the reviewer, change the default branch except
  through the merge queue, or change CI.
- The worker holds the broker token for the workers' client. It runs on the
  host, not in a container, and is trusted.
- The gateway and the broker serve plain HTTP. Keep them on a private
  network.

## Not yet checked

- That the reviewer App's approval counts toward the ruleset's required
  approval.
- That turning on auto-merge for an approved pull request whose checks
  pass puts it in the merge queue, or whether the worker must add it to the
  queue itself.
- That GraphQL lists refs under `refs/fc-claims/`.
