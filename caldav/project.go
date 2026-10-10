package caldav

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/daptin/go-webdav/internal"
	"github.com/emersion/go-ical"
)

// projectCalendarData filters the valid stored encoding. A selected REPORT
// response can omit required iCalendar properties, so it cannot be re-encoded
// through go-ical's validating encoder.
func projectCalendarData(calendar *ical.Calendar, request *calendarDataReq) ([]byte, error) {
	if request != nil {
		if len(request.Other) != 0 {
			return nil, internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid calendar-data selection")
		}
		if request.Expand != nil || request.LimitRecurrenceSet != nil || request.LimitFreebusySet != nil {
			return nil, internal.HTTPErrorf(http.StatusNotImplemented, "caldav: recurrence data selection is unavailable")
		}
		if request.Comp != nil {
			if err := validateCalendarSelection(request.Comp); err != nil {
				return nil, err
			}
		}
	}
	var encoded bytes.Buffer
	if err := ical.NewEncoder(&encoded).Encode(calendar); err != nil {
		return nil, err
	}
	if request == nil || request.Comp == nil {
		return encoded.Bytes(), nil
	}
	if !strings.EqualFold(request.Comp.Name, ical.CompCalendar) {
		return nil, internal.HTTPErrorf(http.StatusBadRequest, "caldav: calendar-data must select VCALENDAR")
	}
	type frame struct {
		selection *comp
		include   bool
	}
	var stack []frame
	var out bytes.Buffer
	for _, value := range unfoldCalendarLines(encoded.Bytes()) {
		line := []byte(value)
		switch {
		case strings.HasPrefix(value, "BEGIN:"):
			name := strings.TrimPrefix(value, "BEGIN:")
			next := frame{include: len(stack) == 0, selection: request.Comp}
			if len(stack) != 0 {
				parent := stack[len(stack)-1]
				next = frame{include: parent.include && parent.selection == nil}
				if parent.include && parent.selection != nil {
					if parent.selection.Allcomp != nil {
						next.include = true
					} else {
						for i := range parent.selection.Comp {
							if strings.EqualFold(parent.selection.Comp[i].Name, name) {
								next = frame{selection: &parent.selection.Comp[i], include: true}
								break
							}
						}
					}
				}
			}
			stack = append(stack, next)
			if next.include {
				out.Write(line)
				out.WriteString("\r\n")
			}
		case strings.HasPrefix(value, "END:"):
			if len(stack) == 0 {
				return nil, internal.HTTPErrorf(http.StatusInternalServerError, "caldav: invalid encoded calendar")
			}
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if current.include {
				out.Write(line)
				out.WriteString("\r\n")
			}
		default:
			if len(stack) == 0 || !stack[len(stack)-1].include {
				continue
			}
			selection := stack[len(stack)-1].selection
			if selection == nil || selection.Allprop != nil {
				out.Write(line)
				out.WriteString("\r\n")
				continue
			}
			name := value
			if index := strings.IndexAny(name, ";:"); index >= 0 {
				name = name[:index]
			}
			for _, property := range selection.Prop {
				if !strings.EqualFold(name, property.Name) {
					continue
				}
				if property.NoValue == "yes" {
					if index := calendarValueSeparator(value); index >= 0 {
						line = line[:index+1]
					}
				}
				out.Write(line)
				out.WriteString("\r\n")
				break
			}
		}
	}
	return out.Bytes(), nil
}

func validateCalendarSelection(selection *comp) error {
	if selection.Name == "" || selection.Allprop != nil && len(selection.Prop) != 0 ||
		selection.Allcomp != nil && len(selection.Comp) != 0 {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid calendar component selection")
	}
	for _, property := range selection.Prop {
		if property.Name == "" || property.NoValue != "" && property.NoValue != "yes" && property.NoValue != "no" {
			return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid calendar property selection")
		}
	}
	for i := range selection.Comp {
		if err := validateCalendarSelection(&selection.Comp[i]); err != nil {
			return err
		}
	}
	return nil
}

func unfoldCalendarLines(encoded []byte) []string {
	physical := bytes.Split(bytes.TrimSuffix(encoded, []byte("\r\n")), []byte("\r\n"))
	lines := make([]string, 0, len(physical))
	for _, line := range physical {
		if len(line) != 0 && (line[0] == ' ' || line[0] == '\t') && len(lines) != 0 {
			lines[len(lines)-1] += string(line[1:])
		} else {
			lines = append(lines, string(line))
		}
	}
	return lines
}

func calendarValueSeparator(line string) int {
	quoted := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			quoted = !quoted
		case ':':
			if !quoted {
				return i
			}
		}
	}
	return -1
}
