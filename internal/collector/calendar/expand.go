package calendar

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

const untitled = "(sans titre)"

var (
	unescape = strings.NewReplacer(`\\`, `\`, `\,`, `,`, `\;`, `;`, `\n`, " ", `\N`, " ")

	dateProps = []string{
		ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropRecurrenceID,
		ical.PropExceptionDates, ical.PropRecurrenceDates,
	}
)

type window struct {
	now      time.Time
	today    time.Time
	tomorrow time.Time
	end      time.Time
	loc      *time.Location
}

func newWindow(now time.Time, days int, loc *time.Location) window {
	n := now.In(loc)
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	return window{now: now, today: today, tomorrow: today.AddDate(0, 0, 1), end: today.AddDate(0, 0, days), loc: loc}
}

func (w window) overlaps(s span) bool {
	return s.start.Before(w.end) && (s.end.After(w.today) || !s.start.Before(w.today))
}

type span struct {
	start, end time.Time
	allDay     bool
}

func (s span) startingAt(start time.Time) span {
	if s.allDay {
		days := int(math.Round(s.end.Sub(s.start).Hours() / 24))
		return span{start: start, end: start.AddDate(0, 0, days), allDay: true}
	}
	return span{start: start, end: start.Add(s.end.Sub(s.start))}
}

type occurrence struct {
	uid string
	at  int64
}

type expander struct {
	win        window
	calendar   string
	overridden map[occurrence]bool
	skipped    int
	firstErr   error
}

func (x *expander) events(cal *ical.Calendar) []Event {
	var comps []*ical.Component
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		sanitize(child)
		comps = append(comps, child)
		if p := child.Props.Get(ical.PropRecurrenceID); p != nil {
			if at, err := p.DateTime(x.win.loc); err == nil {
				x.overridden[occurrence{uid: uid(child), at: at.Unix()}] = true
			}
		}
	}
	var out []Event
	for _, comp := range comps {
		out = append(out, x.expand(comp)...)
	}
	return out
}

func (x *expander) expand(comp *ical.Component) []Event {
	if cancelled(comp) {
		return nil
	}
	base, err := x.span(comp)
	if err != nil {
		x.skip(err)
		return nil
	}
	spans := []span{base}
	if comp.Props.Get(ical.PropRecurrenceRule) != nil && comp.Props.Get(ical.PropRecurrenceID) == nil {
		if spans, err = x.recurrences(comp, base); err != nil {
			x.skip(err)
			return nil
		}
	}
	title := text(comp, ical.PropSummary)
	if title == "" {
		title = untitled
	}
	where := text(comp, ical.PropLocation)
	var out []Event
	for _, s := range spans {
		if !x.win.overlaps(s) {
			continue
		}
		out = append(out, Event{
			Title:      title,
			Start:      s.start.In(x.win.loc),
			End:        s.end.In(x.win.loc),
			AllDay:     s.allDay,
			InProgress: !s.allDay && !x.win.now.Before(s.start) && x.win.now.Before(s.end),
			Calendar:   x.calendar,
			Location:   where,
		})
	}
	return out
}

func (x *expander) span(comp *ical.Component) (span, error) {
	startProp := comp.Props.Get(ical.PropDateTimeStart)
	if startProp == nil {
		return span{}, errors.New("missing DTSTART")
	}
	start, err := startProp.DateTime(x.win.loc)
	if err != nil {
		return span{}, fmt.Errorf("DTSTART: %w", err)
	}
	s := span{start: start, end: start, allDay: startProp.ValueType() == ical.ValueDate}
	endProp, durProp := comp.Props.Get(ical.PropDateTimeEnd), comp.Props.Get(ical.PropDuration)
	switch {
	case endProp != nil:
		if s.end, err = endProp.DateTime(x.win.loc); err != nil {
			return span{}, fmt.Errorf("DTEND: %w", err)
		}
	case durProp != nil:
		d, derr := durProp.Duration()
		if derr != nil {
			return span{}, fmt.Errorf("DURATION: %w", derr)
		}
		if s.allDay {
			s.end = s.start.AddDate(0, 0, int(d/(24*time.Hour)))
		} else {
			s.end = s.start.Add(d)
		}
	}
	switch {
	case s.allDay && !s.end.After(s.start):
		s.end = s.start.AddDate(0, 0, 1)
	case s.end.Before(s.start):
		s.end = s.start
	}
	return s, nil
}

func (x *expander) recurrences(comp *ical.Component, base span) ([]span, error) {
	set, err := comp.RecurrenceSet(x.win.loc)
	if err != nil {
		return nil, err
	}
	from := x.win.today.Add(-(base.end.Sub(base.start) + 24*time.Hour))
	id := uid(comp)
	var out []span
	for _, start := range set.Between(from, x.win.end, true) {
		if !x.overridden[occurrence{uid: id, at: start.Unix()}] {
			out = append(out, base.startingAt(start.In(x.win.loc)))
		}
	}
	return out, nil
}

func (x *expander) skip(err error) {
	if x.skipped == 0 {
		x.firstErr = err
	}
	x.skipped++
}

// sanitize makes a VEVENT parseable by go-ical: unknown TZIDs become local time and multi-valued EXDATE/RDATE are split.
func sanitize(comp *ical.Component) {
	for _, name := range dateProps {
		props := comp.Props[name]
		if len(props) == 0 {
			continue
		}
		out := make([]ical.Prop, 0, len(props))
		for _, p := range props {
			if tz := p.Params.Get(ical.ParamTimezoneID); tz != "" {
				if _, err := time.LoadLocation(tz); err != nil {
					p.Params.Del(ical.ParamTimezoneID)
				}
			}
			for v := range strings.SplitSeq(p.Value, ",") {
				out = append(out, ical.Prop{Name: p.Name, Params: p.Params, Value: v})
			}
		}
		comp.Props[name] = out
	}
}

func cancelled(comp *ical.Component) bool {
	p := comp.Props.Get(ical.PropStatus)
	return p != nil && strings.EqualFold(strings.TrimSpace(p.Value), string(ical.EventCancelled))
}

func uid(comp *ical.Component) string {
	if p := comp.Props.Get(ical.PropUID); p != nil {
		return strings.TrimSpace(p.Value)
	}
	return ""
}

func text(comp *ical.Component, name string) string {
	p := comp.Props.Get(name)
	if p == nil {
		return ""
	}
	return strings.Join(strings.Fields(unescape.Replace(p.Value)), " ")
}
