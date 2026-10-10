package caldav

import (
	"context"
	"encoding/xml"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/daptin/go-webdav/internal"
)

// CalendarACE is one grant-only, non-inherited calendar collection ACE.
type CalendarACE struct {
	Principal  string
	Privileges []xml.Name
	Protected  bool
}

// CalendarACL describes the editable calendar grants and the protected owner.
type CalendarACL struct {
	Owner    string
	Entries  []CalendarACE
	CanWrite bool
}

// CalendarACLBackend is optional. The backend remains the authority for
// principals, permissions, and atomic replacement of collection grants.
type CalendarACLBackend interface {
	GetCalendarACL(context.Context, string) (*CalendarACL, error)
	ReplaceCalendarACL(context.Context, string, []CalendarACE) error
	GetGroupPrincipal(context.Context, string) error
}

type aclXML struct {
	XMLName xml.Name        `xml:"DAV: acl"`
	ACE     []aceXML        `xml:"DAV: ace"`
	Other   []unknownACLXML `xml:",any"`
}

type aceXML struct {
	Principal principalXML    `xml:"DAV: principal"`
	Grant     *grantXML       `xml:"DAV: grant,omitempty"`
	Deny      *struct{}       `xml:"DAV: deny,omitempty"`
	Protected *struct{}       `xml:"DAV: protected,omitempty"`
	Inherited *struct{}       `xml:"DAV: inherited,omitempty"`
	Other     []unknownACLXML `xml:",any"`
}

type principalXML struct {
	Href  []string        `xml:"DAV: href"`
	Other []unknownACLXML `xml:",any"`
}

type grantXML struct {
	Privileges []privilegeXML  `xml:"DAV: privilege"`
	Other      []unknownACLXML `xml:",any"`
}

type privilegeXML struct {
	Names []namedPrivilege `xml:",any"`
}

type unknownACLXML struct{ XMLName xml.Name }

type aclRestrictionsXML struct {
	XMLName   xml.Name  `xml:"DAV: acl-restrictions"`
	GrantOnly *struct{} `xml:"DAV: grant-only"`
	NoInvert  *struct{} `xml:"DAV: no-invert"`
}

type principalCollectionSetXML struct {
	XMLName xml.Name `xml:"DAV: principal-collection-set"`
	Href    string   `xml:"DAV: href"`
}

type supportedPrivilegeSetXML struct {
	XMLName    xml.Name                `xml:"DAV: supported-privilege-set"`
	Privileges []supportedPrivilegeXML `xml:"DAV: supported-privilege"`
}

type supportedPrivilegeXML struct {
	XMLName   xml.Name                `xml:"DAV: supported-privilege"`
	Privilege privilegeXML            `xml:"DAV: privilege"`
	Abstract  *struct{}               `xml:"DAV: abstract,omitempty"`
	Children  []supportedPrivilegeXML `xml:"DAV: supported-privilege,omitempty"`
}

func supportedCalendarACLPrivileges() *supportedPrivilegeSetXML {
	all := supportedPrivilegeXML{Privilege: privilegeXML{Names: []namedPrivilege{{XMLName: xml.Name{Space: internal.Namespace, Local: "all"}}}}, Abstract: &struct{}{}}
	for _, name := range []xml.Name{
		{Space: internal.Namespace, Local: "read"},
		{Space: namespace, Local: "read-free-busy"},
		{Space: internal.Namespace, Local: "write-content"},
		{Space: internal.Namespace, Local: "bind"},
		{Space: internal.Namespace, Local: "unbind"},
		{Space: internal.Namespace, Local: "write-acl"},
	} {
		all.Children = append(all.Children, supportedPrivilegeXML{Privilege: privilegeXML{Names: []namedPrivilege{{XMLName: name}}}})
	}
	return &supportedPrivilegeSetXML{Privileges: []supportedPrivilegeXML{all}}
}

type namedPrivilege struct {
	XMLName xml.Name
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
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/xml" && mediaType != "text/xml" {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: expected XML ACL body")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid ACL body")
	}
	var request aclXML
	if err := xml.Unmarshal(body, &request); err != nil {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid ACL XML")
	}
	if request.XMLName != (xml.Name{Space: internal.Namespace, Local: "acl"}) || len(request.Other) != 0 {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: expected DAV:acl")
	}
	entries := make([]CalendarACE, 0, len(request.ACE))
	for _, ace := range request.ACE {
		if len(ace.Principal.Href) != 1 || ace.Principal.Href[0] == "" || len(ace.Principal.Other) != 0 || ace.Grant == nil ||
			len(ace.Grant.Other) != 0 || len(ace.Other) != 0 || ace.Deny != nil || ace.Inherited != nil || len(ace.Grant.Privileges) == 0 {
			return internal.HTTPErrorf(http.StatusForbidden, "caldav: unsupported ACE")
		}
		entry := CalendarACE{Principal: ace.Principal.Href[0], Protected: ace.Protected != nil}
		for _, privilege := range ace.Grant.Privileges {
			if len(privilege.Names) != 1 {
				return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid privilege")
			}
			entry.Privileges = append(entry.Privileges, privilege.Names[0].XMLName)
		}
		entries = append(entries, entry)
	}
	if err := aclBackend.ReplaceCalendarACL(r.Context(), r.URL.Path, entries); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (p privilegeXML) MarshalXML(encoder *xml.Encoder, start xml.StartElement) error {
	if err := encoder.EncodeToken(start); err != nil {
		return err
	}
	for _, name := range p.Names {
		child := xml.StartElement{Name: name.XMLName}
		if err := encoder.EncodeToken(child); err != nil {
			return err
		}
		if err := encoder.EncodeToken(child.End()); err != nil {
			return err
		}
	}
	return encoder.EncodeToken(start.End())
}

func calendarACLProperty(acl *CalendarACL) *aclXML {
	property := &aclXML{}
	for _, entry := range acl.Entries {
		ace := aceXML{Principal: principalXML{Href: []string{entry.Principal}}, Grant: &grantXML{}}
		if entry.Protected {
			ace.Protected = &struct{}{}
		}
		for _, name := range entry.Privileges {
			ace.Grant.Privileges = append(ace.Grant.Privileges, privilegeXML{Names: []namedPrivilege{{XMLName: name}}})
		}
		property.ACE = append(property.ACE, ace)
	}
	return property
}
