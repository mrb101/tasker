package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrb101/tasker/internal/store"
)

func (a *app) queueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "queue",
		Aliases: []string{"queues", "q"},
		Short:   "Queues: one per worktree; items run in order, one branch each",
	}

	var all bool
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List queues", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			a_ := ""
			if all {
				a_ = "1"
			}
			return call(a, "GET", "/api/queues"+q("project", a.project, "all", a_), nil, a.printQueues)
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include archived queues")
	cmd.AddCommand(list)

	var in store.QueueInput
	var here bool
	create := &cobra.Command{
		Use: "create NAME", Short: "Create a queue (lowercase name, e.g. auth-lane)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := a.requireProject()
			if err != nil {
				return err
			}
			in.Name = args[0]
			if here {
				in.Worktree = worktreeRoot()
			}
			return call(a, "POST", "/api/projects/"+esc(p)+"/queues", in, func(qu *store.Queue) {
				fmt.Fprintf(a.out, "created queue %s (base %s)\n", qu.Ref, qu.BaseBranch)
				if qu.Worktree != "" {
					fmt.Fprintf(a.out, "bound to %s\n", qu.Worktree)
				}
			})
		},
	}
	create.Flags().StringVar(&in.BaseBranch, "base", "main", "branch stories branch from")
	create.Flags().BoolVar(&in.Stacked, "stacked", false, "each story branches from the previous story's branch")
	create.Flags().StringVar(&in.Worktree, "worktree", "", "worktree path")
	create.Flags().BoolVar(&here, "here", false, "bind to the current git worktree")
	create.Flags().StringVar(&in.Agent, "agent", "", "agent working this queue")
	create.Flags().StringVarP(&in.Description, "description", "d", "", "description")
	cmd.AddCommand(create)

	cmd.AddCommand(&cobra.Command{
		Use: "show [QUEUE]", Short: "Show a queue in order (default: this worktree's queue)", Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ref, err := a.resolveQueueRef(first(args))
			if err != nil {
				return err
			}
			path, err := queuePath(ref)
			if err != nil {
				return err
			}
			return call(a, "GET", path, nil, a.printQueue)
		},
	})

	var up struct {
		name, desc, base, worktree, agent, status string
		stacked                                   bool
	}
	update := &cobra.Command{
		Use: "update QUEUE", Short: "Change queue settings (--status paused|active|archived)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := a.resolveQueueRef(args[0])
			if err != nil {
				return err
			}
			path, err := queuePath(ref)
			if err != nil {
				return err
			}
			patch := map[string]any{}
			for flag, pair := range map[string]struct {
				key string
				val any
			}{"name": {"name", up.name}, "description": {"description", up.desc}, "base": {"base_branch", up.base},
				"worktree": {"worktree", up.worktree}, "agent": {"agent", up.agent}, "status": {"status", up.status}, "stacked": {"stacked", up.stacked}} {
				if cmd.Flags().Changed(flag) {
					patch[pair.key] = pair.val
				}
			}
			return call(a, "PATCH", path, patch, func(qu *store.Queue) { fmt.Fprintf(a.out, "updated queue %s (%s)\n", qu.Ref, qu.Status) })
		},
	}
	update.Flags().StringVar(&up.name, "name", "", "rename")
	update.Flags().StringVarP(&up.desc, "description", "d", "", "description")
	update.Flags().StringVar(&up.base, "base", "", "base branch")
	update.Flags().StringVar(&up.worktree, "worktree", "", "worktree path")
	update.Flags().StringVar(&up.agent, "agent", "", "agent")
	update.Flags().StringVar(&up.status, "status", "", "active|paused|archived")
	update.Flags().BoolVar(&up.stacked, "stacked", false, "stacked branches")
	cmd.AddCommand(update)

	var yes bool
	del := &cobra.Command{
		Use: "delete QUEUE", Short: "Delete a queue; waiting items go back to triage", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if !yes {
				return &exitErr{ExitUsage, "error: refusing to delete without --yes"}
			}
			ref, err := a.resolveQueueRef(args[0])
			if err != nil {
				return err
			}
			path, err := queuePath(ref)
			if err != nil {
				return err
			}
			if err := a.client().Do("DELETE", path, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "deleted queue %s\n", ref)
			return nil
		},
	}
	del.Flags().BoolVar(&yes, "yes", false, "confirm")
	cmd.AddCommand(del)

	var top bool
	var before, after string
	add := &cobra.Command{
		Use:   "add QUEUE REF...",
		Short: "Put items into a queue in order (moves them from other queues)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.queueAdd(args[0], args[1:], top, before, after)
		},
	}
	add.Flags().BoolVar(&top, "top", false, "insert at the top")
	add.Flags().StringVar(&before, "before", "", "insert before this item")
	add.Flags().StringVar(&after, "after", "", "insert after this item")
	cmd.AddCommand(add)

	var to string
	var mtop bool
	var mbefore, mafter string
	move := &cobra.Command{
		Use:   "move REF... [--to QUEUE] [--top|--before REF|--after REF]",
		Short: "Reorder items, or move them to another queue",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			qname := to
			if qname == "" {
				var it store.Item
				if err := a.client().Do("GET", "/api/items/"+esc(args[0]), nil, &it); err != nil {
					return err
				}
				if it.Queue == "" {
					return &exitErr{ExitUsage, "error: " + it.Ref + " is not in a queue; use --to QUEUE"}
				}
				qname = it.Project + "/" + it.Queue
			}
			return a.queueAdd(qname, args, mtop, mbefore, mafter)
		},
	}
	move.Flags().StringVar(&to, "to", "", "target queue (default: the item's current queue)")
	move.Flags().BoolVar(&mtop, "top", false, "move to the top")
	move.Flags().StringVar(&mbefore, "before", "", "place before this item")
	move.Flags().StringVar(&mafter, "after", "", "place after this item")
	cmd.AddCommand(move)

	cmd.AddCommand(&cobra.Command{
		Use: "remove REF...", Short: "Take items out of their queue (back to triage)", Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			for _, r := range args {
				if err := a.client().Do("DELETE", "/api/queued/"+esc(r), nil, nil); err != nil {
					return err
				}
				fmt.Fprintf(a.out, "%s → triage\n", strings.ToUpper(r))
			}
			return nil
		},
	})

	var path string
	bind := &cobra.Command{
		Use:   "bind QUEUE",
		Short: "Bind a queue to this worktree so `tasker next/start/finish` pick it up here",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ref, err := a.resolveQueueRef(args[0])
			if err != nil {
				return err
			}
			qp, err := queuePath(ref)
			if err != nil {
				return err
			}
			if path == "" {
				path = worktreeRoot()
			}
			return call(a, "PATCH", qp, map[string]any{"worktree": path}, func(qu *store.Queue) {
				fmt.Fprintf(a.out, "queue %s bound to %s\n", qu.Ref, qu.Worktree)
			})
		},
	}
	bind.Flags().StringVar(&path, "path", "", "worktree path (default: git toplevel of the current directory)")
	cmd.AddCommand(bind)

	cmd.AddCommand(&cobra.Command{
		Use: "unbind QUEUE", Short: "Forget a queue's worktree path", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ref, err := a.resolveQueueRef(args[0])
			if err != nil {
				return err
			}
			qp, err := queuePath(ref)
			if err != nil {
				return err
			}
			return call(a, "PATCH", qp, map[string]any{"worktree": ""}, func(qu *store.Queue) {
				fmt.Fprintf(a.out, "queue %s unbound\n", qu.Ref)
			})
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use: "current", Short: "Show which queue this directory is bound to", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			cwd, _ := os.Getwd()
			return call(a, "GET", "/api/queues"+q("path", cwd), nil, func(qu *store.Queue) {
				fmt.Fprintf(a.out, "%s (worktree %s)\n", qu.Ref, qu.Worktree)
			})
		},
	})
	return cmd
}

func first(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func (a *app) queueAdd(queue string, refs []string, top bool, before, after string) error {
	ref, err := a.resolveQueueRef(queue)
	if err != nil {
		return err
	}
	path, err := queuePath(ref)
	if err != nil {
		return err
	}
	pos := store.Position{Where: store.PosBottom}
	switch {
	case top:
		pos.Where = store.PosTop
	case before != "":
		pos = store.Position{Where: store.PosBefore, Anchor: before}
	case after != "":
		pos = store.Position{Where: store.PosAfter, Anchor: after}
	}
	return call(a, "POST", path+"/items", map[string]any{"refs": refs, "position": pos}, a.printQueue)
}
