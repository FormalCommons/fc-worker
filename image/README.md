# fc-agent

The image that fc-worker's jobs run in:
[hive-agent](https://github.com/FormalCommons/hive/tree/main/agent), plus

- `gh`, as a wrapper that fetches a GitHub token from hive-broker for every
  call;
- `git-credential-fc`, a git credential helper that does the same for
  `git`, set up for `https://github.com`;
- `fc-token`, which both use: it prints a token from hive-broker;
- elan, in `/opt/elan`, without a toolchain. A repository's
  `lean-toolchain` picks one, and elan installs it on first use.

CI publishes it as `ghcr.io/formalcommons/fc-agent`, tagged `latest` and
`sha-` plus the commit. It is built for linux/amd64 only. A new package on
GitHub starts private: make it public in its package settings, or log
worker hosts in with `podman login ghcr.io`.

## Environment

| Variable | Meaning |
|---|---|
| `HIVE_BROKER_URL`, `HIVE_BROKER_TOKEN` | The broker and the client token whose GitHub tokens the job uses: the agent App's for work, continue, fix and triage, the reviewer App's for review. |
| `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN` | hive-gateway and the job's gateway token, for `claude`. |
| `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, `GIT_COMMITTER_EMAIL` | Who the job's commits are by, such as the agent App. |

## Caches

Each job starts from scratch, so each would download its Lean toolchain
and Mathlib's build cache again. Two mounts keep them on the host:

| Mount | Holds |
|---|---|
| `/opt/elan/toolchains` | Lean toolchains |
| `/home/node/.cache/mathlib` | Mathlib's downloaded build cache |

Both hold programs that jobs run, such as `lean`, `lake` and `leantar`,
and any job that can write to them can change those programs for every job
that uses them after it. So share them only among work, continue, fix and triage
jobs, which run as the same App anyway. Give review jobs no cache, or a
cache of their own: a review job that ran a program a work agent had
planted would run it with the reviewer's token, read-only mount or not.
