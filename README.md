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
   ├──work──► draft pull request ──continue──► pull request  (work too long for one job)
   │                  ▲      │
   │                  └──────┘
   └──work──► new issues (proposed) ──triage──► ready, or closed
```

Each time a worker looks, it works out the state of every open issue and
pull request from GitHub. The first row that matches decides:

| Item | State | Next |
|---|---|---|
| Anything labelled `needs-human` | parked | nothing; a person decides |
| Draft pull request | unfinished | a **continue** job if the agent App opened it; otherwise nothing, until its author marks it ready |
| Anything opened by someone outside the project | outside | nothing; a maintainer decides |
| Issue labelled `proposed` or `close-proposed` | needs triage | a **triage** job |
| Issue with an open blocker or an open sub-issue | waiting | nothing, until those close |
| Issue that an open pull request from the project will close, or that a pull request merged less than five minutes ago will close (GitHub closes it seconds after the merge) | in review | nothing; the pull request moves it |
| Any other open issue | ready | a **work** job |
| Pull request whose label `merge-first` has [counted](#merging-one-pull-request-first) for less than three days and that the reviewer has never approved, with a conflict, with checks that are running or have passed, and with neither an approval nor a request for changes from the reviewer, nor a request for changes from a person in the project, on its latest commit | waiting, then needs review | nothing while the checks run, then a **review** job: until its approval it holds nothing back, so a fix for each new conflict would only start its checks and its review again |
| Pull request with a conflict, failing checks, changes requested on its latest commit by the reviewer or a person in the project, or removed from the merge queue twice since its approval and its latest commit, and not back in it | needs fix | a **fix** job if the agent App opened it; otherwise its author fixes it |
| Pull request whose mergeability GitHub is still working out, as it does after every merge | waiting | nothing, until GitHub knows |
| Pull request whose checks are running or have not started | waiting | nothing, until they finish; without CI, forever |
| Pull request with neither an approval nor a request for changes from the reviewer on its latest commit | needs review | a **review** job |
| Pull request of the agent App that the reviewer approved on its latest commit, that GitHub would take, and that neither has auto-merge on nor is in the merge queue, while another pull request [merges first](#merging-one-pull-request-first) | held back | nothing, until that one has merged or has held the others back for `merge_first_limit` |
| The pull request that merges first, if the agent App opened it, with such an approval, when its latest commit was made before it began to hold the others back and the base branch has commits that it lacks | needs fix | a **fix** job, which merges the base branch, so that it catches up once before the merge queue takes it. If GitHub does not say whether it lacks any, nothing, until a survey gets the answer |
| Pull request approved by the reviewer on its latest commit | approved | auto-merge, so the merge queue takes it, if the agent App opened it; otherwise its author decides. If GitHub does not count the approval, or a person's request for changes stands, the worker parks it. After a first removal from the merge queue since the approval and the latest commit, it turns auto-merge on again, once, since the cause may be outside the code, such as a failed download in CI |

Every state either has a job or waits for something that has one, so
nothing is left behind. Limits send an item to `needs-human` rather than
around the loop forever: `idle_rounds` rounds in a row that change
nothing, or `max_rounds` rounds of any kind on one item, such as a pull
request's reviews and fixes.

Outside the project means anyone but the two Apps, the `maintainers` in the
config, and the repository's owners, organization members and
collaborators; for issues, also the `apps` in the config. The repository is public, so anyone can open an issue or
pull request, and agents must not take that as their task. To hand such an item to the agents, a maintainer opens
an issue of their own.

## Jobs

| Job | For | Runs as | The agent |
|---|---|---|---|
| work | a ready issue | the agent App | makes progress on the issue in one of three ways: opens a pull request that closes it; splits it into new issues, labelled `proposed`, as its sub-issues or blockers; or, if it needs no change, labels it `close-proposed` and says why. If the work is too long for one job, it opens the pull request as a draft, with notes on what is done and what remains. |
| continue | a draft pull request that the agent App opened | the agent App | goes on from the branch and the notes, and pushes. When the work is finished it marks the pull request ready for review; otherwise it brings the notes up to date for the next job. |
| fix | a pull request that needs fixing | the agent App | pushes commits to the pull request's branch that address the reviews, failing checks or conflict. |
| review | a pull request that needs review | the reviewer App | reviews the latest commit, merged with the latest base branch, and approves or requests changes. Work it finds that belongs in another pull request becomes a new issue, labelled `proposed`. |
| triage | an issue labelled `proposed` or `close-proposed` | the agent App | for `proposed`: removes the label, after editing the issue if needed, or closes it as not planned or a duplicate. For `close-proposed`: closes the issue, or removes the label and says why it still needs work. |

Each job is one fresh agent in a fresh container, started with the job's
prompt. Example prompts are in [prompts/](prompts/). A project can use its
own, as files or from a git repository that the workers follow during a run
(see [Configuration](#configuration)). Fix only runs on pull requests that
the agent App opened. Other pull requests get reviews, and their authors fix
them.

A job ends when its work is done or its time is up, and nothing but what
it pushed to GitHub outlives it. Work that must be one pull request and
takes longer than one job, such as moving to a new version of a
dependency, therefore takes several: the work job pushes as it goes and
opens the pull request as a draft, and continue jobs take it up, each with
a fresh agent, until one marks it ready. Until then it gets no review and
no fix, whatever its checks say. Each prompt can tell its agent when the
job will be stopped (`{{.Deadline}}`). `max_rounds` and `idle_rounds`
bound these rounds like any others: a continue job that ends by itself
without a new commit changed nothing. One that is stopped at its deadline
is a failed command, which does not count toward `idle_rounds`, so only
`max_rounds` bounds a draft whose jobs keep running out of time. To hold
a draft of the agent App back from the workers, label it `needs-human`.

## The worker

Each worker runs one job at a time:

1. **Survey.** Read the claims with git, then the open issues and pull
   requests: from GitHub with GraphQL, or from a desk if the config names
   one (see [Surveying from a desk](#surveying-from-a-desk)).
2. **Pick.** Choose the first unclaimed job, in this order: fix, review,
   continue, triage, work. Within each, oldest first. The jobs of a pull
   request that [merges first](#merging-one-pull-request-first) come
   before all others, and after them the jobs that lead to such a pull
   request: for three days, those of an issue whose label `merge-first`
   counts, of the agent's draft that closes it, and of a pull request
   whose label counts and that the reviewer has not approved yet.
3. **Claim** the item (see [Claims](#claims)). If another worker was faster,
   pick again.
4. **Run** the job's command, such as `podman run … hive-agent -once …`,
   with the rendered prompt. It must finish within the job's timeout.
5. **Check.** Read the item again, and comment on it: which job ran, and
   whether it changed anything. These comments begin with hidden markers,
   which count the rounds; only the agent App's comments count.
6. **Release** the claim.

When there is nothing to do, it waits and surveys again. It also does
nothing but wait while fewer than `rate_reserve` points or requests are
left in the agent App's GraphQL or REST rate limit, which it asks GitHub
for before each survey. The workers and agent jobs of an installation
share these limits, 5,000 an hour each unless GitHub grants more, and a
survey of GitHub costs about 3 GraphQL points for every 50 open issues and
25 open pull requests. Review jobs use the reviewer App's limits, which the
worker does not watch. `-list` shows what is left.

A job whose command fails still counts as a round, but not toward
`idle_rounds`, and the worker waits before it goes on. So a failure that
repeats, such as a gateway that is down, does not run through the items at
once.

The worker also acts on its own, without an agent:

- It turns on auto-merge for an approved pull request that the agent App
  opened, which puts it in the merge queue as soon as the ruleset allows.
  If that fails, it looks at the pull request again ten seconds later,
  since a pull request still looks open for a few seconds after it
  merges. It parks the pull request unless it has merged, closed, joined
  the merge queue or had auto-merge turned on meanwhile. It parks only
  for a reason of the pull request's. When GitHub refused for a rate
  limit or for a fault of its own, when the request did not get an answer
  from GitHub's GraphQL at all, or when the pull request cannot be read
  again or is not open when read again, nothing is known of the pull
  request: the worker logs the failure, gives the claim back and waits
  `poll`, and whichever worker next finds the pull request tries again.
  After a rate limit's refusal of any job it also logs what GitHub gave
  as left of its rate limits when the step began and what it gives then.
- It labels an item `needs-human` and says why, when
  - an item has had `idle_rounds` rounds in a row that changed nothing, or
    `max_rounds` rounds in all. Both count from when the item was last
    parked, so removing `needs-human` gives it a fresh start;
  - a person's request for changes on an earlier commit still blocks a
    pull request that the reviewer approved. That person must approve or
    dismiss their review before the label comes off, or the worker parks
    it again;
  - GitHub does not count the reviewer's approval toward the required
    reviews, so the merge queue would never take the pull request. This
    happens when the reviewer App lacks Contents: write (see below).

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
claim's job did is already in the survey of GitHub that follows. A desk's
copy can be older than that, so a worker that surveys from one does more
(see [Surveying from a desk](#surveying-from-a-desk)).

GitHub now and then fails a push, and a claim left unreleased would hold
its item until the lease expired. So a failed release is tried again for
up to a minute, while the claim is still the worker's.

## Labels

| Label | Meaning |
|---|---|
| `proposed` | An agent created this issue. A triage job checks it before anyone works on it. |
| `close-proposed` | An agent says this issue needs no code. A triage job checks that before it is closed. |
| `needs-human` | The worker stopped working on this. The comment above says why. Remove the label to hand it back. |
| `merge-first` | On a pull request, from a maintainer: its jobs come before the others, and from the reviewer's first approval this one merges before all others. On an issue, from a maintainer, or from an App in `apps` that opens the issue with it: each pull request that closes it does, and the issue's own jobs come before the others. See [below](#merging-one-pull-request-first). |

People in the project steer the work through issues, comments and reviews.
An issue that one of them opens is ready at once. Labelling anything
`needs-human` stops the workers from touching it. Agents take direction
only from the project; they treat other text as information.

### Merging one pull request first

A pull request that changes very many files, such as a move to a new
version of a dependency, may never merge by itself while many others do:
each merge can give it a new conflict, and it then needs a fix, its checks
and a review again, which take longer than the next conflict takes to
come. GitHub starts no `pull_request` workflow for a pull request that has
a conflict.

Label such a pull request `merge-first`. What the label does comes in two
parts, while the pull request is open, from the project, ready for review
and not parked.

**Before the reviewer's first approval** it holds nothing back. Its
checks, its reviews and its fixes can take hours, and if the others
waited all that time, nothing would merge for hours. Until then:

- its own jobs come before those of the others;
- a conflict alone does not send it to a fix. While the checks of its
  latest commit run, it waits for them, and when they have passed and
  the commit has no review, it gets its review, with the conflict: the
  default branch still moves, and a fix for each new conflict would start
  its checks and its review again, as above. Failed checks and a request
  for changes lead to a fix as for any pull request, and so does a
  conflict on a commit whose checks never started, which is what a
  conflict does to a new commit: there is nothing to wait for then.

Both last for three days from when the label began to count. After that
its jobs are picked like any others, and a conflict leads to a fix again.

**From the reviewer's first approval** it merges first:

- the workers turn on auto-merge for no other pull request, so the default
  branch stops moving once the merge queue is empty. Pull requests that
  are in the queue already, or have auto-merge on, still merge. All other
  jobs go on, and the approved pull requests go to the merge queue when
  this one has merged;
- its own jobs come before all others;
- it catches up with the default branch once. The others merged until the
  approval, so the commit that was checked and approved usually lacks
  many commits of the base branch, and the merge queue, which builds the
  two together, would be the first to find out whether they still fit.
  So when the pull request is the agent App's, is approved on its latest
  commit and would get auto-merge, and that commit was made before it
  began to hold the others back, the worker asks GitHub how many commits
  the base branch has that it lacks. If there are any, its next job is a
  fix, which merges the base branch and repairs what that breaks; after
  the checks and a review of that commit it gets auto-merge. If there are
  none, it gets auto-merge at once. A commit made after the others began
  to wait is not asked about, since the base branch has stood still since
  then, but for what was in the merge queue already. A person's pull
  request is left to its author, as always.

  So it catches up once for each time that it holds the others back, and
  the date of its latest commit decides. Nothing catches it up again with
  what merges after that: pull requests that were in the merge queue when
  the hold began, or another labelled pull request that merged meanwhile.
  The merge queue's own run tests it together with those, and if that
  fails, the removal from the queue brings a fix as for any pull request.
  If GitHub does not answer the question, the pull request gets no job
  until a survey has the answer, and the worker logs it; the other items
  keep their jobs.

The first approval is the reviewer App's earliest approving review, of
whichever commit. A later commit, or a later request for changes, does
not end the hold. GitHub marks an approval as dismissed when a new commit
makes it stale, so the worker counts each dismissed review of the
reviewer's as an approval of an earlier commit. The worker looks for the
first approval among the latest 100 reviews of a pull request.

A request for changes of the reviewer's that a maintainer dismisses looks
the same, and counts as an approval from when that review was submitted,
not from when it was dismissed. The pull request then holds the others
back from that time, or from when the label began to count if that was
later, so `merge_first_limit` may be partly or wholly over already.
Once it is over, its jobs do not come before the others either, since
that is for a pull request without an approval. A maintainer who
dismisses such a review and wants the pull request to merge first
removes the label and adds it again: the time then runs from the new
label.

The pull request holds the others back for `merge_first_limit`, 4 hours
by default, from the first approval, or from when the label began to
count if that was later, so that a pull request that never gets through
does not hold the others back for good. To start the time anew, remove
the label and add it again. Of two pull requests that hold the others
back, the one with the lower number goes first, and the other waits like
the rest; its own time runs meanwhile.

On a pull request, the label counts only if a maintainer in the config's
`maintainers` added it. From anyone else it does nothing: the agent App
can label anything, so a misled agent could otherwise stop every merge,
and an App in `apps` acts on what workflows run. GitHub does not tell the
worker whether the one who added a label is an owner, organization member
or collaborator, so those must be in `maintainers` for this. The worker
reads the latest 20 events of a pull request (a label added, marked ready,
made a draft), so a label that was added before those does not count
either.

**The label on an issue.** The label can also go on an issue, before
there is a pull request. Each pull request from the project that closes
the issue, by `Closes #N` in its description or a link made on GitHub,
is then treated as if it had the label. The label counts for it from when
it was ready for review: from when it was opened or, if it was opened as a
draft, from when it was first marked ready. If the issue got the label
after that, it counts from then. Making the pull request a draft and ready
again does not change this time, unless 20 events have come since it
was first ready (the worker reads the latest 20). The label does not pass
to the issue's sub-issues.

