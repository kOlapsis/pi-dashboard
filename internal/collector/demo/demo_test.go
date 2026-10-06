package demo

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
)

func TestCollectorsProduceData(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)
	clk := clock.NewFake(time.Date(2026, 10, 7, 9, 41, 0, 0, loc))
	cols := Collectors(clk, loc, 42, map[string]Mode{"stripe": "fail", "qonto": "stale"})
	seen := map[string]bool{}
	for _, c := range cols {
		assert.False(t, seen[c.Name()], "duplicate name %s", c.Name())
		seen[c.Name()] = true
		assert.Greater(t, c.Interval(), time.Second)
		data, err := c.Collect(context.Background())
		switch c.Name() {
		case "stripe":
			assert.Error(t, err)
			continue
		case "qonto":
			require.NoError(t, err)
			_, err = c.Collect(context.Background())
			assert.Error(t, err, "stale mode fails after the first success")
		default:
			require.NoError(t, err, c.Name())
		}
		b, err := json.Marshal(data)
		require.NoError(t, err)
		assert.Greater(t, len(b), 20, c.Name())
	}
	for _, name := range []string{"mail", "github", "umami", "stripe", "qonto", "weather", "calendar", "health", "registry", "maintenant", "shm"} {
		assert.True(t, seen[name], name)
	}
}

func TestGeneratorsAreDeterministic(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, loc)
	a := Collectors(clock.NewFake(now), loc, 7, nil)
	b := Collectors(clock.NewFake(now), loc, 7, nil)
	for i := range a {
		da, _ := a[i].Collect(context.Background())
		db, _ := b[i].Collect(context.Background())
		ja, _ := json.Marshal(da)
		jb, _ := json.Marshal(db)
		assert.JSONEq(t, string(ja), string(jb), a[i].Name())
	}
}
