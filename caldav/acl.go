package caldav

import (
	"context"
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/daptin/go-webdav/internal"
)

type CalendarACE = internal.GrantACLEntry
type CalendarACL = internal.GrantACL

// CalendarACLBackend keeps principal and group grants in the application.
type CalendarACLBackend interface {
	GetCalendarACL(context.Context, string) (*CalendarACL, error)
	ReplaceCalendarACL(context.Context, string, []CalendarACE) error
	GetGroupPrincipal(context.Context, string) error
}

type CalendarAccountPrincipalBackend interface {
	GetAccountPrincipal(context.Context, string) error
}

type aclRestrictionsXML = internal.ACLRestrictions
type principalCollectionSetXML = internal.PrincipalCollectionSet

func supportedCalendarACLPrivileges() *internal.SupportedPrivilegeSet {
	return internal.SupportedACLPrivileges([]xml.Name{
		{Space: internal.Namespace, Local: "read"},
		{Space: namespace, Local: "read-free-busy"},
		{Space: internal.Namespace, Local: "write-content"},
		{Space: internal.Namespace, Local: "write-properties"},
		{Space: internal.Namespace, Local: "bind"},
		{Space: internal.Namespace, Local: "unbind"},
		{Space: internal.Namespace, Local: "write-acl"},
	})
}

func (h *Handler) handleACL(w http.ResponseWriter, r *http.Request) error {
	b := backend{Backend: h.Backend, Prefix: strings.TrimSuffix(h.Prefix, "/")}
	if b.resourceTypeAtPath(r.URL.Path) != resourceTypeCalendar {
		return internal.HTTPErrorf(http.StatusMethodNotAllowed, "caldav: ACL requires a calendar collection")
	}
	aclBackend, ok := h.Backend.(CalendarACLBackend)
	if !ok {
		return internal.HTTPErrorf(http.StatusMethodNotAllowed, "caldav: ACL unavailable")
	}
	entries, err := internal.ParseGrantACL(r)
	if err != nil {
		return err
	}
	if err := aclBackend.ReplaceCalendarACL(r.Context(), r.URL.Path, entries); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func calendarACLProperty(acl *CalendarACL) interface{} {
	return internal.GrantACLProperty(acl)
}
