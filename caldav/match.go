package caldav

import (
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

// Filter returns the filtered list of calendar objects matching the provided query.
// A nil query will return the full list of calendar objects.
func Filter(query *CalendarQuery, cos []CalendarObject) ([]CalendarObject, error) {
	if query == nil {
		// FIXME: should we always return a copy of the provided slice?
		return cos, nil
	}

	var out []CalendarObject
	for _, co := range cos {
		ok, err := Match(query.CompFilter, &co)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		// TODO properties are not currently filtered even if requested
		out = append(out, co)
	}
	return out, nil
}

// Match reports whether the provided CalendarObject matches the query.
func Match(query CompFilter, co *CalendarObject) (matched bool, err error) {
	if co.Data == nil || co.Data.Component == nil {
		panic("request to process empty calendar object")
	}
	return match(query, co.Data.Component)
}

func match(filter CompFilter, comp *ical.Component) (bool, error) {
	if comp.Name != filter.Name {
		return filter.IsNotDefined, nil
	}

	if !filter.Start.IsZero() || !filter.End.IsZero() {
		match, err := matchCompTimeRange(filter.Start, filter.End, comp)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}
	for _, compFilter := range filter.Comps {
		match, err := matchCompFilter(compFilter, comp)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}
	for _, propFilter := range filter.Props {
		match, err := matchPropFilter(propFilter, comp)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}
	return true, nil
}

func matchCompFilter(filter CompFilter, comp *ical.Component) (bool, error) {
	if filter.IsNotDefined {
		for _, child := range comp.Children {
			if child.Name == filter.Name {
				return false, nil
			}
		}
		return true, nil
	}
	var matches []*ical.Component

	for _, child := range comp.Children {
		match, err := match(filter, child)
		if err != nil {
			return false, err
		} else if match {
			matches = append(matches, child)
		}
	}
	if len(matches) == 0 {
		return filter.IsNotDefined, nil
	}
	return true, nil
}

func matchPropFilter(filter PropFilter, comp *ical.Component) (bool, error) {
	fields := comp.Props.Values(filter.Name)
	if len(fields) == 0 {
		return filter.IsNotDefined, nil
	}
	if filter.IsNotDefined {
		return false, nil
	}
	for i := range fields {
		field := &fields[i]
		matches := true
		for _, paramFilter := range filter.ParamFilter {
			if !matchParamFilter(paramFilter, field) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		if !filter.Start.IsZero() || !filter.End.IsZero() {
			match, err := matchPropTimeRange(filter.Start, filter.End, field)
			if err != nil {
				return false, err
			}
			if !match {
				continue
			}
		}
		if filter.TextMatch != nil && !matchTextMatch(*filter.TextMatch, field.Value) {
			continue
		}
		return true, nil
	}
	return false, nil
}

func matchCompTimeRange(start, end time.Time, comp *ical.Component) (bool, error) {
	// See https://datatracker.ietf.org/doc/html/rfc4791#section-9.9
	if comp.Name == ical.CompToDo || comp.Name == ical.CompJournal {
		rset, err := comp.RecurrenceSet(start.Location())
		if err != nil {
			return false, err
		}
		if rset != nil {
			initial, err := comp.Props.DateTime(ical.PropDateTimeStart, start.Location())
			if err != nil {
				return false, err
			}
			// A recurring task can begin before the requested interval and
			// remain due within it. Check the immediately preceding instance.
			candidates := []time.Time{rset.Before(start, true), rset.After(start, false)}
			for _, instance := range candidates {
				if instance.IsZero() || !end.IsZero() && !instance.Before(end) {
					continue
				}
				shifted := *comp
				shifted.Props = make(ical.Props, len(comp.Props))
				for name, values := range comp.Props {
					shifted.Props[name] = values
				}
				if prop := comp.Props.Get(ical.PropDateTimeStart); prop != nil && prop.ValueType() == ical.ValueDate {
					shifted.Props.SetDate(ical.PropDateTimeStart, instance)
				} else {
					shifted.Props.SetDateTime(ical.PropDateTimeStart, instance)
				}
				if due := comp.Props.Get(ical.PropDue); due != nil {
					originalDue, err := due.DateTime(start.Location())
					if err != nil {
						return false, err
					}
					if due.ValueType() == ical.ValueDate {
						shifted.Props.SetDate(ical.PropDue, originalDue.Add(instance.Sub(initial)))
					} else {
						shifted.Props.SetDateTime(ical.PropDue, originalDue.Add(instance.Sub(initial)))
					}
				}
				match, err := matchTaskOrJournalTimeRange(start, end, &shifted)
				if err != nil || match {
					return match, err
				}
			}
			return false, nil
		}
		return matchTaskOrJournalTimeRange(start, end, comp)
	}

	// evaluate recurring events
	rset, err := comp.RecurrenceSet(start.Location())
	if err != nil {
		return false, err
	}
	if comp.Name != ical.CompEvent {
		if rset != nil {
			return len(rset.Between(start, end, true)) > 0, nil
		}
		return false, nil
	}
	event := ical.Event{comp}

	eventStart, err := event.DateTimeStart(start.Location())
	if err != nil {
		return false, err
	}
	eventEnd, err := event.DateTimeEnd(end.Location())
	if err != nil {
		return false, err
	}
	if rset != nil {
		// RecurrenceSet contains starts, while a VEVENT time range matches
		// any occurrence that overlaps [start, end). The latest start at or
		// before start is the only earlier occurrence we need to inspect.
		previous := rset.Before(start, true)
		if !previous.IsZero() {
			duration := eventEnd.Sub(eventStart)
			if duration > 0 && previous.Add(duration).After(start) || duration == 0 && previous.Equal(start) {
				return true, nil
			}
		}
		next := rset.After(start, false)
		return !next.IsZero() && (end.IsZero() || next.Before(end)), nil
	}

	if !eventEnd.After(eventStart) {
		return atOrBefore(start, eventStart) && after(end, eventStart), nil
	}
	return before(start, eventEnd) && after(end, eventStart), nil
}

func matchTaskOrJournalTimeRange(start, end time.Time, comp *ical.Component) (bool, error) {
	location := start.Location()
	get := func(name string) (time.Time, error) { return comp.Props.DateTime(name, location) }
	dtstart, err := get(ical.PropDateTimeStart)
	if err != nil {
		return false, err
	}
	if comp.Name == ical.CompJournal {
		if dtstart.IsZero() {
			return false, nil
		}
		if prop := comp.Props.Get(ical.PropDateTimeStart); prop != nil && strings.EqualFold(prop.Params.Get("VALUE"), "DATE") {
			return before(start, dtstart.AddDate(0, 0, 1)) && after(end, dtstart), nil
		}
		return atOrBefore(start, dtstart) && after(end, dtstart), nil
	}
	due, err := get(ical.PropDue)
	if err != nil {
		return false, err
	}
	completed, err := get(ical.PropCompleted)
	if err != nil {
		return false, err
	}
	created, err := get(ical.PropCreated)
	if err != nil {
		return false, err
	}
	if !dtstart.IsZero() {
		if duration := comp.Props.Get(ical.PropDuration); duration != nil {
			value, err := duration.Duration()
			if err != nil {
				return false, err
			}
			finish := dtstart.Add(value)
			return atOrBefore(start, finish) && (after(end, dtstart) || atOrAfter(end, finish)), nil
		}
		if !due.IsZero() {
			return (before(start, due) || atOrBefore(start, dtstart)) &&
				(after(end, dtstart) || atOrAfter(end, due)), nil
		}
		return atOrBefore(start, dtstart) && after(end, dtstart), nil
	}
	if !due.IsZero() {
		return before(start, due) && atOrAfter(end, due), nil
	}
	if !completed.IsZero() {
		if !created.IsZero() {
			return (atOrBefore(start, created) || atOrBefore(start, completed)) &&
				(atOrAfter(end, created) || atOrAfter(end, completed)), nil
		}
		return atOrBefore(start, completed) && atOrAfter(end, completed), nil
	}
	if !created.IsZero() {
		return after(end, created), nil
	}
	return true, nil
}

func before(bound, value time.Time) bool     { return bound.IsZero() || bound.Before(value) }
func after(bound, value time.Time) bool      { return bound.IsZero() || bound.After(value) }
func atOrBefore(bound, value time.Time) bool { return bound.IsZero() || !bound.After(value) }
func atOrAfter(bound, value time.Time) bool  { return bound.IsZero() || !bound.Before(value) }

func matchPropTimeRange(start, end time.Time, field *ical.Prop) (bool, error) {
	// See https://datatracker.ietf.org/doc/html/rfc4791#section-9.9

	ptime, err := field.DateTime(start.Location())
	if err != nil {
		return false, err
	}
	if atOrBefore(start, ptime) && after(end, ptime) {
		return true, nil
	}
	return false, nil
}

func matchParamFilter(filter ParamFilter, field *ical.Prop) bool {
	values := field.Params.Values(filter.Name)
	if len(values) == 0 {
		return filter.IsNotDefined
	}
	if filter.IsNotDefined {
		return false
	}
	if filter.TextMatch != nil {
		for _, value := range values {
			if matchTextMatch(*filter.TextMatch, value) {
				return true
			}
		}
		return false
	}
	return true
}

func matchTextMatch(txt TextMatch, value string) bool {
	needle := txt.Text
	if txt.Collation == "" || txt.Collation == "i;ascii-casemap" {
		value = asciiLower(value)
		needle = asciiLower(needle)
	}
	match := strings.Contains(value, needle)
	if txt.NegateCondition {
		match = !match
	}
	return match
}

func asciiLower(value string) string {
	bytes := []byte(value)
	for i, b := range bytes {
		if b >= 'A' && b <= 'Z' {
			bytes[i] += 'a' - 'A'
		}
	}
	return string(bytes)
}