The jobs that lead to that pull request come before all others too, but
after those of a pull request that merges first already: the issue's own
jobs and the continue jobs of a draft from the agent that closes the
issue, both for three days from the label. A label on the draft itself
gives its jobs nothing. Work is otherwise the last job that a
worker picks, so the change would wait behind everything else while the
others go on merging what it must then follow. This holds nothing back: the
other pull requests stay out of the merge queue only once the reviewer
has approved the pull request.

On an issue the label counts from a maintainer in `maintainers`, and from
an App in `apps` that opened the issue with the label, as a workflow does
with `gh issue create --label merge-first`. So a workflow that opens an
issue for such a change can label it, and nobody has to be there when the
pull request comes. The App's label on an issue that someone else opened,
or one that it adds later, does not count.

The agents decide which pull requests close an issue, so they decide
which ones get its label, and each gets a `merge_first_limit` of its own.
What bounds this is a second limit: a pull request gets nothing from the
issue if it was ready for review three days or more after the issue got
the label. So one label on an issue passes to no pull request that is
ready later than three days after it was added, and each one that gets
it holds the others back once, for `merge_first_limit` at most, from the
reviewer's first approval of it. To end it sooner, remove
the label from the issue; a label that a maintainer put on the pull
request itself still counts. To give such a pull request more time, label
the pull request.

