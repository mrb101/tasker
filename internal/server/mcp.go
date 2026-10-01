package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mrb101/tasker/internal/store"
)

// Minimal MCP server over "streamable HTTP" (JSON responses only, stateless).
// The CLI is the primary agent interface; this exists for MCP-only clients.

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	run         func(s *Server, actor string, args json.RawMessage) (any, error)
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func strs(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func enum(desc string, vals []string) map[string]any {
	return map[string]any{"type": "string", "enum": vals, "description": desc}
}

func args[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &v); err != nil {
			return v, &store.Error{Kind: store.ErrInvalid, Msg: "bad arguments: " + err.Error()}
		}
	}
	return v, nil
}

var itemSchema = map[string]any{
	"title":      str("Short title"),
	"body":       str("Markdown description"),
	"type":       enum("Item type", store.ItemTypes),
	"priority":   enum("Priority", store.Priorities),
	"epic":       str("Epic ref, e.g. API-E2"),
	"queue":      str("Queue name to append to"),
	"labels":     strs("Labels"),
	"branch":     str("Branch name (set to share a branch between items)"),
	"blocked_by": strs("Refs (or batch keys) this item depends on"),
	"key":        str("Batch-local key other items can reference in blocked_by"),
}

var mcpTools = []mcpTool{
	{
		Name: "tasker_projects", Description: "List projects with counts.",
		InputSchema: obj(map[string]any{}),
		run:         func(s *Server, _ string, _ json.RawMessage) (any, error) { return s.store.ListProjects() },
	},
	{
		Name:        "tasker_next",
		Description: "Get the next workable item. Give a queue (worktree lane) to follow queue order, or a project for priority order, or the worktree path. Returns the item with plan status, branch and base branch.",
		InputSchema: obj(map[string]any{"queue": str("Queue name or PROJECT/name"), "project": str("Project key"), "worktree_path": str("Absolute path of the current worktree")}),
		run: func(s *Server, actor string, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Queue, Project string
				Path           string `json:"worktree_path"`
			}](raw)
			if err != nil {
				return nil, err
			}
			return s.store.Next(store.NextQuery{Queue: a.Queue, Project: a.Project, Path: a.Path, Agent: actor})
		},
	},
	{
		Name: "tasker_get", Description: "Get an item (API-12), epic (API-E2) or document (API-D3) with links, comments and docs.",
		InputSchema: obj(map[string]any{"ref": str("Reference")}, "ref"),
		run: func(s *Server, _ string, raw json.RawMessage) (any, error) {
			a, err := args[struct{ Ref string }](raw)
			if err != nil {
				return nil, err
			}
			return s.lookup(a.Ref)
		},
	},
	{
		Name: "tasker_list_items", Description: "List items in a project. status accepts open, closed or a status name.",
		InputSchema: obj(map[string]any{"project": str("Project key"), "status": str("open | closed | inbox | queued | active | review | done | cancelled"),
			"type": enum("Type", store.ItemTypes), "epic": str("Epic ref"), "queue": str("Queue name or 'none'"), "q": str("Text search")}, "project"),
		run: func(s *Server, _ string, raw json.RawMessage) (any, error) {
			a, err := args[struct{ Project, Status, Type, Epic, Queue, Q string }](raw)
			if err != nil {
				return nil, err
			}
			st := []string{"open"}
			if a.Status != "" {
				st = []string{a.Status}
			}
			return s.store.ListItems(store.ItemFilter{Project: a.Project, Status: st, Type: a.Type, Epic: a.Epic, Queue: a.Queue, Query: a.Q})
		},
	},
	{
		Name:        "tasker_create_items",
		Description: "Create one or more items atomically. Batch defaults (epic, queue, branch, doc) apply to every item; doc links each item to a document (role 'found' by default, for review findings).",
		InputSchema: obj(map[string]any{
			"project": str("Project key"), "epic": str("Default epic ref"), "queue": str("Default queue"),
			"branch": str("Shared branch for all items"), "doc": str("Document ref to link items to"), "doc_role": enum("Link role", store.DocRoles),
			"items": map[string]any{"type": "array", "items": obj(itemSchema, "title")},
		}, "project", "items"),
		run: func(s *Server, actor string, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Project string `json:"project"`
				store.BulkInput
			}](raw)
			if err != nil {
				return nil, err
			}
			return s.store.BulkCreate(actor, a.Project, a.BulkInput)
		},
	},
	{
		Name: "tasker_update_item", Description: "Update item fields. Only given fields change. Set queue to '' to move back to the inbox.",
		InputSchema: obj(map[string]any{
			"ref": str("Item ref"), "title": str(""), "body": str("Markdown"), "type": enum("", store.ItemTypes), "status": enum("", store.ItemStatuses),
			"priority": enum("", store.Priorities), "epic": str("Epic ref or ''"), "queue": str("Queue name or ''"), "assignee": str(""),
			"branch": str(""), "pr_url": str(""), "add_labels": strs(""), "remove_labels": strs(""),
			"if_version": map[string]any{"type": "integer", "description": "Fail if the item changed since this version"},
		}, "ref"),
		run: func(s *Server, actor string, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Ref string `json:"ref"`
				store.ItemPatch
			}](raw)
			if err != nil {
				return nil, err
			}
			return s.store.UpdateItem(actor, a.Ref, a.ItemPatch)
		},
	},
	{
		Name: "tasker_start", Description: "Claim an item, mark it active and record its branch (default branch name is generated). Fails if blocked, claimed by another agent, or the plan gate requires an approved plan.",
		InputSchema: obj(map[string]any{"ref": str("Item ref"), "branch": str("Branch name (optional)")}, "ref"),
		run: func(s *Server, actor string, raw json.RawMessage) (any, error) {
			a, err := args[struct{ Ref, Branch string }](raw)
			if err != nil {
				return nil, err
			}
			return s.store.Start(actor, a.Ref, store.StartInput{Branch: a.Branch})
		},
	},
	{
		Name: "tasker_finish", Description: "Finish an item: status review (PR open, default) or done; releases the claim.",
		InputSchema: obj(map[string]any{"ref": str("Item ref"), "pr_url": str("Pull request URL"), "status": enum("", []string{"review", "done"}), "comment": str("Summary of what was done")}, "ref"),
		run: func(s *Server, actor string, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Ref string `json:"ref"`
				store.FinishInput
			}](raw)
			if err != nil {
				return nil, err
			}
			return s.store.Finish(actor, a.Ref, a.FinishInput)
		},
	},
	{
		Name: "tasker_link", Description: "Add a dependency between items: kind blocked_by | blocks | relates.",
		InputSchema: obj(map[string]any{"ref": str("Item ref"), "kind": enum("", []string{"blocked_by", "blocks", "relates"}), "target": str("Other item ref")}, "ref", "kind", "target"),
		run: func(s *Server, actor string, raw json.RawMessage) (any, error) {
			a, err := args[struct{ Ref, Kind, Target string }](raw)
			if err != nil {
				return nil, err
			}
			if err := s.store.AddLink(actor, a.Ref, a.Kind, a.Target); err != nil {
				return nil, err
			}
			return s.store.GetItem(a.Ref)
		},
	},
	{
		Name: "tasker_comment", Description: "Comment on an item, epic or document (markdown).",
		InputSchema: obj(map[string]any{"ref": str("Reference"), "body": str("Markdown")}, "ref", "body"),
		run: func(s *Server, actor string, raw json.RawMessage) (any, error) {
			a, err := args[struct{ Ref, Body string }](raw)
			if err != nil {
				return nil, err
			}
			return s.store.AddComment(actor, a.Ref, a.Body)
		},
	},
	{
		Name: "tasker_queue", Description: "Show a queue (worktree lane): ordered waiting items, items in review, recently done. Omit queue to list queues of a project.",
		InputSchema: obj(map[string]any{"queue": str("Queue name or PROJECT/name"), "project": str("Project key (to list queues)")}),
		run: func(s *Server, _ string, raw json.RawMessage) (any, error) {
			a, err := args[struct{ Queue, Project string }](raw)
			if err != nil {
				return nil, err
			}
			if a.Queue == "" {
				return s.store.ListQueues(a.Project, false)
			}
			return s.store.GetQueue(a.Queue)
		},
	},
	{
		Name: "tasker_queue_add", Description: "Put items into a queue (moving them from any other queue). where: top | bottom | before | after (with anchor ref).",
		InputSchema: obj(map[string]any{"queue": str("Queue"), "refs": strs("Item refs"), "where": enum("", []string{"top", "bottom", "before", "after"}), "anchor": str("Anchor item for before/after")}, "queue", "refs"),
		run: func(s *Server, actor string, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Queue, Where, Anchor string
				Refs                 []string
			}](raw)
			if err != nil {
				return nil, err
			}
			return s.store.QueueAdd(actor, a.Queue, a.Refs, store.Position{Where: a.Where, Anchor: a.Anchor})
		},
	},
	{
		Name: "tasker_create_doc", Description: "Create a document (research, plan, review, decision, note) linked to items/epics. submit=true sends it for human review.",
		InputSchema: obj(map[string]any{"project": str("Project key"), "type": enum("", store.DocTypes), "title": str("Title (default: first # heading)"),
			"body": str("Markdown"), "for": strs("Item/epic refs"), "role": enum("Link role (default by type)", store.DocRoles), "submit": boolean("Submit for review")}, "project", "type", "body"),
		run: func(s *Server, actor string, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Project string `json:"project"`
				store.DocInput
			}](raw)
			if err != nil {
				return nil, err
			}
			return s.store.CreateDoc(actor, a.Project, a.DocInput)
		},
	},
	{
		Name: "tasker_update_doc", Description: "Update a document; content changes create a new revision and go back to review once submitted. Read review comments with tasker_get.",
		InputSchema: obj(map[string]any{"ref": str("Doc ref"), "title": str(""), "body": str("Full new markdown"), "submit": boolean("Submit for review")}, "ref"),
		run: func(s *Server, actor string, raw json.RawMessage) (any, error) {
			a, err := args[struct {
				Ref string `json:"ref"`
				store.DocPatch
			}](raw)
			if err != nil {
				return nil, err
			}
			return s.store.UpdateDoc(actor, a.Ref, a.DocPatch)
		},
	},
	{
		Name: "tasker_search", Description: "Search items and documents by text.",
		InputSchema: obj(map[string]any{"q": str("Text"), "project": str("Limit to project")}, "q"),
		run: func(s *Server, _ string, raw json.RawMessage) (any, error) {
			a, err := args[struct{ Q, Project string }](raw)
			if err != nil {
				return nil, err
			}
			items, err := s.store.ListItems(store.ItemFilter{Project: a.Project, Query: a.Q, Limit: 50})
			if err != nil {
				return nil, err
			}
			docs, err := s.store.ListDocs(store.DocFilter{Project: a.Project, Query: a.Q})
			return map[string]any{"items": items, "docs": docs}, err
		},
	},
}

