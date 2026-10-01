package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/mrb101/tasker/internal/store"
)

// ---- projects ----

func (a *app) projectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Aliases: []string{"projects"}, Short: "Manage projects"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List projects", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return call(a, "GET", "/api/projects", nil, func(ps []*store.Project) {
				if len(ps) == 0 {
					fmt.Fprintln(a.out, "(no projects) create one: tasker project create KEY \"Name\"")
					return
				}
				w := tw(a.out)
				fmt.Fprintln(w, "KEY\tNAME\tTRIAGE\tQUEUED\tACTIVE\tREVIEW\tDONE\tDOCS TO REVIEW")
				for _, p := range ps {
					c := p.Counts
					fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\n", p.Key, p.Name, c.Inbox, c.Queued, c.Active, c.Review, c.Done, c.DocsInReview)
				}
				w.Flush()
			})
		},
	})
	var desc string
	create := &cobra.Command{
		Use: "create KEY [NAME]", Short: "Create a project (KEY like API: 2-10 letters/digits)", Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) > 1 {
				name = args[1]
			}
			return call(a, "POST", "/api/projects", map[string]string{"key": args[0], "name": name, "description": desc}, func(p *store.Project) {
				fmt.Fprintf(a.out, "created project %s (%s)\n", p.Key, p.Name)
			})
		},
	}
	create.Flags().StringVarP(&desc, "description", "d", "", "description")
	cmd.AddCommand(create)
	cmd.AddCommand(&cobra.Command{
		Use: "show KEY", Short: "Show a project", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "GET", "/api/projects/"+esc(args[0]), nil, func(p *store.Project) {
				fmt.Fprintf(a.out, "%s  %s\n", p.Key, p.Name)
				if p.Description != "" {
					fmt.Fprintln(a.out, p.Description)
				}
				c := p.Counts
				fmt.Fprintf(a.out, "plan gate: %v\ntriage %d · queued %d · active %d · review %d · done %d\nqueues %d · open epics %d · docs to review %d\n",
					p.PlanGate, c.Inbox, c.Queued, c.Active, c.Review, c.Done, c.Queues, c.Epics, c.DocsInReview)
			})
		},
	})
	var name, pdesc, gate string
	set := &cobra.Command{
		Use: "set KEY", Short: "Change project settings", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			patch := map[string]any{}
			if cmd.Flags().Changed("name") {
				patch["name"] = name
			}
			if cmd.Flags().Changed("description") {
				patch["description"] = pdesc
			}
			if cmd.Flags().Changed("plan-gate") {
				patch["plan_gate"] = gate == "on" || gate == "true" || gate == "1"
			}
			return call(a, "PATCH", "/api/projects/"+esc(args[0]), patch, func(p *store.Project) {
				fmt.Fprintf(a.out, "updated %s (plan gate: %v)\n", p.Key, p.PlanGate)
			})
		},
	}
	set.Flags().StringVar(&name, "name", "", "name")
	set.Flags().StringVarP(&pdesc, "description", "d", "", "description")
	set.Flags().StringVar(&gate, "plan-gate", "", "on|off: require an approved plan before items can start")
	cmd.AddCommand(set)
	cmd.AddCommand(a.deleteCmd("delete KEY", "Delete a project and everything in it", func(ref string) string { return "/api/projects/" + esc(ref) }))
	return cmd
}

func (a *app) deleteCmd(use, short string, path func(string) string) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use: use, Short: short, Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if !yes {
				return &exitErr{ExitUsage, "error: refusing to delete without --yes"}
			}
			if err := a.client().Do("DELETE", path(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "deleted %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm deletion")
	return cmd
}

// ---- epics ----

