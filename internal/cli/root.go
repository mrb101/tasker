// Package cli implements the tasker command line (server + client).
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrb101/tasker/internal/server"
	"github.com/mrb101/tasker/internal/store"
)

type app struct {
	url     string
	actor   string
	project string
	json    bool
	version string
	out     io.Writer
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func defaultActor() string {
	if a := os.Getenv("TASKER_ACTOR"); a != "" {
		return a
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "cli"
}

// Execute runs the CLI and returns the process exit code.
func Execute(version string) int {
	a := &app{version: version, out: os.Stdout}
	root := a.rootCmd()
	if err := root.Execute(); err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			fmt.Fprintln(os.Stderr, "error:", ae.Msg)
			return ae.ExitCode()
		}
		var ee *exitErr
		if errors.As(err, &ee) {
			if ee.msg != "" {
				fmt.Fprintln(os.Stderr, ee.msg)
			}
			return ee.code
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		if strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "flag") || strings.Contains(err.Error(), "arg") {
			return ExitUsage
		}
		return ExitError
	}
	return 0
}

type exitErr struct {
	code int
	msg  string
}

func (e *exitErr) Error() string { return e.msg }

func (a *app) client() *Client { return NewClient(a.url, a.actor) }

func (a *app) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "tasker",
		Short:         "Local work tracker for humans and coding agents",
		Long:          "tasker tracks projects, epics, items, queues (one per worktree) and reviewable documents.\nRun `tasker serve` for the server + web UI; every other command is a client.\nAgents: run `tasker agent-guide` for a compact usage guide.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       a.version,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&a.url, "url", env("TASKER_URL", "http://localhost:7777"), "server URL (env TASKER_URL)")
	pf.StringVar(&a.actor, "as", defaultActor(), "who is acting, e.g. an agent name (env TASKER_ACTOR)")
	pf.StringVarP(&a.project, "project", "p", os.Getenv("TASKER_PROJECT"), "project key (env TASKER_PROJECT; defaults to the project of the queue bound to this worktree)")
	pf.BoolVar(&a.json, "json", false, "print raw JSON")

	root.AddCommand(
		a.serveCmd(), a.healthCmd(), a.guideCmd(), a.backupCmd(),
		a.projectCmd(), a.epicCmd(), a.itemCmd(), a.queueCmd(), a.docCmd(),
		a.showCmd(), a.nextCmd(), a.startCmd(), a.finishCmd(), a.claimCmd(), a.releaseCmd(),
		a.linkCmd(false), a.linkCmd(true), a.commentCmd(), a.searchCmd(), a.activityCmd(),
		a.addAlias(), a.lsAlias(), a.mcpCmd(),
	)
	return root
}

// ---- helpers ----

func (a *app) printJSON(data any) error {
	var b []byte
	switch v := data.(type) {
	case []byte:
		var buf bytes.Buffer
		if json.Indent(&buf, bytes.TrimSpace(v), "", "  ") == nil {
			b = buf.Bytes()
		} else {
			b = v
		}
	default:
		b, _ = json.MarshalIndent(v, "", "  ")
	}
	fmt.Fprintln(a.out, string(b))
	return nil
}

// call performs a request and either prints the raw JSON (--json) or decodes
// into T and calls human.
func call[T any](a *app, method, path string, body any, human func(T)) error {
	data, err := a.client().raw(method, path, body)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(data)
	}
	var v T
	if len(data) > 0 {
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
	}
	human(v)
	return nil
}

// requireProject returns the project key from -p, env, or the worktree's bound queue.
func (a *app) requireProject() (string, error) {
	if a.project != "" {
		return strings.ToUpper(a.project), nil
	}
	if q, err := a.currentQueue(); err == nil {
		return q.Project, nil
	}
	return "", &exitErr{ExitUsage, "error: no project given; use -p KEY, set TASKER_PROJECT, or bind this worktree to a queue"}
}

// worktreeRoot returns the git worktree root of cwd (read-only), falling back to cwd.
func worktreeRoot() string {
	cwd, _ := os.Getwd()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			return p
		}
	}
	return cwd
}

