# Issue tracker: GitHub

Issues and PRDs for this repo live as GitHub issues at **`TakuroBreath/sociopulse-platform`**. Use the `gh` CLI for all operations.

## Conventions

- **Create an issue**: `gh issue create --title "..." --body "..."`. Use a heredoc for multi-line bodies.
- **Read an issue**: `gh issue view <number> --comments`, filtering comments by `jq` and also fetching labels.
- **List issues**: `gh issue list --state open --json number,title,body,labels,comments --jq '[.[] | {number, title, body, labels: [.labels[].name], comments: [.comments[].body]}]'` with appropriate `--label` and `--state` filters.
- **Comment on an issue**: `gh issue comment <number> --body "..."`
- **Apply / remove labels**: `gh issue edit <number> --add-label "..."` / `--remove-label "..."`
- **Close**: `gh issue close <number> --comment "..."`

Infer the repo from `git remote -v` — `gh` does this automatically when run inside a clone.

## When a skill says "publish to the issue tracker"

Create a GitHub issue in this repo (`TakuroBreath/sociopulse-platform`). For cross-repo work that touches the frontend or infra, also link to / cross-reference the corresponding issues in `TakuroBreath/sociopulse-web` or `TakuroBreath/sociopulse-infra`.

## When a skill says "fetch the relevant ticket"

Run `gh issue view <number> --comments`.

## Authentication

The active `gh` account for this repo is **`TakuroBreath`** (`maxsmurffy@gmail.com`). The local git config in this repo (`git config --local user.{name,email}`) is also set to `TakuroBreath / maxsmurffy@gmail.com` — different from any global git identity. Never rely on the global config for commits in this repo.
