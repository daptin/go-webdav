package caldav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

type queryConformanceBackend struct{ testBackend }

func (b queryConformanceBackend) QueryCalendarObjects(ctx context.Context, path string, query *CalendarQuery) ([]CalendarObject, error) {
	return Filter(query, b.objectMap[path])
}

func TestTaskAndJournalTimeRange(t *testing.T) {
	start := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	for _, test := range []struct {
		name  string
		kind  string
		props map[string]string
		want  bool
	}{
		{"task due in range", ical.CompToDo, map[string]string{ical.PropDue: "20261010T120000Z"}, true},
		{"task due outside range", ical.CompToDo, map[string]string{ical.PropDue: "20261012T120000Z"}, false},
		{"task with no dates", ical.CompToDo, nil, true},
		{"journal at instant", ical.CompJournal, map[string]string{ical.PropDateTimeStart: "20261010T120000Z"}, true},
		{"journal outside range", ical.CompJournal, map[string]string{ical.PropDateTimeStart: "20261012T120000Z"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			component := ical.NewComponent(test.kind)
			for name, value := range test.props {
				parsed, err := time.Parse("20060102T150405Z", value)
				if err != nil {
					t.Fatal(err)
				}
				component.Props.SetDateTime(name, parsed)
			}
			got, err := matchCompTimeRange(start, end, component)
			if err != nil || got != test.want {
				t.Fatalf("match=%v err=%v want=%v", got, err, test.want)
			}
		})
	}
}

func TestRecurringTaskAndEventRangeBoundaries(t *testing.T) {
	start := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	task := ical.NewComponent(ical.CompToDo)
	task.Props.SetDateTime(ical.PropDateTimeStart, start.AddDate(0, 0, -2).Add(12*time.Hour))
	task.Props.SetDateTime(ical.PropDue, start.AddDate(0, 0, -2).Add(13*time.Hour))
	rule := ical.NewProp(ical.PropRecurrenceRule)
	rule.Value = "FREQ=DAILY;COUNT=4"
	task.Props.Set(rule)
	if matched, err := matchCompTimeRange(start, end, task); err != nil || !matched {
		t.Fatalf("recurring task match=%v err=%v", matched, err)
	}
	event := ical.NewComponent(ical.CompEvent)
	event.Props.SetDateTime(ical.PropDateTimeStart, start)
	if matched, err := matchCompTimeRange(start, end, event); err != nil || !matched {
		t.Fatalf("event at inclusive start match=%v err=%v", matched, err)
	}
}

func TestMissingComponentFilter(t *testing.T) {
	calendar := ical.NewCalendar()
	calendar.Children = append(calendar.Children, ical.NewComponent(ical.CompEvent))
	if matched, err := matchCompFilter(CompFilter{Name: ical.CompToDo, IsNotDefined: true}, calendar.Component); err != nil || !matched {
		t.Fatalf("missing VTODO match=%v err=%v", matched, err)
	}
}

func TestTextMatchCollations(t *testing.T) {
	for _, test := range []struct {
		collation string
		want      bool
	}{
		{"", true},
		{"i;ascii-casemap", true},
		{"i;octet", false},
	} {
		if got := matchTextMatch(TextMatch{Text: "MEETING", Collation: test.collation}, "Team meeting"); got != test.want {
			t.Fatalf("collation=%q match=%v want=%v", test.collation, got, test.want)
		}
	}
}