func (a *app) currentQueue() (*store.Queue, error) {
	cwd, _ := os.Getwd()
	var q store.Queue
	if err := a.client().Do("GET", "/api/queues"+q_("path", cwd), nil, &q); err != nil {
		return nil, err
	}
	return &q, nil
}

func q_(kv ...string) string { return q(kv...) }

// queueArg resolves a queue from an explicit arg, TASKER_QUEUE, or the bound worktree.
func (a *app) queueArg(explicit string) (string, error) {
	if explicit != "" {
		if !strings.Contains(explicit, "/") && a.project != "" {
			return strings.ToUpper(a.project) + "/" + explicit, nil
		}
		return explicit, nil
	}
	if e := os.Getenv("TASKER_QUEUE"); e != "" {
		return e, nil
	}
	qu, err := a.currentQueue()
	if err != nil {
		return "", &exitErr{ExitUsage, "error: no queue given and this directory is not bound to a queue (tasker queue bind NAME)"}
	}
	return qu.Ref, nil
}

func queuePath(ref string) (string, error) {
	i := strings.Index(ref, "/")
	if i < 0 {
		return "", fmt.Errorf("queue %q must be PROJECT/name here", ref)
	}
	return "/api/queues/" + esc(strings.ToUpper(ref[:i])) + "/" + esc(ref[i+1:]), nil
}

// resolveQueueRef turns a bare queue name into PROJECT/name via the server.
func (a *app) resolveQueueRef(name string) (string, error) {
	name, err := a.queueArg(name)
	if err != nil {
		return "", err
	}
	if strings.Contains(name, "/") {
		return name, nil
	}
	var list []store.Queue
	if err := a.client().Do("GET", "/api/queues"+q("all", "1"), nil, &list); err != nil {
		return "", err
	}
	var hits []string
	for _, qu := range list {
		if qu.Name == strings.ToLower(name) {
			hits = append(hits, qu.Ref)
		}
	}
	switch len(hits) {
	case 0:
		return "", &APIError{Kind: "not_found", Msg: "queue " + name + " not found"}
	case 1:
		return hits[0], nil
	}
	return "", &exitErr{ExitUsage, fmt.Sprintf("error: queue %q exists in several projects (%s); use -p or PROJECT/name", name, strings.Join(hits, ", "))}
}

// readText reads --body / -f file / "-" stdin.
func readText(body, file string) (string, bool, error) {
	switch {
	case file == "-":
		b, err := io.ReadAll(os.Stdin)
		return string(b), true, err
	case file != "":
		b, err := os.ReadFile(file)
		return string(b), true, err
	case body != "":
		return body, true, nil
	}
	return "", false, nil
}

// ---- server ----

func (a *app) serveCmd() *cobra.Command {
	var addr, db string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the tasker server (API, MCP endpoint and web UI)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir := filepath.Dir(db); dir != "" {
				os.MkdirAll(dir, 0o755)
			}
			st, err := store.Open(db)
			if err != nil {
				return err
			}
			defer st.Close()
			srv, err := server.New(st, a.version)
			if err != nil {
				return err
			}
			hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				hs.Shutdown(sctx)
			}()
			log.Printf("tasker %s listening on %s (db %s)", a.version, addr, db)
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", env("TASKER_ADDR", ":7777"), "listen address (env TASKER_ADDR)")
	cmd.Flags().StringVar(&db, "db", env("TASKER_DB", "tasker.db"), "SQLite database path (env TASKER_DB)")
	return cmd
}

func (a *app) healthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "healthcheck",
		Short: "Exit 0 if the server answers (used by Docker HEALTHCHECK)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var h map[string]any
			if err := a.client().Do("GET", "/api/health", nil, &h); err != nil {
				return err
			}
			fmt.Fprintln(a.out, "ok")
			return nil
		},
	}
}

func (a *app) backupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backup FILE",
		Short: "Download a consistent snapshot of the server database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := a.client().raw("GET", "/api/backup", nil)
			if err != nil {
				return err
			}
			if err := os.WriteFile(args[0], data, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "wrote %s (%d bytes)\n", args[0], len(data))
			return nil
		},
	}
}
