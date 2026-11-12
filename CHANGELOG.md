# Changelog

Notable changes to tasker. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Add an entry under
**Unreleased** with each change; `make release V=x.y.z` turns that section into the release.

## [Unreleased]

### Added

- A Live page (`/live`): work in progress and in review across projects as cards, and a feed of the last 24
  hours, updating as it happens. Wall mode (`?wall=1`) is for a screen left open: big type, no sidebar, the
  screen kept awake. It holds a bounded snapshot, so a page left open for days does not grow.
- Stories remember what they changed. `tasker finish`, run in the story's worktree, reads git and records the
  files (lines added and removed, renames) and commits of the branch since it left its base — the queue's base
  branch, or the story ahead of it in a stacked queue. The story page lists them, cards and the quick view show
  the totals, and `tasker item changes` prints them. `tasker item changes --record` measures again, e.g. after
  review fixes. The server still never runs git; MCP clients pass `changes` to `tasker_finish` themselves.
- `tasker backfill` finds what earlier stories changed in the repository's history — their branch, merge
  commits that name them or their branch, the merge or squash commit of their pull request, or commits made for
  them (the ref leading the subject, or a "Refs:" trailer; passing mentions do not count) — and records it with `--apply` (a dry run
  otherwise). `--gh` asks GitHub as a last resort. Each story says how its changes were found.
- Items can point at places in the code: `--at path:12-20` on `tasker add` and `item update`, `locations:` in
  `item bulk` and MCP. The CLI pins each to the current commit with the lines as a snippet; the item page shows
  them, linked to the exact lines when the project has a repository address. `tasker backfill --locations`
  suggests locations for older items from the places their text names with line numbers (`--include-files`
  for bare file names too).
- `tasker context [PATH…]` (and MCP `tasker_context`): what tasker knows about files — items in flight that
  change them, open findings and bugs at them, and the finished stories that changed them with their plans and
  decisions. Agents report the files they are changing as they work (`next`, `context`, MCP `tasker_report_wip`)
  and stories can declare what they will change (`--touches`, or `touches:` in their plan's frontmatter). `next` and `start` warn when another queue is
  changing the same files, without blocking, and log each warning in the story's activity; board cards show
  the overlap and the story page lists work on the same files.
- `tasker why FILE[:LINE[-END]]` (and MCP `tasker_lookup_commits`): from lines of code (git blame) or a file's
  history to the stories that made them, with their plans, decisions and reviews. Commits are matched by the
  commits recorded for stories, item refs in messages, PR numbers in squash commits, or merged branch names.
- A project's repository address and link style (Settings, or `tasker project set KEY --repo-url … --repo-host …`)
  link files and commits on GitHub, GitLab (also on its own domain), Gitea/Forgejo/Codeberg, Bitbucket, or
  through your own address patterns (cgit, gitweb). Without one — plain git on a machine — there are no links
  and everything else works the same. GitLab merge requests ("See merge request group/repo!12") are matched
  like pull requests.

## [0.1.0] - 2026-11-12

The first tagged release.

### Added

- One Go binary that serves a web UI, a JSON API, an MCP endpoint and the agents' CLI over an embedded SQLite
  database. Work is organised as projects, epics and items; queues map one-to-one to git worktrees and run their
  items in order, one branch each. Research, plans and reviews are documents with revisions and an
  approve/request-changes loop.
- A project dashboard for a chosen period: stories done, open and blocked work, cycle time, documents waiting
  for review, scope against done, throughput, progress by epic, work per queue, oldest work in progress and
  activity by person or agent. Cards show the epic's short reference and name.
- Bulk triage: select stories on the board, queue pages and item lists and change their queue, priority, epic,
  status or labels together in one transaction. `tasker item update` accepts several refs.
- Notifications when a review or a merge waits on someone: browser notifications from open tabs, and an ntfy
  topic through `TASKER_NOTIFY_URL` (`TASKER_PUBLIC_URL` makes them open the right page).
- Automatic database backups: a snapshot every 24 hours, keeping the newest 14, configurable through
  `TASKER_BACKUP_EVERY`, `TASKER_BACKUP_KEEP` and `TASKER_BACKUP_DIR`. Settings lists snapshots for download.
- Releases: a goal plus the stories that reach it, shipped on a fixed date or when their scope is done. The
  release page forecasts from measured pace, lists risks, and shows a burn-up, scope history and release notes.
- The release in progress, planned releases and the last shipped one on the dashboard.
- A release timeline on the Releases page and the dashboard, with likely-finish ranges. Releases without a fixed
  date are forecast one after another.
- Forecasts start from a release's start date, are given in days, and say how far a release is ahead of or
  behind its target.
- Forecasts count only stories that were started before they were finished, and include the current week.
- Board columns can be minimized to a narrow strip, remembered per project.
- Optional serving through a shared Traefik with `compose.edge-proxy.yaml`.

### Changed

- The web UI works well on phones.
- Started stories are found in one pass over the activity log, so forecasts load faster.

[Unreleased]: https://github.com/mrb101/tasker/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/mrb101/tasker/releases/tag/v0.1.0
