// Package store owns the SQLite database and all business rules of tasker.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Event is published after every committed mutation (used for live UI updates).
type Event struct {
	Project string `json:"project"`
	Ref     string `json:"ref"`
	Action  string `json:"action"`
	Actor   string `json:"actor"`
	Title   string `json:"title,omitempty"` // set on events people are notified about
}

// Events that mean something now waits on a person.
const (
	EventReviewRequested = "review.requested" // a document was submitted for review
	EventItemInReview    = "item.review"      // a story was finished into review (a PR waits)
)

type Store struct {
	db *sql.DB

	mu   sync.Mutex
	subs map[chan Event]struct{}
}

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid")
)

// Error carries a user-facing message plus one of the sentinel kinds above.
type Error struct {
	Kind error
	Msg  string
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Kind }

func notFound(format string, a ...any) error {
	return &Error{ErrNotFound, fmt.Sprintf(format, a...)}
}
func conflict(format string, a ...any) error {
	return &Error{ErrConflict, fmt.Sprintf(format, a...)}
}
func invalid(format string, a ...any) error {
	return &Error{ErrInvalid, fmt.Sprintf(format, a...)}
}

type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// Open opens (and migrates) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	params := "_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_txlock=immediate"
	var dsn string
	if path == ":memory:" {
		dsn = "file::memory:?" + params
	} else {
		dsn = "file:" + path + "?" + params + "&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(8)
	}
	s := &Store{db: db, subs: map[chan Event]struct{}{}}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Backup writes a consistent snapshot of the database to path.
func (s *Store) Backup(path string) error {
	_, err := s.db.Exec("VACUUM INTO ?", path)
	return err
}

// Subscribe returns a channel receiving events until the returned cancel func is called.
func (s *Store) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
}

func (s *Store) publish(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- e:
		default: // slow subscriber; drop
		}
	}
}

// txc is a write transaction that records activity and collects events.
type txc struct {
	*sql.Tx
	actor  string
	events []Event
}

