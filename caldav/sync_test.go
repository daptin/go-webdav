package caldav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type syncBackend struct{ testBackend }

func (b *syncBackend) CalendarSyncToken(context.Context, string) (string, error) {
	return "urn:test:current", nil
}

func (b *syncBackend) SyncCalendarObjects(_ context.Context, _, token string, limit int) (*CalendarSyncResult, error) {
	if token == "invalid" {
		return nil, ErrInvalidSyncToken
	}
	if limit != 1 {
		return nil, ErrInvalidSyncToken
	}
	return &CalendarSyncResult{Token: "urn:test:next", Changes: []CalendarSyncChange{{
		Path:   "/caldav/owner/calendars/team/one.ics",
		Object: &CalendarObject{Path: "/caldav/owner/calendars/team/one.ics", ETag: "abc"},
	}}}, nil
}

func TestCalendarSyncReportAndProperties(t *testing.T) {
	b := &syncBackend{testBackend: testBackend{calendars: []Calendar{{Path: "/caldav/owner/calendars/team/", Name: "team"}}}}
	h := Handler{Backend: b, Prefix: "/caldav"}
	request := httptest.NewRequest("PROPFIND", "http://example.test/caldav/owner/calendars/team/", strings.NewReader(
		`<D:propfind xmlns:D="DAV:"><D:prop><D:sync-token/><D:supported-report-set/></D:prop></D:propfind>`))
	request.Header.Set("Content-Type", "application/xml")
	request.Header.Set("Depth", "0")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusMultiStatus || !strings.Contains(response.Body.String(), "urn:test:current") ||
		!strings.Contains(response.Body.String(), "sync-collection") {
		t.Fatalf("sync discovery status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest("REPORT", "http://example.test/caldav/owner/calendars/team/", strings.NewReader(
		`<D:sync-collection xmlns:D="DAV:"><D:sync-token/><D:sync-level>1</D:sync-level><D:limit><D:nresults>1</D:nresults></D:limit><D:prop><D:getetag/></D:prop></D:sync-collection>`))
	request.Header.Set("Content-Type", "application/xml")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusMultiStatus || !strings.Contains(response.Body.String(), "urn:test:next") ||
		!strings.Contains(response.Body.String(), "one.ics") || !strings.Contains(response.Body.String(), "abc") {
		t.Fatalf("sync REPORT status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest("REPORT", "http://example.test/caldav/owner/calendars/team/", strings.NewReader(
		`<D:sync-collection xmlns:D="DAV:"><D:sync-token>invalid</D:sync-token><D:sync-level>1</D:sync-level><D:prop><D:getetag/></D:prop></D:sync-collection>`))
	request.Header.Set("Content-Type", "application/xml")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "valid-sync-token") {
		t.Fatalf("invalid token status=%d body=%s", response.Code, response.Body.String())
	}
}
