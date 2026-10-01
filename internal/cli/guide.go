package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const agentGuide = `# tasker — agent guide

tasker is the work tracker for this project. Use the ` + "`tasker`" + ` CLI (server: $TASKER_URL).
Always identify yourself: set TASKER_ACTOR=<your-agent-name> or pass --as NAME.
Add --json to any command for machine-readable output.

## Concepts
- Project key (API). Items: API-12 (story|bug|security|research|chore). Epics: API-E2. Documents: API-D3.
- Queue = one git worktree. Items in a queue run in order; each item is one branch.
  In a worktree bound to a queue, next/start/finish need no arguments.
- Stories not in any queue are in triage (stored as status "inbox" in --json output).
- Documents (research, plan, review, decision, note) are reviewed by a human:
  draft → in_review → approved | changes_requested → (update) → in_review ...
- Dependencies: an item blocked by unfinished items is skipped by ` + "`next`" + ` and cannot start.
- tasker never runs git. It records branch names; you create/switch branches.

## Work loop
  tasker next                          # next item for this worktree's queue (or -p KEY for project priority)
  tasker start                         # claims it, marks active, prints branch + base: run the git command shown
  # if the project has a plan gate (or the item is non-trivial), plan first:
  tasker doc create --type plan --for API-12 -f plan.md --submit
  tasker doc wait API-D3               # exit 0 approved, 10 changes requested (read comments, revise:)
  tasker doc update API-D3 -f plan.md  # new revision, back to review
  # ... implement on the branch, commit, open PR ...
  tasker finish --pr <url> -m "summary"    # → review (use --done if merged/no PR)

## Creating work
  tasker add "Title" -t bug --priority high -e API-E2 -q auth-lane --blocked-by API-10 -f body.md
  tasker item bulk findings.yaml --doc API-D9      # many items at once (see: tasker item bulk --help)
  tasker epic create "Epic title" -f epic.md
  tasker doc create --type research --for API-E2 -f notes.md
  tasker doc create --type review -f security-review.md --submit   # then file findings with bulk --doc

## Reading
  tasker show API-12 | API-E2 | API-D3   # full detail incl. comments and linked docs
  tasker ls [-s open|triage|queued|active|review|done] [-t type] [-e API-E2] [-q queue] [--search text]
  tasker queue show                      # ordered queue for this worktree
  tasker doc show API-D3 --raw           # markdown only
  tasker search "rate limit"

## Updating
  tasker item update API-12 --status done --add-label perf --if-version 4   # exit 4 if changed meanwhile
  tasker link API-12 --blocked-by API-10 ; tasker unlink API-12 --blocked-by API-10
  tasker comment API-12 "text"           # or -f file / - for stdin
  tasker queue add auth-lane API-12 API-13 [--top|--before API-11|--after API-11]
  tasker queue move API-13 --to other-lane ; tasker queue remove API-13   # back to triage
  tasker doc push docs/plans/x.md        # create/update from a file; id saved in its frontmatter

## Exit codes
  0 ok · 1 error · 2 usage · 3 not found · 4 conflict (claimed, blocked, plan gate, version) · 5 invalid input
  doc wait: 0 approved · 10 changes requested · 11 superseded/draft · 12 timeout
`

func (a *app) guideCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "agent-guide",
		Short: "Print a compact usage guide for coding agents (paste into CLAUDE.md / AGENTS.md)",
		Args:  cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			fmt.Fprint(a.out, agentGuide)
		},
	}
}

// mcpCmd bridges stdio MCP clients to the server's HTTP /mcp endpoint.
func (a *app) mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server on stdio (proxies to TASKER_URL/mcp) for MCP-only clients",
		Long: `Speaks MCP over stdio and forwards each message to the tasker server.
Example (Claude Code):  claude mcp add tasker -- tasker mcp --as claude
Clients that support HTTP MCP can use TASKER_URL/mcp directly.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			in := bufio.NewReader(os.Stdin)
			out := bufio.NewWriter(os.Stdout)
			client := &http.Client{}
			for {
				line, err := in.ReadBytes('\n')
				if len(bytes.TrimSpace(line)) > 0 {
					req, _ := http.NewRequest("POST", strings.TrimRight(a.url, "/")+"/mcp", bytes.NewReader(line))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Accept", "application/json, text/event-stream")
					req.Header.Set("X-Tasker-Actor", a.actor)
					res, herr := client.Do(req)
					if herr != nil {
						fmt.Fprintln(os.Stderr, "tasker mcp:", herr)
					} else {
						body, _ := io.ReadAll(res.Body)
						res.Body.Close()
						if res.StatusCode != http.StatusAccepted && len(bytes.TrimSpace(body)) > 0 {
							var compact bytes.Buffer
							if json.Compact(&compact, body) != nil {
								compact.Reset()
								compact.Write(bytes.ReplaceAll(bytes.TrimSpace(body), []byte("\n"), nil))
							}
							out.Write(compact.Bytes())
							out.WriteByte('\n')
							out.Flush()
						}
					}
				}
				if err != nil {
					if err == io.EOF {
						return nil
					}
					return err
				}
			}
		},
	}
}
