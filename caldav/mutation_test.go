package caldav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type mutationBackend struct {
	testBackend
	properties []CalendarPropertyUpdate
	source     string
	dest       string
	recursive  bool
}

func (b *mutationBackend) PatchCalendarProperties(_ context.Context, _ string, changes []CalendarPropertyUpdate) error {
	b.properties = changes
	return nil
}

func (b *mutationBackend) CopyCalendarObject(_ context.Context, source, dest string, _ bool) (bool, error) {
	b.source, b.dest = source, dest
	return true, nil
}

func (b *mutationBackend) MoveCalendarObject(_ context.Context, source, dest string, _ bool) (bool, error) {
	b.source, b.dest = source, dest
	return false, nil
}

func (b *mutationBackend) CopyCalendarCollection(_ context.Context, source, dest string, recursive, _ bool) (bool, error) {
	b.source, b.dest, b.recursive = source, dest, recursive
	return true, nil
}

func (b *mutationBackend) MoveCalendarCollection(_ context.Context, source, dest string, _ bool) (bool, error) {
	b.source, b.dest = source, dest
	return false, nil
}

func TestCalendarMutationMethods(t *testing.T) {
	b := &mutationBackend{testBackend: testBackend{calendars: []Calendar{{Path: "/caldav/owner/calendars/team/"}}}}
	h := Handler{Backend: b, Prefix: "/caldav"}
	patch := `<D:propertyupdate xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:set><D:prop><D:displayname>Team</D:displayname><C:calendar-description>Notes</C:calendar-description></D:prop></D:set></D:propertyupdate>`
	request := httptest.NewRequest("PROPPATCH", "http://example.test/caldav/owner/calendars/team/", strings.NewReader(patch))
	request.Header.Set("Content-Type", "application/xml")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusMultiStatus || len(b.properties) != 2 {
		t.Fatalf("PROPPATCH status=%d properties=%#v body=%s", response.Code, b.properties, response.Body.String())
	}
	request = httptest.NewRequest("COPY", "http://example.test/caldav/owner/calendars/team/one.ics", nil)
	request.Header.Set("Destination", "http://example.test/caldav/owner/calendars/team/two.ics")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || b.dest != "/caldav/owner/calendars/team/two.ics" {
		t.Fatalf("COPY status=%d destination=%q body=%s", response.Code, b.dest, response.Body.String())
	}
	request = httptest.NewRequest("MOVE", "http://example.test/caldav/owner/calendars/team/one.ics", nil)
	request.Header.Set("Destination", "http://elsewhere.test/caldav/owner/calendars/team/two.ics")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("foreign MOVE status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest("COPY", "http://example.test/caldav/owner/calendars/team/", nil)
	request.Header.Set("Destination", "http://example.test/caldav/owner/calendars/copy/")
	request.Header.Set("Depth", "0")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || b.recursive || b.dest != "/caldav/owner/calendars/copy/" {
		t.Fatalf("collection COPY status=%d recursive=%t destination=%q body=%s", response.Code, b.recursive, b.dest, response.Body.String())
	}
	request = httptest.NewRequest("MOVE", "http://example.test/caldav/owner/calendars/team/", nil)
	request.Header.Set("Destination", "http://example.test/caldav/owner/calendars/moved/")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || b.dest != "/caldav/owner/calendars/moved/" {
		t.Fatalf("collection MOVE status=%d destination=%q body=%s", response.Code, b.dest, response.Body.String())
	}
}
