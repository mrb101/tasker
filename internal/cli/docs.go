package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/mrb101/tasker/internal/render"
	"github.com/mrb101/tasker/internal/store"
)

func (a *app) docCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "doc",
		Aliases: []string{"docs", "document"},
		Short:   "Documents: research, plans, reviews, decisions — reviewed by a human",
	}

	var typ, status, forRef, search string
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List documents", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return call(a, "GET", "/api/docs"+q("project", a.project, "type", typ, "status", status, "target", forRef, "q", search), nil, a.printDocs)
		},
	}
	list.Flags().StringVarP(&typ, "type", "t", "", "research|plan|review|decision|note")
	list.Flags().StringVarP(&status, "status", "s", "", "draft|in_review|changes_requested|approved|superseded")
	list.Flags().StringVar(&forRef, "for", "", "documents linked to this item/epic")
	list.Flags().StringVar(&search, "search", "", "text search")
	cmd.AddCommand(list)

	var in store.DocInput
	var body, file string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a document (title defaults to the first # heading)",
		Example: `  tasker doc create --type plan --for API-41,API-42 -f plan.md --submit
  tasker doc create --type research --for API-E2 -f docs/research/auth.md`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := a.requireProject()
			if err != nil {
				return err
			}
			text, _, err := readText(body, file)
			if err != nil {
				return err
			}
			in.Body = stripFrontmatter(text)
			return call(a, "POST", "/api/projects/"+esc(p)+"/docs", in, func(d *store.Doc) {
				fmt.Fprintf(a.out, "created %s [%s] %s (%s)\n", d.Ref, d.Type, d.Title, d.Status)
				if d.Status == "in_review" {
					fmt.Fprintf(a.out, "wait for the review: tasker doc wait %s\n", d.Ref)
				}
			})
		},
	}
	create.Flags().StringVarP(&in.Type, "type", "t", "note", "research|plan|review|decision|note")
	create.Flags().StringVar(&in.Title, "title", "", "title")
	create.Flags().StringSliceVar(&in.For, "for", nil, "link to items/epics (comma separated)")
	create.Flags().StringVar(&in.Role, "role", "", "link role: plans|informs|found (default by type)")
	create.Flags().BoolVar(&in.Submit, "submit", false, "submit for human review")
	create.Flags().StringVar(&body, "body", "", "markdown body")
	create.Flags().StringVarP(&file, "file", "f", "", "read body from file (- for stdin)")
	cmd.AddCommand(create)

	var raw bool
	var rev int
	show := &cobra.Command{
		Use: "show REF", Short: "Show a document with its review thread (--raw: markdown only)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if rev > 0 {
				return call(a, "GET", "/api/docs/"+esc(args[0])+"/revisions/"+fmt.Sprint(rev), nil, func(r *store.Revision) {
					if !raw {
						fmt.Fprintf(a.out, "%s r%d by %s at %s\n\n", strings.ToUpper(args[0]), r.Revision, r.Author, when(r.CreatedAt))
					}
					fmt.Fprintln(a.out, strings.TrimRight(r.Body, "\n"))
				})
			}
			return call(a, "GET", "/api/docs/"+esc(args[0]), nil, func(d *store.Doc) { a.printDoc(d, raw) })
		},
	}
	show.Flags().BoolVar(&raw, "raw", false, "print only the markdown body")
	show.Flags().IntVar(&rev, "rev", 0, "show an older revision")
	cmd.AddCommand(show)

	var ubody, ufile, utitle, utype string
	var usubmit bool
	var ifRev int
	update := &cobra.Command{
		Use:   "update REF",
		Short: "Update a document (new revision; goes back to review once submitted)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			patch := map[string]any{}
			if text, ok, err := readText(ubody, ufile); err != nil {
				return err
			} else if ok {
				patch["body"] = stripFrontmatter(text)
			}
			if cmd.Flags().Changed("title") {
				patch["title"] = utitle
			}
			if cmd.Flags().Changed("type") {
				patch["type"] = utype
			}
			if usubmit {
				patch["submit"] = true
			}
			if ifRev > 0 {
				patch["if_revision"] = ifRev
			}
			return call(a, "PATCH", "/api/docs/"+esc(args[0]), patch, func(d *store.Doc) {
				fmt.Fprintf(a.out, "updated %s → r%d (%s)\n", d.Ref, d.Revision, d.Status)
			})
		},
	}
	update.Flags().StringVar(&ubody, "body", "", "new markdown body")
	update.Flags().StringVarP(&ufile, "file", "f", "", "read body from file (- for stdin)")
	update.Flags().StringVar(&utitle, "title", "", "title")
	update.Flags().StringVarP(&utype, "type", "t", "", "type")
	update.Flags().BoolVar(&usubmit, "submit", false, "submit for review")
	update.Flags().IntVar(&ifRev, "if-revision", 0, "fail with exit 4 if the document changed since this revision")
	cmd.AddCommand(update)

	for _, act := range []struct{ name, short string }{
		{"submit", "Submit for human review"},
		{"approve", "Approve (humans usually do this in the web UI)"},
		{"request-changes", "Request changes (-m required)"},
		{"supersede", "Mark superseded"},
		{"reopen", "Reopen as draft"},
	} {
		var msg string
		c := &cobra.Command{
			Use: act.name + " REF", Short: act.short, Args: cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				return call(a, "POST", "/api/docs/"+esc(args[0])+"/actions/"+strings.ReplaceAll(act.name, "-", "_"), map[string]string{"comment": msg}, func(d *store.Doc) {
					fmt.Fprintf(a.out, "%s → %s\n", d.Ref, d.Status)
				})
			},
		}
		c.Flags().StringVarP(&msg, "message", "m", "", "comment")
		cmd.AddCommand(c)
	}

	var timeout, interval time.Duration
	wait := &cobra.Command{
		Use:   "wait REF",
		Short: "Block until a human approves or requests changes; prints the review",
		Long: `Polls until the document leaves review, then prints the new status and the review comments.
Exit codes: 0 approved · 10 changes requested (revise, then tasker doc update REF -f file) · 11 superseded/draft · 12 timeout.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			deadline := time.Now().Add(timeout)
			for {
				var d store.Doc
				if err := a.client().Do("GET", "/api/docs/"+esc(args[0]), nil, &d); err != nil {
					return err
				}
				if d.Status != "in_review" {
					if a.json {
						a.printJSON(d)
					} else {
						fmt.Fprintf(a.out, "%s is %s (revision %d)\n", d.Ref, d.Status, d.Revision)
						a.printComments(sinceSubmit(d.Comments))
					}
					switch d.Status {
					case "approved":
						return nil
					case "changes_requested":
						return &exitErr{10, ""}
					}
					return &exitErr{11, ""}
				}
				if timeout > 0 && time.Now().After(deadline) {
					return &exitErr{12, fmt.Sprintf("timed out: %s is still in review", d.Ref)}
				}
				time.Sleep(interval)
			}
		},
	}
	wait.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long (0 = wait forever)")
	wait.Flags().DurationVar(&interval, "interval", 5*time.Second, "poll interval")
	cmd.AddCommand(wait)

	var to, role string
	link := &cobra.Command{
		Use: "link REF --to TARGET", Short: "Link a document to an item or epic", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := a.client()
			for _, t := range strings.Split(to, ",") {
				if t = strings.TrimSpace(t); t != "" {
					if err := c.Do("POST", "/api/docs/"+esc(args[0])+"/links", map[string]string{"target": t, "role": role}, nil); err != nil {
						return err
					}
				}
			}
			return call(a, "GET", "/api/docs/"+esc(args[0]), nil, func(d *store.Doc) {
				for _, l := range d.Links {
					fmt.Fprintf(a.out, "%s %s %s\n", d.Ref, l.Role, l.Target)
				}
			})
		},
	}
	link.Flags().StringVar(&to, "to", "", "item/epic refs (comma separated)")
	link.Flags().StringVar(&role, "role", "", "plans|informs|found (default by type)")
	link.MarkFlagRequired("to")
	cmd.AddCommand(link)

	var from string
	unlink := &cobra.Command{
		Use: "unlink REF --from TARGET", Short: "Remove a link", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "DELETE", "/api/docs/"+esc(args[0])+"/links"+q("target", from), nil, func(d *store.Doc) {
				fmt.Fprintf(a.out, "unlinked %s from %s\n", from, d.Ref)
			})
		},
	}
	unlink.Flags().StringVar(&from, "from", "", "item/epic ref")
	unlink.MarkFlagRequired("from")
	cmd.AddCommand(unlink)

	cmd.AddCommand(&cobra.Command{
		Use: "revisions REF", Short: "List revisions", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return call(a, "GET", "/api/docs/"+esc(args[0])+"/revisions", nil, func(rs []store.Revision) {
				w := tw(a.out)
				for _, r := range rs {
					fmt.Fprintf(w, "r%d\t%s\t%s\t%s\n", r.Revision, when(r.CreatedAt), r.Author, r.Title)
				}
				w.Flush()
			})
		},
	})

	var dfrom, dto int
	diff := &cobra.Command{
		Use: "diff REF", Short: "Line diff between revisions (default: previous → current)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := a.client()
			var d store.Doc
			if err := c.Do("GET", "/api/docs/"+esc(args[0]), nil, &d); err != nil {
				return err
			}
			if dto == 0 {
				dto = d.Revision
			}
			if dfrom == 0 {
				dfrom = dto - 1
			}
			if dfrom < 1 {
				fmt.Fprintln(a.out, "(only one revision)")
				return nil
			}
			var ra, rb store.Revision
			if err := c.Do("GET", fmt.Sprintf("/api/docs/%s/revisions/%d", esc(args[0]), dfrom), nil, &ra); err != nil {
				return err
			}
			if err := c.Do("GET", fmt.Sprintf("/api/docs/%s/revisions/%d", esc(args[0]), dto), nil, &rb); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "--- %s r%d\n+++ %s r%d\n", d.Ref, dfrom, d.Ref, dto)
			for _, l := range render.Diff(ra.Body, rb.Body) {
				if l.Op != " " {
					fmt.Fprintf(a.out, "%s%s\n", l.Op, l.Text)
				}
			}
			return nil
		},
	}
	diff.Flags().IntVar(&dfrom, "from", 0, "from revision")
	diff.Flags().IntVar(&dto, "to", 0, "to revision")
	cmd.AddCommand(diff)

	cmd.AddCommand(a.pushCmd(), a.pullCmd())
	cmd.AddCommand(a.deleteCmd("delete REF", "Delete a document and its history", func(r string) string { return "/api/docs/" + esc(r) }))
	return cmd
}

// sinceSubmit returns the comments after the last submission (the current review round).
func sinceSubmit(cs []store.Comment) []store.Comment {
	last := -1
	for i, c := range cs {
		if c.Kind == "submit" {
			last = i
		}
	}
	return cs[last+1:]
}

// ---- markdown files <-> documents ----

type frontmatter struct {
	Tasker  string   `yaml:"tasker,omitempty"`
	Type    string   `yaml:"type,omitempty"`
	Title   string   `yaml:"title,omitempty"`
	For     []string `yaml:"for,omitempty"`
	Project string   `yaml:"project,omitempty"`
}

// splitFrontmatter returns the YAML frontmatter block (without fences) and the body.
func splitFrontmatter(text string) (string, string, bool) {
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return "", text, false
	}
	rest := text[strings.Index(text, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", text, false
	}
	fm := rest[:end]
	body := rest[end+4:]
	body = strings.TrimLeft(strings.TrimPrefix(body, "\r"), "\n")
	return fm, body, true
}

func stripFrontmatter(text string) string {
	_, body, _ := splitFrontmatter(text)
	return body
}

func guessDocType(path string) string {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "plan"):
		return "plan"
	case strings.Contains(p, "research"):
		return "research"
	case strings.Contains(p, "review"), strings.Contains(p, "audit"), strings.Contains(p, "security"):
		return "review"
	case strings.Contains(p, "adr"), strings.Contains(p, "decision"):
		return "decision"
	}
	return "note"
}

func (a *app) pushCmd() *cobra.Command {
	var typ string
	var forRefs []string
	var submit bool
	cmd := &cobra.Command{
		Use:   "push FILE...",
		Short: "Create or update documents from markdown files (id stored in frontmatter)",
		Long: `Pushes markdown files as documents. The first push creates the document and writes
"tasker: API-D7" into the file's frontmatter; later pushes update the same document
(a new revision; it goes back to review if it was already submitted).

Frontmatter (all optional): tasker, type, title, for: [API-12, API-E2], project.
Type defaults to --type, else is guessed from the path (plan/research/review/adr).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := a.client()
			for _, path := range args {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				fmText, body, hasFM := splitFrontmatter(string(data))
				var fm frontmatter
				if hasFM {
					if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
						return fmt.Errorf("%s: frontmatter: %w", path, err)
					}
				}
				if fm.Tasker != "" {
					patch := map[string]any{"body": body}
					if fm.Title != "" {
						patch["title"] = fm.Title
					}
					if submit {
						patch["submit"] = true
					}
					var d store.Doc
					if err := c.Do("PATCH", "/api/docs/"+esc(fm.Tasker), patch, &d); err != nil {
						return fmt.Errorf("%s: %w", path, err)
					}
					fmt.Fprintf(a.out, "%s → %s r%d (%s)\n", path, d.Ref, d.Revision, d.Status)
					continue
				}
				project := fm.Project
				if project == "" {
					if project, err = a.requireProject(); err != nil {
						return err
					}
				}
				t := fm.Type
				if cmd.Flags().Changed("type") || t == "" {
					t = typ
				}
				if t == "" {
					t = guessDocType(path)
				}
				in := store.DocInput{Type: t, Title: fm.Title, Body: body, For: append(fm.For, forRefs...), Submit: submit}
				if in.Title == "" && !strings.HasPrefix(strings.TrimSpace(body), "#") {
					in.Title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
				}
				var d store.Doc
				if err := c.Do("POST", "/api/projects/"+esc(project)+"/docs", in, &d); err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				if err := os.WriteFile(path, []byte(withTaskerID(string(data), d.Ref)), 0o644); err != nil {
					return err
				}
				fmt.Fprintf(a.out, "%s → created %s [%s] (%s)\n", path, d.Ref, d.Type, d.Status)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&typ, "type", "t", "", "document type for new documents")
	cmd.Flags().StringSliceVar(&forRefs, "for", nil, "link new documents to these refs")
	cmd.Flags().BoolVar(&submit, "submit", false, "submit for review")
	return cmd
}

