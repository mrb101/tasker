# tasker

A local, single-binary work tracker for one person and many coding agents.

- **One binary**: server, web UI, JSON API, MCP endpoint and CLI (Go + embedded SQLite, no CGO).
- **Queues, not boards**: a queue is one git worktree. Stories in a queue run in order, one branch each.
- **Reviewable documents**: research, plans, reviews and decisions have revisions, diffs, and an approve/request-changes loop.
- **Agent-first**: agents use the CLI (`tasker agent-guide`), or MCP if they have to.

## Run it

```sh
docker compose up -d --build        # http://localhost:7777, data in the tasker-data volume
# or
make build && ./bin/tasker serve --db ./tasker.db
```

Put the same binary on your host path (`make install`) so you and your agents can use the CLI.
It talks to `TASKER_URL` (default `http://localhost:7777`).

## Concepts

| Thing    | Ref      | Notes |
|----------|----------|-------|
| Project  | `API`    | 2-10 letters/digits. Optional **plan gate**: items can only start with an approved plan. |
| Epic     | `API-E2` | Groups items (planning view). Progress bar + dependency graph. |
| Item     | `API-12` | story, bug, security, research, chore. Status: triage → queued → active → review → done. |
| Queue    | `API/auth-lane` | One worktree. Ordered items. Base branch (default `main`), optionally *stacked*. |
| Document | `API-D3` | research, plan, review, decision, note. draft → in_review → approved / changes_requested. |

- Items not in a queue sit in **triage** (stored as status `inbox`); move them into queues by dragging
  in the web UI (or `tasker queue add auth-lane API-12 --top`).
- Dependencies (`blocked_by`) are across queues and projects; cycles are rejected. `next` skips blocked items.
- Branches: each item gets one (`api-12-rate-limit-login`), recorded when started. Items *may* share a
  branch (e.g. a batch of small review fixes via `item bulk --branch fix/batch`); the UI shows it.
- tasker never runs git. It records branch names and tells the agent the base branch.
- Documents link to items/epics with a role: `plans`, `informs`, `found` (review findings → the items they created).

## Agent setup

```sh
tasker agent-guide >> CLAUDE.md          # or AGENTS.md — compact usage guide
export TASKER_ACTOR=claude-auth          # name each agent
cd ~/src/api-auth && tasker queue bind auth-lane   # bind a worktree to a queue
```

A typical loop inside a bound worktree:

```sh
tasker next                     # what's next here (plan status, branch, base)
tasker doc create --type plan --for API-12 -f plan.md --submit
tasker doc wait API-D3          # exit 0 approved, 10 changes requested
tasker start                    # claim + active + branch; prints the git command
tasker finish --pr https://…    # → review
```

Review agents file findings in one go:

```sh
tasker doc push reviews/security-2026-09.md --submit        # id saved into the file's frontmatter
tasker item bulk findings.yaml --doc API-D9 --queue sec-lane
```

### MCP

HTTP: point your client at `http://localhost:7777/mcp`. Stdio: `claude mcp add tasker -- tasker mcp --as claude`.
Tools mirror the CLI (`tasker_next`, `tasker_start`, `tasker_create_items`, `tasker_create_doc`, …).

## Web UI

- **Queues** board: the Triage tray, then one column per queue; drag to schedule and reorder; quick add (`c`),
  filter (`f`). Click a card for a quick view: read it, move it to a queue or change its priority, step
  through the column with ←/→, and open the full page only when you need it.
- **Items / Epics / Docs** lists, detail pages with inline editing, dependencies, comments, activity.
- **Reviews**: every document waiting for you across projects, plus items waiting to merge.
- Document page: approve / request changes, revision history, diff between revisions.
- Live: pages refresh when agents change things (server-sent events).
- Navigation is a sidebar with a project switcher; on narrow screens it folds into a menu.
- Your name for comments/approvals: click the name at the bottom of the sidebar.

## API

JSON over HTTP under `/api` (projects, items, epics, queues, docs, next, search, activity, events, backup).
Errors are `{"error": "...", "kind": "not_found|conflict|invalid"}` with 404/409/400.
Writes need `Content-Type: application/json`; cross-origin writes are refused (there is no auth, keep it on localhost).
Optimistic concurrency: `if_version` on items, `if_revision` on documents.

## Reach it from other devices

tasker listens on localhost only. To also listen on another address, such as this machine's
[Tailscale](https://tailscale.com/) address, set it in `.env` (see `.env.example`) and restart:

```sh
echo "TASKER_EXTRA_BIND_ADDRESS=$(tailscale ip -4)" > .env
docker compose up -d
```

Other devices then use `http://<tailscale-ip-or-name>:7777`, and their CLI uses the same address
in `TASKER_URL`. **tasker has no login**: everyone who can reach that address can read and change
everything, so only do this on a network you trust.

## Data

One SQLite file (`TASKER_DB`, `/data/tasker.db` in Docker). Consistent snapshot: `tasker backup out.db`
or `GET /api/backup`. Export documents as markdown: `tasker doc pull -p API --out docs/tasker`.

## Troubleshooting

`TASKER_DEBUG=1 tasker serve` logs every request with its duration.

## Development

```sh
make test     # store + HTTP tests
make build    # bin/tasker
```

Layout: `internal/store` (SQLite + all rules), `internal/server` (API, MCP, web UI templates/static, embedded),
`internal/cli` (cobra commands), `internal/render` (markdown, diff).

Third-party code bundled into the binary is listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## License

MIT — see [LICENSE](LICENSE).
