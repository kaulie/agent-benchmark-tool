# Branching & PR flow

## Branch naming

Create one branch per task, named after the task id:

| Prefix | Use for |
|---|---|
| `feature/<taskId>-<slug>` | new capability |
| `fix/<taskId>-<slug>` | bug fix |
| `issue/<taskId>-<slug>` | work driven by a reported issue |

Example: `feature/task-701ab0bbdd5d45fd-phase1-reason-turns-list`.

Branch from `main` and keep the branch scoped to a single task.

## Flow

```bash
git clone https://github.com/kaulie/agent-benchmark-tool.git
cd agent-benchmark-tool
git checkout -b feature/<taskId>-<slug>
# ... work ...
make lint test
git commit -m "feat: ..."
git push -u origin HEAD
gh pr create --fill --base main
```

- Open the PR against `main`; never push directly to `main`.
- A PR must pass `make lint` and `make test` before review.
- **Merging is a human decision.** The agent that opened the PR reports the PR
  URL and waits; it does not merge. Release/deploy is triggered only when the
  user asks for it (see README → 部署).

## Commits

Conventional commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`)
with a one line summary focused on the *why*.
