package mail

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/kolapsis/pi-dashboard/internal/collector"
)

var _ collector.Notifier = (*Collector)(nil)

func TestNotify(t *testing.T) {
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	a := Item{From: "Alice", Subject: "Devis", At: at}
	b := Item{From: "Bob", Subject: "Re: Convention HOX – pièces complémentaires pour le dossier de financement", At: at.Add(time.Minute)}
	prev := Data{Unseen: 1, Items: []Item{a}}

	assert.Nil(t, Notify(prev, prev))
	assert.Equal(t, []string{"Mail de Bob · Re: Convention HOX – pièces complémentaires pour le dossier …"},
		Notify(prev, Data{Unseen: 2, Items: []Item{b, a}}))
	assert.Equal(t, []string{"Mail de Bob · Re: Convention HOX – pièces complémentaires pour le dossier …"},
		Notify(&prev, &Data{Unseen: 2, Items: []Item{b, a}}), "pointers are accepted")
	assert.Nil(t, Notify(prev, Data{Unseen: 0}), "read messages disappear silently")
	assert.Equal(t, []string{"2 nouveaux mails"}, Notify(prev, Data{Unseen: 3, Items: []Item{a}}), "unseen grows without listed items")
	assert.Equal(t, []string{"1 nouveau mail"}, Notify(prev, Data{Unseen: 2, Items: []Item{a}}))

	many := Data{Unseen: 5, Items: []Item{a}}
	for i := range 4 {
		many.Items = append(many.Items, Item{From: "X", Subject: "s", At: at.Add(time.Duration(i+1) * time.Hour)})
	}
	assert.Equal(t, []string{"4 nouveaux mails"}, Notify(prev, many))
	assert.Nil(t, Notify("nope", prev))
}
