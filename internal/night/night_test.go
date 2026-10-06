package night

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func at(h, m int) time.Time { return time.Date(2026, 10, 7, h, m, 0, 0, time.UTC) }

func TestWindowAcrossMidnight(t *testing.T) {
	w := Window{From: "23:00", To: "07:00"}
	assert.NoError(t, w.Validate())
	assert.True(t, w.Active(at(23, 0)))
	assert.True(t, w.Active(at(2, 30)))
	assert.True(t, w.Active(at(6, 59)))
	assert.False(t, w.Active(at(7, 0)))
	assert.False(t, w.Active(at(12, 0)))
	assert.False(t, w.Active(at(22, 59)))
}

func TestWindowSameDay(t *testing.T) {
	w := Window{From: "13:00", To: "14:30"}
	assert.True(t, w.Active(at(13, 0)))
	assert.True(t, w.Active(at(14, 29)))
	assert.False(t, w.Active(at(14, 30)))
	assert.False(t, w.Active(at(9, 0)))
}

func TestWindowInvalid(t *testing.T) {
	assert.Error(t, Window{From: "7h", To: "23:00"}.Validate())
	assert.Error(t, Window{From: "24:00", To: "23:00"}.Validate())
	assert.Error(t, Window{From: "23:00", To: "07:60"}.Validate())
	assert.False(t, Window{From: "08:00", To: "08:00"}.Active(at(8, 0)))
}