`fc-worker -list` shows, for a pull request that has the label or closes
an issue whose label counts, until when it merges first, that it waits
for the reviewer's approval, or why it does not merge first. For a
labelled issue it shows whether the label counts, if the issue
needs a job or an open pull request closes it.

While an open issue that has the label needs a job, or an open pull
request closes it, each survey costs one GraphQL point more. That is from
when the issue is ready for a job until its pull request has merged,
whether the label counts or not, so take a label that does not count off
again. The worker asks who labelled ten such issues at most: those that a
pull request closes first, and the oldest first.

When the pull request that merges first would get auto-merge on a commit
made before it began to hold the others back, a survey makes two REST
requests more: one for the name of its base branch, which no survey
reads, and one that compares the two. Each worker makes them once for
such a commit if the base branch has commits that it lacks: it keeps that
answer while it runs, since the base branch only gains commits, and does
not ask again while the fix job runs, however often it surveys. So the
catch-up costs two requests for each worker, not two for each survey. If
the commit lacks nothing, the worker turns auto-merge on in the same
step, and nobody asks after that; only a worker that surveys before then
asks too. The requests count toward the agent App's REST limit, not
toward its GraphQL points, which run out first. If GitHub does not answer
them, the survey goes on without the answer: that pull request gets no
job, the worker keeps nothing, and its next survey asks again.