func (s *Store) write(actor string, fn func(t *txc) error) error {
	if strings.TrimSpace(actor) == "" {
		actor = "unknown"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	t := &txc{Tx: tx, actor: actor}
	if err := fn(t); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, e := range t.events {
		s.publish(e)
	}
	return nil
}

// log records an activity entry and queues a live event.
// notify queues an event that something now waits on a person; it is not logged as activity.
func (t *txc) notify(projectKey, ref, title, action string) {
	t.events = append(t.events, Event{Project: projectKey, Ref: ref, Action: action, Actor: t.actor, Title: title})
}

func (t *txc) log(projectID int64, projectKey, ref, action, detail string) error {
	_, err := t.Exec(`INSERT INTO activity(project_id, ref, actor, action, detail, created_at) VALUES(?,?,?,?,?,?)`,
		projectID, ref, t.actor, action, detail, now())
	t.events = append(t.events, Event{Project: projectKey, Ref: ref, Action: action, Actor: t.actor})
	return err
}

var migrations = []string{`
CREATE TABLE projects (
	id INTEGER PRIMARY KEY,
	key TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	plan_gate INTEGER NOT NULL DEFAULT 0,
	item_seq INTEGER NOT NULL DEFAULT 0,
	epic_seq INTEGER NOT NULL DEFAULT 0,
	doc_seq INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE epics (
	id INTEGER PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	number INTEGER NOT NULL,
	title TEXT NOT NULL,
	body TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'open',
	version INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(project_id, number)
);
CREATE TABLE queues (
	id INTEGER PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	base_branch TEXT NOT NULL DEFAULT 'main',
	stacked INTEGER NOT NULL DEFAULT 0,
	worktree TEXT NOT NULL DEFAULT '',
	agent TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'active',
	position INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(project_id, name)
);
CREATE TABLE items (
	id INTEGER PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	number INTEGER NOT NULL,
	epic_id INTEGER REFERENCES epics(id) ON DELETE SET NULL,
	queue_id INTEGER REFERENCES queues(id) ON DELETE SET NULL,
	queue_pos INTEGER NOT NULL DEFAULT 0,
	type TEXT NOT NULL DEFAULT 'story',
	title TEXT NOT NULL,
	body TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'inbox',
	priority INTEGER NOT NULL DEFAULT 2,
	assignee TEXT NOT NULL DEFAULT '',
	branch TEXT NOT NULL DEFAULT '',
	pr_url TEXT NOT NULL DEFAULT '',
	claimed_by TEXT NOT NULL DEFAULT '',
	claim_expires TEXT NOT NULL DEFAULT '',
	version INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	closed_at TEXT NOT NULL DEFAULT '',
	UNIQUE(project_id, number)
);
CREATE INDEX items_queue ON items(queue_id, queue_pos);
CREATE INDEX items_status ON items(project_id, status);
CREATE TABLE item_labels (
	item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	label TEXT NOT NULL,
	PRIMARY KEY(item_id, label)
);
CREATE TABLE item_links (
	from_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	to_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	kind TEXT NOT NULL,
	PRIMARY KEY(from_id, to_id, kind)
);
CREATE INDEX item_links_to ON item_links(to_id);
CREATE TABLE documents (
	id INTEGER PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	number INTEGER NOT NULL,
	type TEXT NOT NULL,
	title TEXT NOT NULL,
	body TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'draft',
	author TEXT NOT NULL DEFAULT '',
	revision INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(project_id, number)
);
CREATE TABLE doc_revisions (
	doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	revision INTEGER NOT NULL,
	title TEXT NOT NULL,
	body TEXT NOT NULL,
	author TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY(doc_id, revision)
);
CREATE TABLE doc_links (
	doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	target_kind TEXT NOT NULL,
	target_id INTEGER NOT NULL,
	role TEXT NOT NULL,
	PRIMARY KEY(doc_id, target_kind, target_id, role)
);
CREATE INDEX doc_links_target ON doc_links(target_kind, target_id);
CREATE TABLE comments (
	id INTEGER PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	target_kind TEXT NOT NULL,
	target_id INTEGER NOT NULL,
	author TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'comment',
	revision INTEGER NOT NULL DEFAULT 0,
	body TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX comments_target ON comments(target_kind, target_id);
CREATE TABLE activity (
	id INTEGER PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	ref TEXT NOT NULL,
	actor TEXT NOT NULL,
	action TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE INDEX activity_ref ON activity(ref);
CREATE INDEX activity_project ON activity(project_id, id);
`, `
CREATE INDEX items_epic ON items(epic_id);
CREATE INDEX items_project_queue ON items(project_id, queue_id);
`, `
ALTER TABLE projects ADD COLUMN release_mode TEXT NOT NULL DEFAULT 'date';
ALTER TABLE projects ADD COLUMN release_weeks INTEGER NOT NULL DEFAULT 4;
ALTER TABLE projects ADD COLUMN release_seq INTEGER NOT NULL DEFAULT 0;
CREATE TABLE releases (
	id INTEGER PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	number INTEGER NOT NULL,
	name TEXT NOT NULL,
	goal TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'planned',
	starts_on TEXT NOT NULL DEFAULT '',
	target_on TEXT NOT NULL DEFAULT '',
	shipped_at TEXT NOT NULL DEFAULT '',
	tag TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(project_id, number)
);
ALTER TABLE items ADD COLUMN release_id INTEGER REFERENCES releases(id) ON DELETE SET NULL;
ALTER TABLE items ADD COLUMN stretch INTEGER NOT NULL DEFAULT 0;
CREATE INDEX items_release ON items(release_id);
`, `
ALTER TABLE projects ADD COLUMN repo_url TEXT NOT NULL DEFAULT '';
CREATE TABLE item_files (
	item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	path TEXT NOT NULL,
	source TEXT NOT NULL,
	old_path TEXT NOT NULL DEFAULT '',
	added INTEGER NOT NULL DEFAULT 0,
	deleted INTEGER NOT NULL DEFAULT 0,
	line_start INTEGER NOT NULL DEFAULT 0,
	line_end INTEGER NOT NULL DEFAULT 0,
	snippet TEXT NOT NULL DEFAULT '',
	at_sha TEXT NOT NULL DEFAULT '',
	position INTEGER NOT NULL DEFAULT 0,
	recorded_at TEXT NOT NULL
);
CREATE INDEX item_files_item ON item_files(item_id, source);
CREATE INDEX item_files_path ON item_files(path);
CREATE TABLE item_commits (
	item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	sha TEXT NOT NULL,
	subject TEXT NOT NULL DEFAULT '',
	authored_at TEXT NOT NULL DEFAULT '',
	position INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(item_id, sha)
);
CREATE INDEX item_commits_sha ON item_commits(sha);
ALTER TABLE items ADD COLUMN head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN base_ref TEXT NOT NULL DEFAULT '';
`}

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