// withTaskerID inserts "tasker: REF" into existing frontmatter, or adds frontmatter.
func withTaskerID(text, ref string) string {
	if strings.HasPrefix(text, "---\n") {
		if _, _, ok := splitFrontmatter(text); ok {
			return "---\ntasker: " + ref + "\n" + text[4:]
		}
	}
	return "---\ntasker: " + ref + "\n---\n\n" + text
}

func (a *app) pullCmd() *cobra.Command {
	var out, typ, status string
	cmd := &cobra.Command{
		Use:   "pull --out DIR",
		Short: "Export documents of a project to markdown files (with frontmatter)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := a.requireProject()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
			c := a.client()
			var docs []*store.Doc
			if err := c.Do("GET", "/api/docs"+q("project", p, "type", typ, "status", status), nil, &docs); err != nil {
				return err
			}
			existing, _ := filepath.Glob(filepath.Join(out, "*.md"))
			for _, d0 := range docs {
				var d store.Doc
				if err := c.Do("GET", "/api/docs/"+esc(d0.Ref), nil, &d); err != nil {
					return err
				}
				fm := map[string]any{"tasker": d.Ref, "type": d.Type, "title": d.Title, "status": d.Status, "revision": d.Revision}
				var links []string
				for _, l := range d.Links {
					links = append(links, l.Target)
				}
				if len(links) > 0 {
					fm["for"] = links
				}
				y, _ := yaml.Marshal(fm)
				content := "---\n" + string(y) + "---\n\n" + d.Body
				name := filepath.Join(out, strings.ToLower(d.Ref)+"-"+store.Slug(d.Title, 50)+".md")
				for _, e := range existing { // keep the existing file name for this ref
					if strings.HasPrefix(filepath.Base(e), strings.ToLower(d.Ref)+"-") {
						name = e
					}
				}
				if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
					return err
				}
				fmt.Fprintf(a.out, "%s r%d → %s\n", d.Ref, d.Revision, name)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "docs/tasker", "output directory")
	cmd.Flags().StringVarP(&typ, "type", "t", "", "only this type")
	cmd.Flags().StringVarP(&status, "status", "s", "", "only this status")
	return cmd
}