func (a *app) epicCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "epic", Aliases: []string{"epics"}, Short: "Manage epics (groups of items)"}
	var all bool
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List epics of a project", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := a.requireProject()
			if err != nil {
				return err
			}
			all_ := ""
			if all {
				all_ = "1"
			}
			return call(a, "GET", "/api/projects/"+esc(p)+"/epics"+q("all", all_), nil, func(es []*store.Epic) {
				if len(es) == 0 {
					fmt.Fprintln(a.out, "(no epics)")
					return
				}
				w := tw(a.out)
				fmt.Fprintln(w, "REF\tSTATUS\tDONE\tTITLE")
				for _, e := range es {
					fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\n", e.Ref, e.Status, e.Done, e.Total, e.Title)
				}
				w.Flush()
			})
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include closed epics")
	cmd.AddCommand(list)

	var body, file string
	create := &cobra.Command{
		Use: "create TITLE", Short: "Create an epic", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := a.requireProject()
			if err != nil {
				return err
			}
			text, _, err := readText(body, file)
			if err != nil {
				return err
			}
			return call(a, "POST", "/api/projects/"+esc(p)+"/epics", map[string]string{"title": args[0], "body": text}, func(e *store.Epic) {
				fmt.Fprintf(a.out, "created %s %s\n", e.Ref, e.Title)
			})
		},
	}
	create.Flags().StringVar(&body, "body", "", "markdown description")
	create.Flags().StringVarP(&file, "file", "f", "", "read description from file (- for stdin)")
	cmd.AddCommand(create)

	cmd.AddCommand(&cobra.Command{
		Use: "show REF", Short: "Show an epic with its items", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "GET", "/api/epics/"+esc(args[0]), nil, a.printEpic)
		},
	})
	var title, ubody, ufile, status string
	update := &cobra.Command{
		Use: "update REF", Short: "Update an epic", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			patch := map[string]any{}
			if cmd.Flags().Changed("title") {
				patch["title"] = title
			}
			if cmd.Flags().Changed("status") {
				patch["status"] = status
			}
			if text, ok, err := readText(ubody, ufile); err != nil {
				return err
			} else if ok {
				patch["body"] = text
			}
			return call(a, "PATCH", "/api/epics/"+esc(args[0]), patch, func(e *store.Epic) {
				fmt.Fprintf(a.out, "updated %s (%s)\n", e.Ref, e.Status)
			})
		},
	}
	update.Flags().StringVar(&title, "title", "", "title")
	update.Flags().StringVar(&status, "status", "", "open|done|cancelled")
	update.Flags().StringVar(&ubody, "body", "", "markdown description")
	update.Flags().StringVarP(&ufile, "file", "f", "", "read description from file (- for stdin)")
	cmd.AddCommand(update)
	cmd.AddCommand(a.deleteCmd("delete REF", "Delete an epic (items are kept)", func(r string) string { return "/api/epics/" + esc(r) }))
	return cmd
}

// ---- items ----

type itemFlags struct {
	typ, priority, epic, queue, body, file, branch, assignee string
	labels, blockedBy                                        []string
	top                                                      bool
}

func (a *app) itemCreateCmd(use string) *cobra.Command {
	var f itemFlags
	cmd := &cobra.Command{
		Use: use + " TITLE", Short: "Create an item (story, bug, security, research, chore)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := a.requireProject()
			if err != nil {
				return err
			}
			text, _, err := readText(f.body, f.file)
			if err != nil {
				return err
			}
			in := store.ItemInput{Title: args[0], Body: text, Type: f.typ, Priority: f.priority, Epic: f.epic, Queue: f.queue, QueueTop: f.top,
				Labels: f.labels, BlockedBy: f.blockedBy, Branch: f.branch, Assignee: f.assignee}
			return call(a, "POST", "/api/projects/"+esc(p)+"/items", in, func(it *store.Item) {
				where := "triage"
				if it.Queue != "" {
					where = "queue " + it.Queue
				}
				fmt.Fprintf(a.out, "created %s [%s] %s (%s)\n", it.Ref, it.Type, it.Title, where)
			})
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&f.typ, "type", "t", "story", "story|bug|security|research|chore")
	fl.StringVar(&f.priority, "priority", "medium", "urgent|high|medium|low")
	fl.StringVarP(&f.epic, "epic", "e", "", "epic ref (API-E1)")
	fl.StringVarP(&f.queue, "queue", "q", "", "append to this queue")
	fl.BoolVar(&f.top, "top", false, "put at the top of the queue instead of the bottom")
	fl.StringSliceVarP(&f.labels, "label", "l", nil, "labels (repeatable or comma separated)")
	fl.StringSliceVar(&f.blockedBy, "blocked-by", nil, "refs this item depends on")
	fl.StringVar(&f.body, "body", "", "markdown description")
	fl.StringVarP(&f.file, "file", "f", "", "read description from file (- for stdin)")
	fl.StringVar(&f.branch, "branch", "", "branch name (share one branch between items)")
	fl.StringVar(&f.assignee, "assignee", "", "assignee")
	return cmd
}

