package store

import (
	"net/url"
	"strconv"
	"strings"
)

// Links from tasker to a repository's web pages, for the hosts people use.
// tasker never talks to the host; it only builds addresses. A project on a
// machine with no web view leaves the repository address empty: no links.

// RepoHosts are the address styles tasker knows. "custom" uses the project's
// own patterns (cgit, gitweb, anything else).
var RepoHosts = []string{"github", "gitlab", "gitea", "bitbucket", "custom"}

type repoStyle struct {
	file, commit string // {repo} {sha} {path} {lines}
	lines        func(a, b int) string
}

var repoStyles = map[string]repoStyle{
	"github": {"{repo}/blob/{sha}/{path}{lines}", "{repo}/commit/{sha}", func(a, b int) string {
		if b > a {
			return "#L" + strconv.Itoa(a) + "-L" + strconv.Itoa(b)
		}
		return "#L" + strconv.Itoa(a)
	}},
	"gitlab": {"{repo}/-/blob/{sha}/{path}{lines}", "{repo}/-/commit/{sha}", func(a, b int) string {
		if b > a {
			return "#L" + strconv.Itoa(a) + "-" + strconv.Itoa(b)
		}
		return "#L" + strconv.Itoa(a)
	}},
	"gitea": {"{repo}/src/commit/{sha}/{path}{lines}", "{repo}/commit/{sha}", func(a, b int) string {
		if b > a {
			return "#L" + strconv.Itoa(a) + "-L" + strconv.Itoa(b)
		}
		return "#L" + strconv.Itoa(a)
	}},
	"bitbucket": {"{repo}/src/{sha}/{path}{lines}", "{repo}/commits/{sha}", func(a, b int) string {
		if b > a {
			return "#lines-" + strconv.Itoa(a) + ":" + strconv.Itoa(b)
		}
		return "#lines-" + strconv.Itoa(a)
	}},
}

// GuessRepoHost picks the address style from the repository address. Hosts
// on their own domains (a company GitLab) are not recognised: choose one.
func GuessRepoHost(repo string) string {
	u, err := url.Parse(repo)
	if err != nil {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	switch {
	case strings.Contains(h, "github"):
		return "github"
	case strings.Contains(h, "gitlab"):
		return "gitlab"
	case strings.Contains(h, "bitbucket"):
		return "bitbucket"
	case strings.Contains(h, "gitea"), strings.Contains(h, "forgejo"), strings.Contains(h, "codeberg"):
		return "gitea"
	}
	return ""
}

// LinkHost is the address style in use: the chosen one, else the guessed one.
func (p *Project) LinkHost() string {
	if p.RepoHost != "" {
		return p.RepoHost
	}
	return GuessRepoHost(p.RepoURL)
}

func (p *Project) fill(tpl, sha, path, lines string) string {
	parts := strings.Split(path, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.NewReplacer("{repo}", p.RepoURL, "{sha}", sha, "{path}", strings.Join(parts, "/"), "{lines}", lines).Replace(tpl)
}

// FileURL links a file at a commit, optionally to lines; "" when the project has no links.
func (p *Project) FileURL(sha, path string, line, end int) string {
	if p == nil || sha == "" {
		return ""
	}
	host := p.LinkHost()
	if host == "custom" {
		if p.RepoFileURL == "" {
			return ""
		}
		lines := ""
		if line > 0 {
			lines = strconv.Itoa(line)
		}
		return strings.ReplaceAll(p.fill(p.RepoFileURL, sha, path, ""), "{line}", lines)
	}
	st, ok := repoStyles[host]
	if !ok || p.RepoURL == "" {
		return ""
	}
	lines := ""
	if line > 0 {
		lines = st.lines(line, end)
	}
	return p.fill(st.file, sha, path, lines)
}

// CommitURL links a commit; "" when the project has no links.
func (p *Project) CommitURL(sha string) string {
	if p == nil || sha == "" {
		return ""
	}
	host := p.LinkHost()
	if host == "custom" {
		if p.RepoCommitURL == "" {
			return ""
		}
		return p.fill(p.RepoCommitURL, sha, "", "")
	}
	st, ok := repoStyles[host]
	if !ok || p.RepoURL == "" {
		return ""
	}
	return p.fill(st.commit, sha, "", "")
}
