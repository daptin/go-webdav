package caldav

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type aclTestBackend struct {
	testBackend
	entries []CalendarACE
	writes  int
}

func (b *aclTestBackend) GetCalendarACL(context.Context, string) (*CalendarACL, error) {
	return &CalendarACL{Owner: "/user/", Entries: b.entries, CanWrite: true}, nil
}

func (b *aclTestBackend) ReplaceCalendarACL(_ context.Context, _ string, entries []CalendarACE) error {
	b.entries = entries
	b.writes++
	return nil
}

func (b *aclTestBackend) GetGroupPrincipal(context.Context, string) error { return nil }

func TestCalendarACLMethodAndProperties(t *testing.T) {
	backend := &aclTestBackend{testBackend: testBackend{calendars: []Calendar{{Path: "/user/calendars/team/"}}}}
	handler := Handler{Backend: backend}
	request := httptest.NewRequest("ACL", "/user/calendars/team/", strings.NewReader(
		`<D:acl xmlns:D="DAV:"><D:ace><D:principal><D:href>/user/delegate/</D:href></D:principal><D:grant><D:privilege><D:read/></D:privilege></D:grant></D:ace></D:acl>`))
	request.Header.Set("Content-Type", "application/xml")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || backend.writes != 1 || len(backend.entries) != 1 ||
		backend.entries[0].Principal != "/user/delegate/" || len(backend.entries[0].Privileges) != 1 ||
		backend.entries[0].Privileges[0] != (xml.Name{Space: "DAV:", Local: "read"}) {
		t.Fatalf("ACL status=%d entries=%#v body=%s", response.Code, backend.entries, response.Body.String())
	}
	request = httptest.NewRequest("PROPFIND", "/user/calendars/team/", strings.NewReader(
		`<D:propfind xmlns:D="DAV:"><D:prop><D:acl/><D:owner/><D:acl-restrictions/><D:supported-privilege-set/><D:current-user-privilege-set/></D:prop></D:propfind>`))
	request.Header.Set("Content-Type", "application/xml")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	for _, value := range []string{"/user/delegate/", "grant-only", "supported-privilege-set", "write-acl"} {
		if response.Code != http.StatusMultiStatus || !strings.Contains(response.Body.String(), value) {
			t.Fatalf("PROPFIND status=%d missing %s: %s", response.Code, value, response.Body.String())
		}
	}
}

func TestCalendarACLRejectsUnsupportedACE(t *testing.T) {
	backend := &aclTestBackend{testBackend: testBackend{calendars: []Calendar{{Path: "/user/calendars/team/"}}}}
	handler := Handler{Backend: backend}
	for _, body := range []string{
		`<D:acl xmlns:D="DAV:"><D:ace><D:principal><D:href>/user/delegate/</D:href></D:principal><D:deny><D:privilege><D:read/></D:privilege></D:deny></D:ace></D:acl>`,
		`<D:acl xmlns:D="DAV:"><D:ace><D:principal><D:href>/user/delegate/</D:href></D:principal><D:grant><D:privilege><D:read/></D:privilege></D:grant><D:invert/></D:ace></D:acl>`,
	} {
		request := httptest.NewRequest("ACL", "/user/calendars/team/", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/xml")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 400 || backend.writes != 0 {
			t.Fatalf("unsupported ACL status=%d writes=%d", response.Code, backend.writes)
		}
	}
}
