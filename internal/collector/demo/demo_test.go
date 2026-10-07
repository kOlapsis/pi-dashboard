package demo

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
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

func TestDemoEmitsEvents(t *testing.T) {
	loc := time.UTC
	clk := clock.NewFake(time.Date(2026, 10, 7, 9, 0, 0, 0, loc))
	byName := map[string]collector.Collector{}
	for _, c := range Collectors(clk, loc, 3, nil) {
		byName[c.Name()] = c
	}
	events := func(name string, steps int) (labels []string) {
		c := byName[name]
		prev, err := c.Collect(context.Background())
		require.NoError(t, err)
		for range steps {
			cur, err := c.Collect(context.Background())
			require.NoError(t, err)
			labels = append(labels, c.(collector.Notifier).Notify(prev, cur)...)
			prev = cur
		}
		return labels
	}
	assert.NotEmpty(t, events("mail", 8))
	assert.NotEmpty(t, events("stripe", 16))
	assert.NotEmpty(t, events("qonto", 20))
	assert.Contains(t, events("health", 10), "restoreproof.io est down")
	assert.Empty(t, events("weather", 3))
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