func TestCalendarMultigetPartialData(t *testing.T) {
	calendar := ical.NewCalendar()
	calendar.Props.SetText(ical.PropVersion, "2.0")
	calendar.Props.SetText(ical.PropProductID, "-//Daptin//Test//EN")
	event := ical.NewComponent(ical.CompEvent)
	event.Props.SetText(ical.PropUID, "partial-test")
	event.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	event.Props.SetText(ical.PropSummary, "Visible summary")
	event.Props.SetText(ical.PropLocation, "Hidden location")
	calendar.Children = append(calendar.Children, event)
	path := "/user/calendars/team/item.ics"
	handler := Handler{Backend: testBackend{calendars: []Calendar{{Path: "/user/calendars/team/"}},
		objectMap: map[string][]CalendarObject{"/user/calendars/team/": {{Path: path, Data: calendar}}}}}
	request := httptest.NewRequest("REPORT", "/user/calendars/team/", strings.NewReader(
		`<C:calendar-multiget xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:D="DAV:"><D:prop><C:calendar-data><C:comp name="VCALENDAR"><C:prop name="VERSION"/><C:comp name="VEVENT"><C:prop name="SUMMARY"/></C:comp></C:comp></C:calendar-data></D:prop><D:href>`+path+`</D:href></C:calendar-multiget>`))
	request.Header.Set("Content-Type", "application/xml")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMultiStatus || !strings.Contains(recorder.Body.String(), "Visible summary") || strings.Contains(recorder.Body.String(), "Hidden location") {
		t.Fatalf("partial REPORT status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if event.Props.Get(ical.PropLocation) == nil {
		t.Fatal("REPORT modified the stored calendar")
	}
}

func TestCalendarDataSelectionNoValueAndFoldedLine(t *testing.T) {
	calendar := ical.NewCalendar()
	calendar.Props.SetText(ical.PropVersion, "2.0")
	calendar.Props.SetText(ical.PropProductID, "-//Daptin//Test//EN")
	event := ical.NewComponent(ical.CompEvent)
	event.Props.SetText(ical.PropUID, "partial-test")
	event.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	event.Props.SetText(ical.PropSummary, strings.Repeat("Calendar summary ", 12))
	event.Props.SetText(ical.PropLocation, "Hidden location")
	calendar.Children = append(calendar.Children, event)
	selected, err := projectCalendarData(calendar, &calendarDataReq{Comp: &comp{Name: ical.CompCalendar, Comp: []comp{{Name: ical.CompEvent, Prop: []prop{{Name: ical.PropSummary}, {Name: ical.PropUID, NoValue: "yes"}}}}}})
	if err != nil || !strings.Contains(string(selected), strings.Repeat("Calendar summary ", 12)) ||
		!strings.Contains(string(selected), "UID:\r\n") || strings.Contains(string(selected), "partial-test") ||
		strings.Contains(string(selected), "Hidden location") {
		t.Fatalf("partial calendar=%q err=%v", selected, err)
	}
}

func TestCalendarDataIsReportOnly(t *testing.T) {
	calendar := ical.NewCalendar()
	calendar.Props.SetText(ical.PropVersion, "2.0")
	calendar.Props.SetText(ical.PropProductID, "-//Daptin//Test//EN")
	event := ical.NewComponent(ical.CompEvent)
	event.Props.SetText(ical.PropUID, "propfind-test")
	event.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	calendar.Children = append(calendar.Children, event)
	path := "/user/calendars/team/item.ics"
	handler := Handler{Backend: testBackend{calendars: []Calendar{{Path: "/user/calendars/team/"}},
		objectMap: map[string][]CalendarObject{"/user/calendars/team/": {{Path: path, Data: calendar}}}}}
	request := httptest.NewRequest("PROPFIND", path, strings.NewReader(
		`<D:propfind xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:D="DAV:"><D:prop><C:calendar-data/></D:prop></D:propfind>`))
	request.Header.Set("Content-Type", "application/xml")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMultiStatus || !strings.Contains(recorder.Body.String(), "404 Not Found") || strings.Contains(recorder.Body.String(), "BEGIN:VCALENDAR") {
		t.Fatalf("PROPFIND calendar-data status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCalendarQueryTaskRangeAndTextCollation(t *testing.T) {
	calendar := ical.NewCalendar()
	calendar.Props.SetText(ical.PropVersion, "2.0")
	calendar.Props.SetText(ical.PropProductID, "-//Daptin//Test//EN")
	task := ical.NewComponent(ical.CompToDo)
	task.Props.SetText(ical.PropUID, "task-query-test")
	task.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	task.Props.SetDateTime(ical.PropDue, time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	task.Props.SetText(ical.PropSummary, "Team Meeting")
	calendar.Children = append(calendar.Children, task)
	path := "/user/calendars/team/"
	handler := Handler{Backend: queryConformanceBackend{testBackend{calendars: []Calendar{{Path: path}},
		objectMap: map[string][]CalendarObject{path: {{Path: path + "task.ics", Data: calendar}}}}}}
	for _, test := range []struct {
		collation string
		want      bool
	}{
		{"", true},
		{"i;octet", false},
	} {
		request := httptest.NewRequest("REPORT", path, strings.NewReader(
			`<C:calendar-query xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:D="DAV:"><D:prop><D:getetag/></D:prop><C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VTODO"><C:time-range start="20261010T000000Z" end="20261011T000000Z"/><C:prop-filter name="SUMMARY"><C:text-match collation="`+test.collation+`">meeting</C:text-match></C:prop-filter></C:comp-filter></C:comp-filter></C:filter></C:calendar-query>`))
		request.Header.Set("Content-Type", "application/xml")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		found := strings.Contains(recorder.Body.String(), "task.ics")
		if recorder.Code != http.StatusMultiStatus || found != test.want {
			t.Fatalf("collation=%q status=%d found=%v body=%s", test.collation, recorder.Code, found, recorder.Body.String())
		}
	}
}
