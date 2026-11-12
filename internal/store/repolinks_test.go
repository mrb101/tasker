package store

import (
	"errors"
	"testing"
)

func TestRepoLinks(t *testing.T) {
	const sha = "abc123def"
	for _, c := range []struct {
		p            Project
		file, commit string
	}{
		{Project{RepoURL: "https://github.com/me/api"}, "https://github.com/me/api/blob/abc123def/a/b%20c.go#L3-L9", "https://github.com/me/api/commit/abc123def"},
		{Project{RepoURL: "https://gitlab.com/team/api"}, "https://gitlab.com/team/api/-/blob/abc123def/a/b%20c.go#L3-9", "https://gitlab.com/team/api/-/commit/abc123def"},
		{Project{RepoURL: "https://git.corp.example/team/api", RepoHost: "gitlab"}, "https://git.corp.example/team/api/-/blob/abc123def/a/b%20c.go#L3-9", "https://git.corp.example/team/api/-/commit/abc123def"},
		{Project{RepoURL: "https://codeberg.org/me/api"}, "https://codeberg.org/me/api/src/commit/abc123def/a/b%20c.go#L3-L9", "https://codeberg.org/me/api/commit/abc123def"},
		{Project{RepoURL: "https://bitbucket.org/me/api"}, "https://bitbucket.org/me/api/src/abc123def/a/b%20c.go#lines-3:9", "https://bitbucket.org/me/api/commits/abc123def"},
		{Project{RepoURL: "http://nas.local/cgit/api", RepoHost: "custom", RepoFileURL: "{repo}/tree/{path}?id={sha}#n{line}", RepoCommitURL: "{repo}/commit/?id={sha}"},
			"http://nas.local/cgit/api/tree/a/b%20c.go?id=abc123def#n3", "http://nas.local/cgit/api/commit/?id=abc123def"},
		{Project{RepoURL: "https://git.corp.example/team/api"}, "", ""}, // unknown host, no style chosen: no links
		{Project{}, "", ""}, // plain git, no web view
	} {
		if got := c.p.FileURL(sha, "a/b c.go", 3, 9); got != c.file {
			t.Errorf("%s file: %s", c.p.RepoURL, got)
		}
		if got := c.p.CommitURL(sha); got != c.commit {
			t.Errorf("%s commit: %s", c.p.RepoURL, got)
		}
	}
	gh := Project{RepoURL: "https://github.com/me/api"}
	if got := gh.FileURL(sha, "a.go", 0, 0); got != "https://github.com/me/api/blob/abc123def/a.go" {
		t.Errorf("no lines: %s", got)
	}
	if got := gh.FileURL("", "a.go", 1, 0); got != "" {
		t.Errorf("no commit: %s", got)
	}

	s := newStore(t)
	p := must(s.UpdateProject("me", "API", ProjectPatch{RepoURL: ptr("https://git.corp.example/team/api"), RepoHost: ptr("GitLab")}))
	if p.LinkHost() != "gitlab" {
		t.Fatalf("host %q", p.LinkHost())
	}
	if p = must(s.UpdateProject("me", "API", ProjectPatch{RepoHost: ptr("auto")})); p.RepoHost != "" || p.LinkHost() != "" {
		t.Fatalf("auto %q %q", p.RepoHost, p.LinkHost())
	}
	for _, bad := range []ProjectPatch{
		{RepoHost: ptr("sourcehut")},
		{RepoHost: ptr("custom")},                 // no patterns
		{RepoFileURL: ptr("ftp://x/{path}")},      // not a web address
		{RepoCommitURL: ptr("https://x/commit/")}, // no {sha}
	} {
		if _, err := s.UpdateProject("me", "API", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %+v: %v", bad, err)
		}
	}
}
