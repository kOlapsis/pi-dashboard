package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func histories(t *testing.T) map[string]History {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "h.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return map[string]History{"sqlite": db, "mem": NewMem()}
}

func TestPutAtSeries(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for name, h := range histories(t) {
		t.Run(name, func(t *testing.T) {
			for i := range 10 {
				require.NoError(t, h.Put(ctx, "c", "k", base.Add(time.Duration(i)*24*time.Hour), float64(100+i)))
			}
			require.NoError(t, h.Put(ctx, "c", "k", base.Add(20*time.Minute), 150), "same hour bucket upserts")

			v, ok, err := h.At(ctx, "c", "k", base.Add(3*24*time.Hour+5*time.Hour))
			require.NoError(t, err)
			assert.True(t, ok)
			assert.Equal(t, 103.0, v, "latest point at or before t")

			v, ok, err = h.At(ctx, "c", "k", base.Add(-10*time.Hour))
			require.NoError(t, err)
			assert.True(t, ok)
			assert.Equal(t, 150.0, v, "falls forward to the first point within the window")

			_, ok, err = h.At(ctx, "c", "k", base.Add(-10*24*time.Hour))
			require.NoError(t, err)
			assert.False(t, ok)

			_, ok, err = h.At(ctx, "c", "other", base)
			require.NoError(t, err)
			assert.False(t, ok)

			pts, err := h.Series(ctx, "c", "k", base.Add(2*24*time.Hour), base.Add(5*24*time.Hour))
			require.NoError(t, err)
			require.Len(t, pts, 4)
			assert.Equal(t, 102.0, pts[0].V)
			assert.Equal(t, 105.0, pts[3].V)
			assert.True(t, pts[0].T.Before(pts[1].T))
		})
	}
}

func TestPrune(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "h.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		require.NoError(t, db.Put(ctx, "c", "k", base.Add(time.Duration(i)*24*time.Hour), float64(i)))
	}
	n, err := db.Prune(ctx, base.Add(2*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	pts, err := db.Series(ctx, "c", "k", base, base.Add(10*24*time.Hour))
	require.NoError(t, err)
	assert.Len(t, pts, 3)
}

func TestSaveLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	b, err := LoadFile(path)
	require.NoError(t, err)
	assert.Nil(t, b)
	require.NoError(t, SaveFile(path, []byte(`{"a":1}`)))
	b, err = LoadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":1}`, string(b))
}
