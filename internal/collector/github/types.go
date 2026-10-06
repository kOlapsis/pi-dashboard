package github

import (
	"time"

	"github.com/kolapsis/pi-dashboard/internal/store"
)

type Data struct {
	Stars          int           `json:"stars"`
	Delta7         *int          `json:"delta7"`
	Series30       []store.Point `json:"series30"`
	Repos          []Repo        `json:"repos"`
	TokenExpiresAt *time.Time    `json:"token_expires_at,omitempty"`
}

type Repo struct {
	Name     string    `json:"name"`
	Stars    int       `json:"stars"`
	Delta7   *int      `json:"delta7"`
	Forks    int       `json:"forks"`
	Issues   int       `json:"issues"`
	PRs      int       `json:"prs"`
	PushedAt time.Time `json:"pushed_at"`
	Views14  *int      `json:"views14,omitempty"`
	Clones14 *int      `json:"clones14,omitempty"`
}