## Setting up a repository

1. Create two GitHub Apps, one for agents and one for the reviewer, and
   install them on the repository:

   | App | Repository permissions |
   |---|---|
   | agent | Contents, Issues and Pull requests: read and write. Actions, Checks, Commit statuses: read. No Workflows, so no agent can change the workflow files. |
   | reviewer | Contents, Issues and Pull requests: read and write. Checks, Commit statuses: read. |

   Two Apps are needed because GitHub does not let a pull request's author
   approve it. The reviewer App's approval counts toward a required
   approval only if the App has write access to Contents. Its hive-broker
   must limit the review jobs' client to Contents: read, so that they can
   approve but not push. Issues: write lets review jobs create issues for
   the work they find.
2. Add CI that runs on `pull_request` and `merge_group`, uses no secrets,
   and gives `GITHUB_TOKEN` read access only. In the repository's Actions
   settings, make `GITHUB_TOKEN` read-only by default and do not let
   Actions approve pull requests.
3. Protect the default branch with a ruleset: require a pull request with
   one approval, dismiss approvals on new commits, require approval of the
   most recent push, require the CI checks, and require the merge queue.
   Allow auto-merge in the repository's settings.
4. Create the labels `proposed`, `close-proposed`, `needs-human` and
   `merge-first`. List the people in the project in the config's
   `maintainers`, or add them as collaborators: the Apps see a private
   organization member as outside the project. A `merge-first` label
   counts only from someone in `maintainers`, and on an issue also from an
   App in `apps` that opens the issue with it. `fc-worker -list` shows how
   the worker sees each item.
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
fc-worker [-config /etc/fc-worker/config.json] [-state /var/lib/fc-worker] [-list | -compare]
```

| Flag | Meaning |
|---|---|
| `-config` | The config file. |
| `-state` | A directory for the worker's own files: bare git repositories per worker, which hold its claim commits and the prompts it fetched. |
| `-list` | Print every open issue and pull request with its next job and who holds its claim, and GitHub's rate limits, then exit. |
| `-compare` | Survey the repository from GitHub and from the config's `desk`, print what differs as one line of JSON, then exit. See "Comparing with a desk". |

The worker logs a line when a job starts and when it ends. A job's
standard output goes to a file of its own in the `logs` directory, named
for when it started, the worker, the item and the job, such as
`20261117T193345Z-vps1-a-pr-12-review.log`; without `logs`, the worker
passes it through. The worker never deletes these files. A job's standard
error always goes to the worker's. SIGTERM or SIGINT stops the worker: a
running job gets SIGTERM, and the worker releases its claim before it
exits.

SIGHUP asks the worker to exit once its job, if it has one, is done and
its claim released; an idle worker exits at once. Its service manager then
starts it again, so a new binary or config takes over without stopping any
job. Send SIGHUP to the worker's process alone: `podman run` passes signals
on to the job's container, which would end the job. Under systemd, give the
unit `ExecReload=kill -HUP $MAINPID` and `Restart=always`, since an exit
after SIGHUP is a clean one, which `Restart=on-failure` would leave stopped.
Then install the new files and run `systemctl reload 'fc-worker@*'`. A
worker from before SIGHUP was handled exits at once on it, stopping its job,
so restart it the first time instead.

### Surveying from a desk

A desk is a service that follows the repository on GitHub, keeps a copy of
every open issue and pull request, and answers `GET /survey?repo=owner/name`
from that copy, which costs nothing of GitHub's rate limits. With `desk` in
the config, each survey of the worker is the desk's, and `-list` shows it
too. If the desk does not answer within 15 seconds, or its answer is more
than five minutes old, the worker surveys GitHub that time, and logs why.

The desk's copy is usually up to a minute behind GitHub, and the worker
takes a survey whose `as_of` is up to five minutes old. What only the
desk's full read brings, such as a sub-issue that someone else attached to
an issue, can be ten minutes behind. So the copy can be behind the claims
too: it may still show a job that another worker has done and released
since. The worker therefore has the desk read one item from GitHub at once,
with `POST /refresh`, at three points:

- After it claims an item that it picked from the desk's survey. It then
  surveys again. If the item no longer needs the same job, the worker
  releases the claim and tries the next item; otherwise it runs the job
  on the item as it is now.
- Before it reads the item again after the job, to see what the job
  changed.
- Before it releases the claim after a job, so that the next survey of any
  worker shows what the job did.

So what an old copy costs is a delay, or a claim that is given back, and
not a wrong job.

If the desk fails the first or the second of these, the worker surveys
GitHub instead. After a claim, it then picks the rest of that step's items
from GitHub's survey, which needs no second look; if GitHub fails too, the
worker releases the claim and runs no job. A refresh that fails before a
release is only logged.

A refresh covers the job's own item only. Other items that a job creates,
such as a new pull request or new sub-issues, reach the desk's surveys with
its next sweep, within about a minute; the refreshed issue itself already
shows its new closing pull request and sub-issues. For the same reason a
`merge-first` label (see
[Merging one pull request first](#merging-one-pull-request-first)), or the
approval from which it holds the others back, can take up to a minute
longer to do so than with surveys of GitHub:
the look after a claim reads only the claimed item again.

Each refresh costs the desk's GitHub App a point or so, and the agent App
nothing. The worker still asks GitHub for its rate limits before each
survey, for who labelled an issue `merge-first`, once for how far the
pull request that merges first is behind its base branch, and for an
item's comments after a job, and it writes to GitHub as before.

### Comparing with a desk

`-compare` is for testing a desk: it reads the facts from the desk, then
from GitHub, works out the items from each as a survey does, and prints one
line of JSON:

```json
{
  "at": "2026-11-24T23:40:10Z", "as_of": "2026-11-24T23:39:30Z",
  "github": {"issues": 61, "pulls": 24}, "desk": {"issues": 61, "pulls": 24},
  "differ": [{
    "item": "pr-20",
    "fields": {"Queued": [true, false]},
    "jobs": ["", ""],
    "updated": ["2026-11-24T23:40:02Z", "2026-11-24T23:39:10Z"]
  }]
}
```

`at` is when it asked GitHub, and `as_of` how old the desk said its answer
was. Each pair is GitHub's, then the desk's. `differ` lists the items for
which the two give something different: `fields` holds the fields that
differ, `jobs` the job the item needs next according to each, and `updated`
when GitHub said the item last changed. An item that only one of them has
carries `"only": "github"` or `"only": "desk"`, and an item with two
different `jobs` is listed also when no field of its own differs. What
changed on GitHub between `as_of` and the end of GitHub's answer, some
seconds after `at`, differs without a fault of the desk. A difference that
lasts from one run to the next is one to look at, and so is an issue whose
title, labels or author differ while its two `updated` are equal. The rest
changes without a new `updated`: a pull request's checks, conflicts and
place in the merge queue, and for an issue the states of its blockers,
sub-issues and closing pull requests and who labelled it.

It reads no claims and writes nothing. It costs what a survey costs, and
one point more if an issue labelled `merge-first` needs a job or an open
pull request closes it: who labelled the issue is then asked once for each
side. So are the two REST requests about a pull request that merges first
and waits to catch up with its base branch, unless the first answer is
that it lacks commits: that answer serves both sides.

### Configuration

The worker reads one JSON file. In its strings, other than durations,
`${NAME}` is replaced with the environment variable `NAME`; if that is
unset or empty, the worker refuses to start.

```json
{
  "repo": "FormalCommons/example",
  "name": "vps1-a",
  "broker": { "url": "http://broker-agent:8080", "token": "${WORKER_BROKER_TOKEN}" },
  "agent": "fc-agent[bot]",
  "reviewer": "fc-reviewer[bot]",
  "maintainers": ["alice"],
  "logs": "/var/log/fc-worker",
  "poll": "30s",
  "jobs": {
    "work": {
      "prompt": "/etc/fc-worker/prompts/work.md",
      "timeout": "90m",
      "limit": "85m",
      "command": ["podman", "run", "--rm", "--network", "fc",
                  "--userns", "keep-id:uid=1000,gid=1000",
                  "--env-file", "/etc/fc-worker/agent.env",
                  "-v", "{prompt}:/prompt.md:ro",
                  "ghcr.io/formalcommons/fc-agent",
                  "-cli", "claude", "-session", "fresh", "-once", "-timeout", "85m",
                  "-prompt-file", "/prompt.md", "--", "--dangerously-skip-permissions"]
    },
    "continue": { "…": "the same, with prompts/continue.md" },
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
| `apps` | Logins of other GitHub Apps, such as `github-actions[bot]`, whose issues count as the project's own: for example those of a scheduled workflow that opens issues for the agents. The label `merge-first` counts on an issue that such an App opens with it. Their pull requests and reviews still count as outside. See the security notes before listing one. |
| `maintainers` | Logins of people in the project, besides the repository's owners, organization members and collaborators. List any whose organization membership is private. Remove a login before its owner renames or deletes the account, since someone else could then take it. |
| `prompts` | Optional: a git repository that holds the prompts (see below), as `repo`, a URL that git can fetch with the credentials of the user the worker runs as; `ref`, the branch to follow, default `main`; and `refresh`, how long to use a fetched commit before fetching again, default `1m`. `ref` can also be a tag or a commit, to hold the prompts still. |
| `jobs` | For each of `work`, `continue`, `fix`, `review` and `triage`: `prompt`, the path of its prompt, a file, or a path in the `prompts` repository if there is one; `command`, which runs the job, with each `{prompt}` replaced by the path of the rendered prompt; `timeout`, after which the command gets SIGTERM, and SIGKILL 30 seconds later; and optionally `limit`, at most `timeout`, if the command stops its agent sooner by itself, as hive-agent's `-timeout` does. The prompt's `{{.Deadline}}` is the job's start plus `limit`, or plus `timeout` without it. |
| `idle_rounds` | How many rounds in a row that change nothing park an item. Default 3. |
| `max_rounds` | How many rounds of any kind park an item. Default 20. |
| `poll` | How long to wait when there is nothing to do, and after an error, give or take a quarter at random, so that workers that started together do not keep surveying together. Default `2m`. |
| `rate_reserve` | How much of the agent App's GraphQL and REST rate limits to leave for running jobs: the worker waits while fewer points or requests remain. Default 1000; set 1 to turn the check off. |
| `merge_first_limit` | How long a pull request whose label `merge-first` counts holds the others back from the merge queue, from the reviewer's first approval of it, or from when the label began to count if that was later: when it was added or, for a pull request that closes a labelled issue, when it was ready for review or when the issue got the label, whichever was later. Default `4h`. |
| `desk` | Optional: a desk that follows the repository, as `url` and `token`, this worker's client token for it. The worker then surveys from the desk, and from GitHub only when the desk does not answer or its answer is old (see "Surveying from a desk"). Without it, the worker reads everything from GitHub. |
| `logs` | A directory for the jobs' standard output, a file per job. The worker creates it. Without it, the output goes to the worker's own. |

A prompt is a Go [text/template](https://pkg.go.dev/text/template). It can
use `{{.Repo}}` (`owner/name`), `{{.Owner}}`, `{{.Name}}`, `{{.Kind}}`
(`issue` or `pull request`), `{{.Number}}`, `{{.Title}}`, `{{.Why}}`, why
the item needs the job, such as "checks failed", and `{{.Deadline}}`, when
the job will be stopped, such as "2026-11-23 20:29 UTC". Two more are true
or false, for `{{if .First}}…{{end}}`: `{{.First}}` for the pull request
that [merges first](#merging-one-pull-request-first), and `{{.Leads}}` for
an item whose jobs lead to one and so come before the others: an issue
whose label `merge-first` counts, the agent's draft that closes it, or a
pull request whose label counts and that the reviewer has not approved
yet. A worker whose binary does not know a field cannot render a prompt
that uses it: it rejects such a commit of the `prompts` repository and
keeps the one it had. So upgrade every worker before prompts that use a
new field merge. Files named `_*.md`
beside a prompt are read with it, so prompts can share text:
[prompts/_checks.md](prompts/_checks.md), the points that work, continue,
fix and review jobs all check, is included with `{{template "_checks.md" .}}`.

The worker reads the prompt again for every job, so prompts can change
while it runs. With `prompts`, they live in a git repository of their own,
and a change takes effect once it reaches the branch the workers follow.
Before it claims an item, a worker fetches the branch if it last fetched
`refresh` ago or more. It takes up the new commit only if every job's
prompt renders from it, and reads each job's prompt and its `_*.md` files
from that one commit, so they always match. If a fetch fails, or the new
commit does not render, the worker goes on with the commit it has, which
it keeps across restarts; with none yet, it claims nothing. Its round
comment names the commit, as "with prompts at 1a2b3c4d5e6f", and its log
says which commit each job's prompt came from.

Anyone who can push to that branch decides what every agent is told. Keep
the repository out of the agents' reach: leave it out of the brokers'
`repositories`, or out of the Apps' installations, and let only people push
to it.

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
  most recent push keeps whoever pushed last from approving. It can create
  and edit issues, including issues without `proposed`, which skip triage.
- A known weak point: review agents build and run the code under review,
  in their own container, which holds the reviewer's broker token and a
  gateway token. So a pull request written to do so could take GitHub
  tokens as the reviewer and approve itself. It still cannot push, as long
  as the reviewer's broker limits its client to Contents: read.
  Keeping the tokens out of the container would not help, since the code
  could still write the reviewer's verdict. The fix is to run the code
  under review outside the reviewer's sandbox. Until then this relies on
  every agent and prompt being the project's own, and on the repository
  being limited to collaborators.
- Agents cannot change the workflow files, but CI runs the build scripts
  on their branches. That is why CI gets no secrets and a read-only
  `GITHUB_TOKEN`.
- The worker holds the broker token for the workers' client. It runs on the
  host, not in a container, and is trusted.
- The gateway and the broker serve plain HTTP. Keep them on a private
  network.
- An App listed in `apps` writes the agents' tasks: its issues are worked
  on like a maintainer's. `github-actions[bot]` acts only through the
  repository's own workflows, with the permissions each declares, so list
  it only if no workflow that someone outside the project can start
  (`issues`, `issue_comment`, `pull_request_target`, `workflow_run`, `fork`,
  `watch` and the like) can write issues, and if the workflows that do write
  them put in only what the project controls, such as fixed text and a
  checked tag name, never text from elsewhere. Agents can make it act
  through the code that workflows run. That gives them two things that
  they cannot do as the agent App: each issue that it opens with the label
  `merge-first` lets each of their pull requests that is ready for review
  within three days hold the others back from the merge queue, for at
  most `merge_first_limit` from the reviewer's first approval of it, and
  has its own jobs, those of their drafts that close it and those of such
  a pull request before its approval picked before the others, for three
  days each (see
  [Merging one pull request first](#merging-one-pull-request-first)). So
  a workflow that opens issues with this label must open them rarely, and
  by a rule that agents cannot get around: they can edit and close its
  issues.

## Open points

- An approved pull request with auto-merge on that the merge queue does not
  take, for a reason other than those above, waits without a limit. So
  does a pull request whose mergeability GitHub never works out, though
  every survey asks it to.
- Issues that block each other, or are each other's sub-issues, wait
  forever. Nothing detects such a cycle yet.
- Without the Workflows permission, the agent App cannot push commits that
  change workflow files. So a fix or continue job cannot merge in a base
  branch whose workflows changed since the pull request began.
- Whether the Apps can still comment and review while the repository's
  interactions are limited to collaborators.
- Whether GitHub still counts the reviewer's approval, under "require
  approval of the most recent push", after a fix job pushes a bare merge
  of the base branch. GitHub has kept such approvals so far; if it stops,
  the worker parks the pull request as "approval does not count" instead
  of putting it back in the merge queue.
