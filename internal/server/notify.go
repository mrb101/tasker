package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mrb101/tasker/internal/store"
)

// Notifier pushes "something waits on you" events to an ntfy topic
// (https://ntfy.sh or a self-hosted ntfy), so they reach a phone even when no
// tasker tab is open. Any URL that accepts a plain-text POST works; the
// Title/Click/Tags headers are ntfy's and other receivers ignore them.
type Notifier struct {
	URL       string // e.g. https://ntfy.sh/my-secret-topic
	PublicURL string // where people open tasker, for the notification's link
	Token     string // optional bearer token for a protected topic
	client    *http.Client
}

// message turns an event into a notification, or ok=false if it is not one.
func (n *Notifier) message(e store.Event) (title, body, link, tags string, ok bool) {
	switch e.Action {
	case store.EventReviewRequested:
		title, link, tags = "Review: "+e.Ref, "/d/"+e.Ref, "memo"
		body = fmt.Sprintf("%s\nsubmitted by %s — it waits for your approval.", e.Title, e.Actor)
	case store.EventItemInReview:
		title, link, tags = "Ready to merge: "+e.Ref, "/i/"+e.Ref, "white_check_mark"
		body = fmt.Sprintf("%s\nfinished by %s — the pull request waits for you.", e.Title, e.Actor)
	default:
		return "", "", "", "", false
	}
	return title, body, strings.TrimRight(n.PublicURL, "/") + link, tags, true
}

func (n *Notifier) send(ctx context.Context, e store.Event) error {
	title, body, link, tags, ok := n.message(e)
	if !ok {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", title)
	req.Header.Set("Tags", tags)
	if n.PublicURL != "" {
		req.Header.Set("Click", link)
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	res, err := n.client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s answered %s", n.URL, res.Status)
	}
	return nil
}

// StartNotifier sends notifications until ctx ends. It does nothing when no URL is set.
func (s *Server) StartNotifier(ctx context.Context, n *Notifier) {
	if n == nil || n.URL == "" {
		return
	}
	if n.client == nil {
		n.client = &http.Client{Timeout: 10 * time.Second}
	}
	ch, cancel := s.store.Subscribe()
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case e, open := <-ch:
				if !open {
					return
				}
				if err := n.send(ctx, e); err != nil {
					log.Printf("notify %s %s: %v", e.Action, e.Ref, err)
				}
			}
		}
	}()
	log.Printf("notifications go to %s", n.URL)
}
