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
- The server never runs git. It records branch names and tells the agent the base branch.
- What a story changed: `tasker finish`, run in the story's worktree, has the CLI read git and record the branch's
  files and commits since its base. The story page lists them (linked when the project has a repository address
  in Settings), cards show `+120 −30`, and `tasker item changes [--record]` shows or re-measures them.
  For stories finished before that, run `tasker backfill -p API` in the repository: it finds each story's
  changes in the git history (its branch, the merge or squash commit of its PR, or commits naming it), shows
  what it found, and saves it with `--apply`. `--gh` also asks GitHub about the story's pull request.
- Code locations: an item can point at places in the code (`tasker add … --at internal/auth/login.go:40-58`,
  `locations:` in `item bulk`). Run inside the repository, the CLI pins each one to the current commit and
  keeps the lines as a snippet, so a review finding still reads right after the code moves. For items written
  before, `tasker backfill -p API --locations` turns the files they name in their text into locations.
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
- Bulk triage: tick cards or list rows (shift-click for a range, "Select shown" per column), then move them
  to a queue, set priority, epic or status, or add/remove a label from the bar at the bottom — all or nothing.
  Triage sorts (priority, newest, oldest, by epic) and filters (epic, label, blocked) on the server.
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

## Releases

A release is a goal (what someone can do once it ships) plus the stories that reach it, drawn from any epics.
Each project chooses in Settings how its releases work:

- **Fixed date**: a release ships on its target date with whatever is done; the rest moves to the next release.
  With a rhythm (say every 4 weeks) a new release's dates follow on from the previous one.
- **Fixed scope**: a release ships when its stories are done.

Put stories in a release from the bulk bar (tick stories, choose **Release**, and **committed** or **stretch**),
from a story's page, or a whole epic at once from the release page. The release page shows:

- **a forecast** from your measured pace, not estimates. It replays the stories finished per week over the last
  eight weeks and this one 4,000 times: in date mode it says how many more stories are done by the target (85% sure and
  even odds) and marks each story *likely*, *maybe* or *at risk*; in scope mode it gives the likely finish date.
  Only stories that were started before they were finished count, so bulk closes and imports drop out; the
  current week counts too, scaled up by the days elapsed, so a change of pace shows at once.
- **risks**: stories waiting on something outside the release, waiting on a decision, or without an approved plan
  when the plan gate is on;
- a burn-up of the release's scope against done, its scope history, and **release notes** grouped by epic.

The **timeline** on the Releases page (and in the dashboard's Releases section) puts every release on one
calendar: a bar from start to target, or to the day it shipped, filled by what is done; target ticks; a line
for today; and the likely-finish range (even odds to 85% sure). Releases without a target date are worked one
after another, so each one's forecast counts the open stories of the releases before it, and its dashed
*expected* bar starts when the one before is likely done — a roadmap that moves as work lands.

**Ship** records the git tag and moves unfinished stories to the next planned release (or one you choose). One
release is in progress at a time, and `tasker next -p KEY` prefers its stories. On the command line:
`tasker release create|list|show|start|ship|notes|add|add-epic|remove`. (Giving up a claim on a story is
`tasker unclaim`.)

## Notifications

tasker tells you when something waits on you: a document submitted for review (usually a plan an agent
is blocked on) or a story finished into review (a pull request waiting to merge).

- **In the browser:** click **Notify me** at the bottom of the sidebar and allow notifications. Any open
  tasker tab then shows a system notification; when the tab is in front you get a message on the page.
- **On your phone, with no tab open:** set `TASKER_NOTIFY_URL` to an [ntfy](https://ntfy.sh) topic (and
  `TASKER_PUBLIC_URL` so tapping it opens the page) in `.env`, subscribe to that topic in the ntfy app,
  and restart. See `.env.example`. Any URL that accepts a plain-text POST works as well.

You are not notified about your own actions.

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

### Behind a shared Traefik

If one Traefik on this machine owns port 80 and routes names to projects over an external `proxy`
network (only containers labelled `edge-proxy.enable=true`), tasker can join it under a name of its
own. Add to `.env` and restart:

```sh
COMPOSE_FILE=docker-compose.yml:compose.edge-proxy.yaml
TASKER_HOST=tasker.example.lan
```

Port 7777 stays bound for the CLI and MCP. Set `TASKER_PUBLIC_URL=http://tasker.example.lan` too if
notifications should open that address.

## Data and backups

One SQLite file (`TASKER_DB`, `/data/tasker.db` in Docker). Export documents as markdown with
`tasker doc pull -p API --out docs/tasker`.

**Backups are automatic.** The server snapshots the database every 24 hours (and on start when the newest
snapshot is older than that) and keeps the newest 14. Settings shows them, with a download link each and
**Back up now**. In Docker they go to their own `tasker-backups` volume, apart from the data. Change it in `.env`:

| Variable | Default | |
|---|---|---|
| `TASKER_BACKUP_EVERY` | `24h` | how often; `0` = only when you click Back up now |
| `TASKER_BACKUP_KEEP` | `14` | how many to keep |
| `TASKER_BACKUP_HOST_DIR` | the `tasker-backups` volume | an absolute folder on this machine instead (it must be writable by uid 65532: `docker run --rm -v /path/to/folder:/b alpine chown 65532:65532 /b`) |

Outside Docker the default folder is `backups/` next to the database (`TASKER_BACKUP_DIR` changes it).
A snapshot of the moment is always one command away: `tasker backup out.db`.

**To restore**, stop tasker, put the snapshot in place of the database, and start it again:

```sh
docker compose stop
docker run --rm -v tasker_tasker-data:/data -v tasker_tasker-backups:/backups alpine \
  sh -c 'rm -f /data/tasker.db-wal /data/tasker.db-shm && cp /backups/tasker-20261103-020000.db /data/tasker.db && chown 65532:65532 /data/tasker.db'
docker compose start
```

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

### Cutting a release

Record each change under **Unreleased** in [CHANGELOG.md](CHANGELOG.md) as you make it. To release, on an
up-to-date `main`:

```sh
make release V=0.2.0
```

This moves the Unreleased notes under `0.2.0` with today's date, runs the tests, commits, tags `v0.2.0` and
pushes. The tag starts the release workflow, which builds Linux and macOS binaries and publishes a GitHub
release with that version's notes. Versions follow semver: bump the patch for fixes, the minor for features.

## License

MIT — see [LICENSE](LICENSE).
