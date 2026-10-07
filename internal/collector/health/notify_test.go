package health

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/kolapsis/pi-dashboard/internal/collector"
)

var _ collector.Notifier = (*Collector)(nil)

func TestNotify(t *testing.T) {
	prev := Data{Sites: []Site{{Name: "a", Up: true}, {Name: "b", Up: false}}}
	assert.Nil(t, Notify(prev, prev))
	assert.Equal(t, []string{"a est down"}, Notify(prev, Data{Sites: []Site{{Name: "a"}, {Name: "b"}}}))
	assert.Nil(t, Notify(prev, Data{Sites: []Site{{Name: "a", Up: true}, {Name: "b", Up: true}}}), "recovery is silent")
	assert.Nil(t, Notify(prev, Data{Sites: []Site{{Name: "c"}}}), "a site that was never up is silent")
	assert.Nil(t, Notify(nil, prev))
}
