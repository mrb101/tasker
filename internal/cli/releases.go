package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrb101/tasker/internal/store"
)

func (a *app) releaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "release",
		Aliases: []string{"releases", "rel"},
		Short:   "Releases: a goal, a date or a scope, and the stories that make it up",
	}

	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List releases", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := a.requireProject()
			if err != nil {
				return err
			}
			return call(a, "GET", "/api/projects/"+esc(p)+"/releases", nil, func(rs []*store.Release) {
				if len(rs) == 0 {
					fmt.Fprintln(a.out, "(no releases) plan one: tasker release create \"1.0\" --goal \"...\"")
					return
				}
				w := tw(a.out)
				fmt.Fprintln(w, "REF\tSTATUS\tDONE\tSTART\tTARGET\tNAME")
				for _, r := range rs {
					fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\t%s\t%s\n", r.Ref, r.Status, r.Done, r.Total, dash(r.StartsOn), dash(r.TargetOn), r.Name)
				}
				w.Flush()
			})
		},
	})

	var in store.ReleaseInput
	create := &cobra.Command{
		Use:   "create [NAME]",
		Short: "Plan a release; dates follow the project's rhythm when left out",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := a.requireProject()
			if err != nil {
				return err
			}
			in.Name = first(args)
			return call(a, "POST", "/api/projects/"+esc(p)+"/releases", in, func(r *store.Release) {
				fmt.Fprintf(a.out, "created %s %s (%s → %s)\n", r.Ref, r.Name, dash(r.StartsOn), dash(r.TargetOn))
			})
		},
	}
	create.Flags().StringVar(&in.Goal, "goal", "", "what someone can do once it ships")
	create.Flags().StringVar(&in.StartsOn, "start", "", "start date, YYYY-MM-DD")
	create.Flags().StringVar(&in.TargetOn, "target", "", "target date, YYYY-MM-DD")
	cmd.AddCommand(create)

	cmd.AddCommand(&cobra.Command{
		Use: "show REF", Short: "Show a release: goal, forecast, risks and stories", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "GET", "/api/releases/"+esc(args[0]), nil, a.printRelease)
		},
	})

	var up struct{ name, goal, start, target, tag string }
	update := &cobra.Command{
		Use: "update REF", Short: "Change a release's name, goal, dates or tag", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			patch := map[string]any{}
			for flag, key := range map[string]string{"name": "name", "goal": "goal", "start": "starts_on", "target": "target_on", "tag": "tag"} {
				if c.Flags().Changed(flag) {
					v, _ := c.Flags().GetString(flag)
					patch[key] = v
				}
			}
			return call(a, "PATCH", "/api/releases/"+esc(args[0]), patch, func(r *store.Release) {
				fmt.Fprintf(a.out, "updated %s\n", r.Ref)
			})
		},
	}
	update.Flags().StringVar(&up.name, "name", "", "name or version")
	update.Flags().StringVar(&up.goal, "goal", "", "goal")
	update.Flags().StringVar(&up.start, "start", "", "start date")
	update.Flags().StringVar(&up.target, "target", "", "target date")
	update.Flags().StringVar(&up.tag, "tag", "", "git tag")
	cmd.AddCommand(update)

	cmd.AddCommand(&cobra.Command{
		Use: "start REF", Short: "Mark a release as in progress (one at a time)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "PATCH", "/api/releases/"+esc(args[0]), map[string]any{"status": "active"}, func(r *store.Release) {
				fmt.Fprintf(a.out, "%s is in progress\n", r.Ref)
			})
		},
	})

	var tag, moveTo string
	ship := &cobra.Command{
		Use:   "ship REF",
		Short: "Ship a release; unfinished stories move to the next planned release",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "POST", "/api/releases/"+esc(args[0])+"/ship", store.ShipInput{Tag: tag, MoveTo: moveTo}, func(r *store.Release) {
				fmt.Fprintf(a.out, "shipped %s %s (%d stories)\nnotes: tasker release notes %s\n", r.Ref, r.Name, r.Done, r.Ref)
			})
		},
	}
	ship.Flags().StringVar(&tag, "tag", "", "git tag of what shipped, e.g. v1.4.0")
	ship.Flags().StringVar(&moveTo, "move-to", "", "where unfinished stories go: a release ref, or none (default: the next planned release)")
	cmd.AddCommand(ship)

	cmd.AddCommand(&cobra.Command{
		Use: "notes REF", Short: "Release notes in markdown, grouped by epic", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			data, err := a.client().raw("GET", "/api/releases/"+esc(args[0])+"/notes", nil)
			if err != nil {
				return err
			}
			fmt.Fprint(a.out, string(data))
			return nil
		},
	})

	var stretch bool
	add := &cobra.Command{
		Use: "add REF ITEM...", Short: "Put stories into a release (--stretch: not a commitment)", Args: cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			r, err := store.ParseRef(args[0])
			if err != nil {
				return err
			}
			patch := map[string]any{"release": args[0], "stretch": stretch}
			return call(a, "POST", "/api/projects/"+esc(r.Project)+"/items/batch", map[string]any{"refs": args[1:], "patch": patch}, func(items []*store.Item) {
				fmt.Fprintf(a.out, "%d stories in %s\n", len(items), strings.ToUpper(args[0]))
			})
		},
	}
	add.Flags().BoolVar(&stretch, "stretch", false, "add as stretch rather than commitment")
	cmd.AddCommand(add)

	var epicStretch bool
	addEpic := &cobra.Command{
		Use: "add-epic REF EPIC", Short: "Put an epic's open stories into a release", Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "POST", "/api/releases/"+esc(args[0])+"/epics", map[string]any{"epic": args[1], "stretch": epicStretch}, func(v struct {
				Added   int      `json:"added"`
				Skipped []string `json:"skipped"`
			}) {
				fmt.Fprintf(a.out, "added %d stories of %s\n", v.Added, strings.ToUpper(args[1]))
				if len(v.Skipped) > 0 {
					fmt.Fprintf(a.out, "left where they are: %s\n", strings.Join(v.Skipped, ", "))
				}
			})
		},
	}
	addEpic.Flags().BoolVar(&epicStretch, "stretch", false, "add as stretch rather than commitment")
	cmd.AddCommand(addEpic)

	cmd.AddCommand(&cobra.Command{
		Use: "remove ITEM...", Short: "Take stories out of their release", Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			r, err := store.ParseRef(args[0])
			if err != nil {
				return err
			}
			return call(a, "POST", "/api/projects/"+esc(r.Project)+"/items/batch", map[string]any{"refs": args, "patch": map[string]any{"release": ""}}, func(items []*store.Item) {
				fmt.Fprintf(a.out, "%d stories out of their release\n", len(items))
			})
		},
	})
	cmd.AddCommand(a.deleteCmd("delete REF", "Delete a release (its stories stay, outside any release)", func(r string) string { return "/api/releases/" + esc(r) }))
	return cmd
}