func (a *app) itemListCmd(use string) *cobra.Command {
	var status, typ, epic, queue, label, query, assignee string
	var limit int
	cmd := &cobra.Command{
		Use: use, Short: "List items (default: open items)", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := a.requireProject()
			if err != nil {
				return err
			}
			path := "/api/projects/" + esc(p) + "/items" + q("status", status, "type", typ, "epic", epic, "queue", queue, "label", label, "q", query, "assignee", assignee, "limit", fmt.Sprint(limit))
			return call(a, "GET", path, nil, a.printItems)
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&status, "status", "s", "open", "open|closed|triage|queued|active|review|done|cancelled (comma separated)")
	fl.StringVarP(&typ, "type", "t", "", "type")
	fl.StringVarP(&epic, "epic", "e", "", "epic ref")
	fl.StringVarP(&queue, "queue", "q", "", "queue name, or 'none'")
	fl.StringVarP(&label, "label", "l", "", "label")
	fl.StringVar(&query, "search", "", "text search")
	fl.StringVar(&assignee, "assignee", "", "assignee")
	fl.IntVar(&limit, "limit", 200, "max rows")
	return cmd
}

func (a *app) itemCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "item", Aliases: []string{"items", "story", "issue"}, Short: "Manage items"}
	cmd.AddCommand(a.itemListCmd("list"), a.itemCreateCmd("create"), a.bulkCmd(), a.itemUpdateCmd())
	cmd.AddCommand(&cobra.Command{
		Use: "show REF", Short: "Show an item", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "GET", "/api/items/"+esc(args[0]), nil, a.printItem)
		},
	})
	cmd.AddCommand(a.deleteCmd("delete REF", "Delete an item", func(r string) string { return "/api/items/" + esc(r) }))
	return cmd
}

func (a *app) addAlias() *cobra.Command {
	c := a.itemCreateCmd("add")
	c.Short = "Create an item (alias of item create)"
	return c
}

func (a *app) lsAlias() *cobra.Command {
	c := a.itemListCmd("ls")
	c.Short = "List items (alias of item list)"
	return c
}

func (a *app) itemUpdateCmd() *cobra.Command {
	var title, body, file, typ, status, prio, epic, queue, assignee, branch, pr string
	var addL, rmL []string
	var ifVersion int
	cmd := &cobra.Command{
		Use: "update REF", Short: "Update item fields (only flags you pass change)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			patch := map[string]any{}
			set := func(flag, key, v string) {
				if cmd.Flags().Changed(flag) {
					patch[key] = v
				}
			}
			set("title", "title", title)
			set("type", "type", typ)
			set("status", "status", status)
			set("priority", "priority", prio)
			set("epic", "epic", epic)
			set("queue", "queue", queue)
			set("assignee", "assignee", assignee)
			set("branch", "branch", branch)
			set("pr", "pr_url", pr)
			if text, ok, err := readText(body, file); err != nil {
				return err
			} else if ok {
				patch["body"] = text
			}
			if len(addL) > 0 {
				patch["add_labels"] = addL
			}
			if len(rmL) > 0 {
				patch["remove_labels"] = rmL
			}
			if ifVersion > 0 {
				patch["if_version"] = ifVersion
			}
			return call(a, "PATCH", "/api/items/"+esc(args[0]), patch, func(it *store.Item) {
				fmt.Fprintf(a.out, "updated %s (%s, v%d)\n", it.Ref, it.Status, it.Version)
			})
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&title, "title", "", "title")
	fl.StringVar(&body, "body", "", "markdown description")
	fl.StringVarP(&file, "file", "f", "", "read description from file (- for stdin)")
	fl.StringVarP(&typ, "type", "t", "", "type")
	fl.StringVarP(&status, "status", "s", "", "triage|queued|active|review|done|cancelled")
	fl.StringVar(&prio, "priority", "", "urgent|high|medium|low")
	fl.StringVarP(&epic, "epic", "e", "", "epic ref ('' to clear)")
	fl.StringVarP(&queue, "queue", "q", "", "move to queue ('' = back to inbox)")
	fl.StringVar(&assignee, "assignee", "", "assignee")
	fl.StringVar(&branch, "branch", "", "branch name")
	fl.StringVar(&pr, "pr", "", "pull request URL")
	fl.StringSliceVar(&addL, "add-label", nil, "add labels")
	fl.StringSliceVar(&rmL, "rm-label", nil, "remove labels")
	fl.IntVar(&ifVersion, "if-version", 0, "fail with exit 4 if the item changed since this version")
	return cmd
}

