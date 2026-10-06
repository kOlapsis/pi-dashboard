package github

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	reposPerPage = 100
	maxRepoPages = 10
)

type repo struct {
	owner, name, label       string
	stars, forks, openIssues int
	pushedAt                 time.Time
}

func (r repo) full() string { return r.owner + "/" + r.name }

type apiRepo struct {
	Name       string    `json:"name"`
	Stars      int       `json:"stargazers_count"`
	Forks      int       `json:"forks_count"`
	OpenIssues int       `json:"open_issues_count"`
	PushedAt   time.Time `json:"pushed_at"`
	CreatedAt  time.Time `json:"created_at"`
}

func (c *Collector) repos(ctx context.Context) ([]repo, error) {
	var (
		out []repo
		err error
	)
	if len(c.cfg.Repos) > 0 {
		out, err = c.namedRepos(ctx)
	} else {
		out, err = c.orgRepos(ctx)
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b repo) int {
		return cmp.Or(cmp.Compare(b.stars, a.stars), cmp.Compare(a.label, b.label))
	})
	return out, nil
}

func (c *Collector) orgRepos(ctx context.Context) ([]repo, error) {
	var out []repo
	next := fmt.Sprintf("%s/orgs/%s/repos?per_page=%d&type=public&sort=pushed", c.base, url.PathEscape(c.cfg.Org), reposPerPage)
	for range maxRepoPages {
		var page []apiRepo
		h, err := c.getJSON(ctx, next, acceptJSON, &page)
		if err != nil {
			return nil, fmt.Errorf("list %s repositories: %w", c.cfg.Org, err)
		}
		for _, a := range page {
			if !c.excluded(c.cfg.Org, a.Name) {
				out = append(out, c.newRepo(c.cfg.Org, a))
			}
		}
		if next = links(h)["next"]; next == "" {
			return out, nil
		}
		if !c.sameOrigin(next) {
			return nil, fmt.Errorf("list %s repositories: pagination link leaves %s", c.cfg.Org, c.base)
		}
	}
	return nil, fmt.Errorf("list %s repositories: more than %d repositories, set github.repos", c.cfg.Org, maxRepoPages*reposPerPage)
}

func (c *Collector) namedRepos(ctx context.Context) ([]repo, error) {
	out := make([]repo, 0, len(c.cfg.Repos))
	for _, entry := range c.cfg.Repos {
		owner, name, err := c.qualify(entry)
		if err != nil {
			return nil, err
		}
		if c.excluded(owner, name) {
			continue
		}
		var a apiRepo
		if _, err := c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s", c.base, url.PathEscape(owner), url.PathEscape(name)), acceptJSON, &a); err != nil {
			return nil, fmt.Errorf("get repository %s/%s: %w", owner, name, err)
		}
		if a.Name == "" {
			a.Name = name
		}
		out = append(out, c.newRepo(owner, a))
	}
	return out, nil
}

func (c *Collector) qualify(entry string) (string, string, error) {
	entry = strings.TrimSpace(entry)
	if owner, name, ok := strings.Cut(entry, "/"); ok {
		if owner == "" || name == "" {
			return "", "", fmt.Errorf("github.repos: invalid entry %q", entry)
		}
		return owner, name, nil
	}
	if c.cfg.Org == "" || entry == "" {
		return "", "", fmt.Errorf("github.repos: entry %q needs github.org or an owner/name form", entry)
	}
	return c.cfg.Org, entry, nil
}

func (c *Collector) newRepo(owner string, a apiRepo) repo {
	r := repo{
		owner: owner, name: a.Name, label: a.Name,
		stars: a.Stars, forks: a.Forks, openIssues: a.OpenIssues, pushedAt: a.PushedAt,
	}
	if r.pushedAt.IsZero() {
		r.pushedAt = a.CreatedAt
	}
	if !strings.EqualFold(owner, c.cfg.Org) {
		r.label = r.full()
	}
	return r
}

func (c *Collector) excluded(owner, name string) bool { return listed(c.cfg.Exclude, owner, name) }

func (c *Collector) wantsTraffic(r repo) bool { return listed(c.cfg.TrafficRepos, r.owner, r.name) }

func listed(entries []string, owner, name string) bool {
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if strings.EqualFold(e, name) || strings.EqualFold(e, owner+"/"+name) {
			return true
		}
	}
	return false
}

func (c *Collector) openPRs(ctx context.Context, r repo) (int, error) {
	// open_issues_count includes pull requests, so zero means there are none to count.
	if r.openIssues == 0 {
		return 0, nil
	}
	var items []json.RawMessage
	u := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&per_page=1", c.base, url.PathEscape(r.owner), url.PathEscape(r.name))
	h, err := c.getJSON(ctx, u, acceptJSON, &items)
	if err != nil {
		return 0, err
	}
	if n := lastPage(h); n > 0 {
		return n, nil
	}
	return len(items), nil
}