func (a *app) printRelease(r *store.Release) {
	o := a.out
	fmt.Fprintf(o, "%s  %s  [%s]\n", r.Ref, r.Name, r.Status)
	if r.Goal != "" {
		fmt.Fprintf(o, "goal: %s\n", r.Goal)
	}
	fmt.Fprintf(o, "dates: %s → %s", dash(r.StartsOn), dash(r.TargetOn))
	if r.ShippedAt != "" {
		fmt.Fprintf(o, " · shipped %s", r.ShippedAt[:10])
	}
	if r.Tag != "" {
		fmt.Fprintf(o, " · tag %s", r.Tag)
	}
	fmt.Fprintf(o, "\nstories: %d done of %d · open: %d committed, %d stretch\n", r.Done, r.Total, r.Committed, r.Stretch)
	if f := r.Forecast; f != nil {
		switch {
		case !f.Enough:
			fmt.Fprintln(o, "forecast: not enough finished work yet to forecast from")
		case f.Date50 != "":
			fmt.Fprintf(o, "forecast: the %d open stories are done by %s (50%%) or %s (85%%)\n", f.Remaining, f.Date50, f.Date85)
		default:
			fmt.Fprintf(o, "forecast: by the target, %d more stories in 85%% of runs, %d in 50%% (%d open)\n", f.Likely85, f.Likely50, f.Remaining)
		}
	}
	for _, rk := range r.Risks {
		fmt.Fprintf(o, "risk: %s %s — %s\n", rk.Ref, trunc(rk.Title, 50), rk.Reason)
	}
	fmt.Fprintln(o)
	w := tw(o)
	fmt.Fprintln(w, "REF\tSTATUS\tPLAN\tOUTLOOK\tTITLE")
	for _, it := range r.Items {
		plan := "committed"
		if it.Stretch {
			plan = "stretch"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", it.Ref, shown(it.Status), plan, dash(r.Outlook[it.Ref]), trunc(it.Title, 60))
	}
	w.Flush()
}