func (a *app) bulkCmd() *cobra.Command {
	var epic, queue, doc, branch string
	cmd := &cobra.Command{
		Use:   "bulk FILE",
		Short: "Create many items at once from YAML/JSON (all or nothing)",
		Long: `Create many items in one transaction. FILE is YAML or JSON ("-" for stdin):

  epic: API-E2          # optional defaults for every item
  queue: auth-lane
  doc: API-D9           # link each item to this doc (role "found": review findings)
  branch: fix/sec-batch # optional shared branch (a batch of small fixes)
  labels: [security]
  items:
    - key: a            # local key, usable in blocked_by below
      title: Rate-limit login
      type: security
      priority: high
      body: |
        markdown...
    - title: Add lockout alert
      blocked_by: [a, API-12]

A plain list of items is accepted too.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := a.requireProject()
			if err != nil {
				return err
			}
			var data []byte
			if args[0] == "-" {
				data, err = readAllStdin()
			} else {
				data, err = os.ReadFile(args[0])
			}
			if err != nil {
				return err
			}
			var raw any
			if err := yaml.Unmarshal(data, &raw); err != nil {
				return fmt.Errorf("parse %s: %w", args[0], err)
			}
			if list, ok := raw.([]any); ok {
				raw = map[string]any{"items": list}
			}
			m, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: expected a mapping with items or a list", args[0])
			}
			for k, v := range map[string]string{"epic": epic, "queue": queue, "doc": doc, "branch": branch} {
				if v != "" {
					m[k] = v
				}
			}
			b, err := json.Marshal(m)
			if err != nil {
				return err
			}
			return call(a, "POST", "/api/projects/"+esc(p)+"/items/bulk", json.RawMessage(b), func(items []*store.Item) {
				fmt.Fprintf(a.out, "created %d items\n", len(items))
				a.printItems(items)
			})
		},
	}
	cmd.Flags().StringVarP(&epic, "epic", "e", "", "default epic")
	cmd.Flags().StringVarP(&queue, "queue", "q", "", "default queue")
	cmd.Flags().StringVar(&doc, "doc", "", "link every item to this document")
	cmd.Flags().StringVar(&branch, "branch", "", "shared branch for every item")
	return cmd
}

func readAllStdin() ([]byte, error) {
	var b strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := os.Stdin.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return []byte(b.String()), nil
}

// ---- generic ----

func (a *app) showCmd() *cobra.Command {
	return &cobra.Command{
		Use: "show REF", Short: "Show any item (API-12), epic (API-E2) or document (API-D3)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "GET", "/api/refs/"+esc(args[0]), nil, func(v struct {
				Kind string      `json:"kind"`
				Item *store.Item `json:"item"`
				Epic *store.Epic `json:"epic"`
				Doc  *store.Doc  `json:"doc"`
			}) {
				switch v.Kind {
				case "epic":
					a.printEpic(v.Epic)
				case "doc":
					a.printDoc(v.Doc, false)
				default:
					a.printItem(v.Item)
				}
			})
		},
	}
}

func (a *app) nextQuery(queue string) (string, error) {
	agent := a.actor
	if queue != "" || os.Getenv("TASKER_QUEUE") != "" {
		qr, err := a.queueArg(queue)
		if err != nil {
			return "", err
		}
		return "/api/next" + q("queue", qr, "agent", agent), nil
	}
	if a.project != "" {
		return "/api/next" + q("project", a.project, "agent", agent), nil
	}
	cwd, _ := os.Getwd()
	return "/api/next" + q("path", cwd, "agent", agent), nil
}

func (a *app) nextCmd() *cobra.Command {
	var queue string
	cmd := &cobra.Command{
		Use:   "next",
		Short: "Show the next item to work on (queue order in a bound worktree, else project priority)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			path, err := a.nextQuery(queue)
			if err != nil {
				return err
			}
			return call(a, "GET", path, nil, a.printNext)
		},
	}
	cmd.Flags().StringVarP(&queue, "queue", "q", "", "queue (default: TASKER_QUEUE or the queue bound to this worktree)")
	return cmd
}

func (a *app) startCmd() *cobra.Command {
	var branch, queue string
	var force bool
	var minutes int
	cmd := &cobra.Command{
		Use:   "start [REF]",
		Short: "Claim an item, mark it active and record its branch (default: the next item)",
		Long:  "Marks the item active, claims it for you (--as) and records its branch.\ntasker never runs git: create/switch the branch yourself (the command prints how).",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := a.client()
			ref := ""
			base := ""
			if len(args) == 1 {
				ref = args[0]
			} else {
				path, err := a.nextQuery(queue)
				if err != nil {
					return err
				}
				var n store.NextResult
				if err := c.Do("GET", path, nil, &n); err != nil {
					return err
				}
				if n.Item == nil {
					return &exitErr{ExitNotFound, n.Message}
				}
				ref, base = n.Item.Ref, n.BaseBranch
			}
			data, err := c.raw("POST", "/api/items/"+esc(ref)+"/start", store.StartInput{Branch: branch, Force: force, Minutes: minutes})
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(data)
			}
			var it store.Item
			json.Unmarshal(data, &it)
			if base == "" && it.Queue != "" {
				var n store.NextResult
				if c.Do("GET", "/api/next"+q("queue", it.Project+"/"+it.Queue, "agent", a.actor), nil, &n) == nil && n.Item != nil && n.Item.Ref == it.Ref {
					base = n.BaseBranch
				}
			}
			fmt.Fprintf(a.out, "started %s %s\nbranch: %s\n", it.Ref, it.Title, it.Branch)
			if base != "" {
				fmt.Fprintf(a.out, "git:    git switch -c %s %s   (or git switch %s if it exists)\n", it.Branch, base, it.Branch)
			}
			fmt.Fprintf(a.out, "done?   tasker finish %s --pr URL\n", it.Ref)
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "branch name (default: existing or generated from the title)")
	cmd.Flags().StringVarP(&queue, "queue", "q", "", "queue to take the next item from")
	cmd.Flags().BoolVar(&force, "force", false, "start even if blocked, claimed or missing an approved plan")
	cmd.Flags().IntVar(&minutes, "lease", 240, "claim lease in minutes")
	return cmd
}

func (a *app) finishCmd() *cobra.Command {
	var pr, msg string
	var done bool
	cmd := &cobra.Command{
		Use:   "finish [REF]",
		Short: "Finish an item: status review (PR open) or --done; releases the claim",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := a.client()
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			} else {
				path, err := a.nextQuery("")
				if err != nil {
					return err
				}
				var n store.NextResult
				if err := c.Do("GET", path, nil, &n); err != nil {
					return err
				}
				if n.Item == nil || !n.Resume {
					return &exitErr{ExitUsage, "error: no active item of yours here; pass REF"}
				}
				ref = n.Item.Ref
			}
			status := "review"
			if done {
				status = "done"
			}
			return call(a, "POST", "/api/items/"+esc(ref)+"/finish", store.FinishInput{PRURL: pr, Status: status, Comment: msg}, func(it *store.Item) {
				fmt.Fprintf(a.out, "%s → %s\n", it.Ref, it.Status)
			})
		},
	}
	cmd.Flags().StringVar(&pr, "pr", "", "pull request URL")
	cmd.Flags().StringVarP(&msg, "message", "m", "", "summary comment")
	cmd.Flags().BoolVar(&done, "done", false, "mark done instead of review")
	return cmd
}

func (a *app) claimCmd() *cobra.Command {
	var minutes int
	cmd := &cobra.Command{
		Use: "claim REF", Short: "Claim an item without starting it", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "POST", "/api/items/"+esc(args[0])+"/claim", map[string]any{"lease_minutes": minutes}, func(it *store.Item) {
				fmt.Fprintf(a.out, "%s claimed by %s until %s\n", it.Ref, it.ClaimedBy, when(it.ClaimExpires))
			})
		},
	}
	cmd.Flags().IntVar(&minutes, "lease", 240, "lease in minutes")
	return cmd
}

func (a *app) releaseCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use: "release REF", Short: "Release your claim on an item", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "POST", "/api/items/"+esc(args[0])+"/release", map[string]any{"force": force}, func(it *store.Item) {
				fmt.Fprintf(a.out, "%s released\n", it.Ref)
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "release someone else's claim")
	return cmd
}

func (a *app) linkCmd(remove bool) *cobra.Command {
	var blockedBy, blocks, relates []string
	use, short, method := "link REF", "Add dependencies: --blocked-by / --blocks / --relates", "POST"
	if remove {
		use, short, method = "unlink REF", "Remove dependencies", "DELETE"
	}
	cmd := &cobra.Command{
		Use: use, Short: short, Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := a.client()
			n := 0
			for kind, targets := range map[string][]string{"blocked_by": blockedBy, "blocks": blocks, "relates": relates} {
				for _, t := range targets {
					if err := c.Do(method, "/api/items/"+esc(args[0])+"/links", map[string]string{"kind": kind, "target": t}, nil); err != nil {
						return err
					}
					n++
				}
			}
			if n == 0 {
				return &exitErr{ExitUsage, "error: give --blocked-by, --blocks or --relates"}
			}
			return call(a, "GET", "/api/items/"+esc(args[0]), nil, func(it *store.Item) {
				fmt.Fprintf(a.out, "%s: blocked by %d, blocks %d, related %d\n", it.Ref, len(it.BlockedBy), len(it.Blocks), len(it.Related))
			})
		},
	}
	cmd.Flags().StringSliceVar(&blockedBy, "blocked-by", nil, "items this one depends on")
	cmd.Flags().StringSliceVar(&blocks, "blocks", nil, "items that depend on this one")
	cmd.Flags().StringSliceVar(&relates, "relates", nil, "related items")
	return cmd
}

func (a *app) commentCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use: "comment REF [TEXT]", Short: "Comment on an item, epic or document (-f file, - for stdin)", Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			body := ""
			if len(args) == 2 {
				body = args[1]
			}
			text, ok, err := readText(body, file)
			if err != nil {
				return err
			}
			if !ok {
				return &exitErr{ExitUsage, "error: give TEXT or -f FILE"}
			}
			return call(a, "POST", "/api/comments/"+esc(args[0]), map[string]string{"body": text}, func(c *store.Comment) {
				fmt.Fprintf(a.out, "commented on %s\n", strings.ToUpper(args[0]))
			})
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "read comment from file (- for stdin)")
	return cmd
}

func (a *app) searchCmd() *cobra.Command {
	return &cobra.Command{
		Use: "search TEXT", Short: "Search items and documents", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "GET", "/api/search"+q("q", args[0], "project", a.project), nil, func(v struct {
				Items []*store.Item `json:"items"`
				Docs  []*store.Doc  `json:"docs"`
			}) {
				a.printItems(v.Items)
				fmt.Fprintln(a.out)
				a.printDocs(v.Docs)
			})
		},
	}
}

func (a *app) activityCmd() *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use: "activity [REF]", Short: "Show recent activity (for a project or a ref)", Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			return call(a, "GET", "/api/activity"+q("project", a.project, "ref", ref, "limit", fmt.Sprint(n)), nil, a.printActivity)
		},
	}
	cmd.Flags().IntVarP(&n, "limit", "n", 30, "entries")
	return cmd
}