const mcpInstructions = `tasker tracks work for coding agents. Concepts: project (key like API) → epics (API-E1) group items (API-12: story/bug/security/research/chore).
A queue is one git worktree; items in a queue run in order, one branch each. Documents (API-D3: research, plan, review, decision) are reviewed by a human.
Typical loop: tasker_next → (write plan with tasker_create_doc submit=true if needed; poll tasker_get until approved) → tasker_start → implement on the given branch → tasker_finish with pr_url.`

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	var req rpcReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32700, "parse error"}})
		return
	}
	if len(req.ID) == 0 { // notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	actor := r.Header.Get("X-Tasker-Actor")
	if actor == "" {
		actor = "mcp-agent"
	}
	resp := rpcResp{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		resp.Result = map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "tasker", "version": s.version},
			"instructions":    mcpInstructions,
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpTools}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcErr{-32602, "invalid params"}
			break
		}
		var tool *mcpTool
		for i := range mcpTools {
			if mcpTools[i].Name == p.Name {
				tool = &mcpTools[i]
			}
		}
		if tool == nil {
			resp.Error = &rpcErr{-32602, "unknown tool " + p.Name}
			break
		}
		out, err := tool.run(s, actor, p.Arguments)
		if err != nil {
			resp.Result = map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": err.Error()}}}
			break
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}
	default:
		if strings.HasPrefix(req.Method, "notifications/") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		resp.Error = &rpcErr{-32601, fmt.Sprintf("method %s not found", req.Method)}
	}
	writeJSON(w, 200, resp)
}
