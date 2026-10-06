package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Point struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

type History interface {
	Put(ctx context.Context, collector, key string, at time.Time, v float64) error
	At(ctx context.Context, collector, key string, t time.Time) (float64, bool, error)
	Series(ctx context.Context, collector, key string, from, to time.Time) ([]Point, error)
}

// Bucket is the resolution at which history points are kept.
const Bucket = time.Hour

const atWindow = 36 * time.Hour

type DB struct {
	db *sql.DB
}

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS history (
		collector TEXT NOT NULL,
		key TEXT NOT NULL,
		ts INTEGER NOT NULL,
		value REAL NOT NULL,
		PRIMARY KEY (collector, key, ts)) WITHOUT ROWID`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("history schema: %w", err)
	}
	return &DB{db: db}, nil
}

func (d *DB) Close() error { return d.db.Close() }

func (d *DB) Put(ctx context.Context, collector, key string, at time.Time, v float64) error {
	_, err := d.db.ExecContext(ctx, `INSERT INTO history (collector, key, ts, value) VALUES (?, ?, ?, ?)
		ON CONFLICT (collector, key, ts) DO UPDATE SET value = excluded.value WHERE value IS NOT excluded.value`,
		collector, key, at.Truncate(Bucket).Unix(), v)
	return err
}

func (d *DB) At(ctx context.Context, collector, key string, t time.Time) (float64, bool, error) {
	var v float64
	err := d.db.QueryRowContext(ctx, `SELECT value FROM history WHERE collector = ? AND key = ? AND ts <= ? AND ts >= ?
		ORDER BY ts DESC LIMIT 1`, collector, key, t.Unix(), t.Add(-atWindow).Unix()).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		err = d.db.QueryRowContext(ctx, `SELECT value FROM history WHERE collector = ? AND key = ? AND ts > ? AND ts <= ?
			ORDER BY ts ASC LIMIT 1`, collector, key, t.Unix(), t.Add(atWindow).Unix()).Scan(&v)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

func (d *DB) Series(ctx context.Context, collector, key string, from, to time.Time) ([]Point, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT ts, value FROM history WHERE collector = ? AND key = ? AND ts BETWEEN ? AND ?
		ORDER BY ts`, collector, key, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Point
	for rows.Next() {
		var ts int64
		var v float64
		if err := rows.Scan(&ts, &v); err != nil {
			return nil, err
		}
		out = append(out, Point{T: time.Unix(ts, 0).UTC(), V: v})
	}
	return out, rows.Err()
}

func (d *DB) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := d.db.ExecContext(ctx, `DELETE FROM history WHERE ts < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

type Mem struct {
	mu   sync.Mutex
	data map[string]map[int64]float64
}

func NewMem() *Mem { return &Mem{data: map[string]map[int64]float64{}} }

func memKey(collector, key string) string { return collector + "\x00" + key }

func (m *Mem) Put(_ context.Context, collector, key string, at time.Time, v float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(collector, key)
	if m.data[k] == nil {
		m.data[k] = map[int64]float64{}
	}
	m.data[k][at.Truncate(Bucket).Unix()] = v
	return nil
}

func (m *Mem) At(_ context.Context, collector, key string, t time.Time) (float64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var (
		bestBefore, bestAfter   int64 = -1, -1
		valBefore, valAfter     float64
		lo, hi                        = t.Add(-atWindow).Unix(), t.Add(atWindow).Unix()
	)
	for ts, v := range m.data[memKey(collector, key)] {
		switch {
		case ts <= t.Unix() && ts >= lo && ts > bestBefore:
			bestBefore, valBefore = ts, v
		case ts > t.Unix() && ts <= hi && (bestAfter < 0 || ts < bestAfter):
			bestAfter, valAfter = ts, v
		}
	}
	if bestBefore >= 0 {
		return valBefore, true, nil
	}
	if bestAfter >= 0 {
		return valAfter, true, nil
	}
	return 0, false, nil
}

func (m *Mem) Series(_ context.Context, collector, key string, from, to time.Time) ([]Point, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Point
	for ts, v := range m.data[memKey(collector, key)] {
		if ts >= from.Unix() && ts <= to.Unix() {
			out = append(out, Point{T: time.Unix(ts, 0).UTC(), V: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out, nil
}

func SaveFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func LoadFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}
