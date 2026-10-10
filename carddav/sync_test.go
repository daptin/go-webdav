package carddav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type syncBackend struct{ testBackend }

func (*syncBackend) AddressBookSyncToken(context.Context, string) (string, error) {
	return "urn:test:current", nil
}

func (*syncBackend) SyncAddressObjects(_ context.Context, _, token string, limit int) (*AddressBookSyncResult, error) {
	if token == "invalid" {
		return nil, ErrInvalidSyncToken
	}
	if limit != 1 {
		return nil, ErrInvalidSyncToken
	}
	return &AddressBookSyncResult{Token: "urn:test:next", Changes: []AddressBookSyncChange{
		{Path: "/carddav/owner/addressbooks/team/one.vcf", Object: &AddressObject{Path: "/carddav/owner/addressbooks/team/one.vcf", ETag: "abc"}},
		{Path: "/carddav/owner/addressbooks/team/gone.vcf"},
	}}, nil
}

func TestAddressBookSyncReportAndProperties(t *testing.T) {
	b := &syncBackend{}
	h := Handler{Backend: b, Prefix: "/carddav"}
	url := "/carddav/owner/addressbooks/team/"
	ctx := context.WithValue(context.Background(), addressBookPathKey, url)
	ctx = context.WithValue(ctx, currentUserPrincipalKey, "/carddav/owner/")
	request := httptest.NewRequest("PROPFIND", url, strings.NewReader(
		`<D:propfind xmlns:D="DAV:"><D:prop><D:sync-token/><D:supported-report-set/></D:prop></D:propfind>`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/xml")
	request.Header.Set("Depth", "0")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusMultiStatus || !strings.Contains(response.Body.String(), "urn:test:current") ||
		!strings.Contains(response.Body.String(), "sync-collection") {
		t.Fatalf("sync discovery status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest("REPORT", url, strings.NewReader(
		`<D:sync-collection xmlns:D="DAV:"><D:sync-token/><D:sync-level>1</D:sync-level><D:limit><D:nresults>1</D:nresults></D:limit><D:prop><D:getetag/></D:prop></D:sync-collection>`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/xml")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusMultiStatus || !strings.Contains(response.Body.String(), "urn:test:next") ||
		!strings.Contains(response.Body.String(), "one.vcf") || !strings.Contains(response.Body.String(), "gone.vcf") ||
		!strings.Contains(response.Body.String(), "404 Not Found") {
		t.Fatalf("sync REPORT status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest("REPORT", url, strings.NewReader(
		`<D:sync-collection xmlns:D="DAV:"><D:sync-token>invalid</D:sync-token><D:sync-level>1</D:sync-level><D:prop><D:getetag/></D:prop></D:sync-collection>`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/xml")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "valid-sync-token") {
		t.Fatalf("invalid token status=%d body=%s", response.Code, response.Body.String())
	}
}
