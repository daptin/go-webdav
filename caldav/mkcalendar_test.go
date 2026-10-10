package caldav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type mkcalendarBackend struct {
	testBackend
	created []*Calendar
}

func (b *mkcalendarBackend) CreateCalendar(_ context.Context, calendar *Calendar) error {
	copy := *calendar
	b.created = append(b.created, &copy)
	return nil
}

func TestMkcalendarCreatesWithPropertiesAtomically(t *testing.T) {
	b := &mkcalendarBackend{}
	h := Handler{Backend: b, Prefix: "/caldav"}
	request := httptest.NewRequest("MKCALENDAR", "/caldav/owner/calendars/new/",
		strings.NewReader(`<C:mkcalendar xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:D="DAV:"><D:set><D:prop><D:displayname>Planning</D:displayname><C:calendar-description>Team calendar</C:calendar-description></D:prop></D:set></C:mkcalendar>`))
	request.Header.Set("Content-Type", "application/xml")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || len(b.created) != 1 ||
		b.created[0].DisplayName != "Planning" || b.created[0].Description != "Team calendar" {
		t.Fatalf("status=%d created=%+v body=%s", response.Code, b.created, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("missing no-cache header: %v", response.Header())
	}
	request = httptest.NewRequest("MKCALENDAR", "/caldav/owner/calendars/invalid/",
		strings.NewReader(`<C:mkcalendar xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:D="DAV:"><D:set><D:prop><C:calendar-timezone>unknown</C:calendar-timezone></D:prop></D:set></C:mkcalendar>`))
	request.Header.Set("Content-Type", "application/xml")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || len(b.created) != 1 {
		t.Fatalf("unsupported property created a calendar: status=%d count=%d", response.Code, len(b.created))
	}
}
