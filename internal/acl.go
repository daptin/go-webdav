package internal

import (
	"encoding/xml"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

func ACLPreconditionError(name string) error {
	return &HTTPError{Code: http.StatusForbidden, Err: &Error{Raw: []RawXMLValue{
		*NewRawXMLElement(xml.Name{Space: Namespace, Local: name}, nil, nil),
	}}}
}

// GrantACLEntry is one editable or protected, non-inherited grant ACE.
type GrantACLEntry struct {
	Principal  string
	Privileges []xml.Name
	Protected  bool
}

type GrantACL struct {
	Owner    string
	Entries  []GrantACLEntry
	CanWrite bool
}

type aclDocument struct {
	XMLName xml.Name      `xml:"DAV: acl"`
	ACE     []aclEntryXML `xml:"DAV: ace"`
	Other   []unknownACL  `xml:",any"`
}

type aclEntryXML struct {
	Principal aclPrincipalXML `xml:"DAV: principal"`
	Grant     *aclGrantXML    `xml:"DAV: grant,omitempty"`
	Deny      *struct{}       `xml:"DAV: deny,omitempty"`
	Protected *struct{}       `xml:"DAV: protected,omitempty"`
	Inherited *struct{}       `xml:"DAV: inherited,omitempty"`
	Other     []unknownACL    `xml:",any"`
}

type aclPrincipalXML struct {
	Href  []string     `xml:"DAV: href"`
	Other []unknownACL `xml:",any"`
}

type aclGrantXML struct {
	Privileges []aclPrivilegeXML `xml:"DAV: privilege"`
	Other      []unknownACL      `xml:",any"`
}

type aclPrivilegeXML struct {
	Names []aclPrivilegeName `xml:",any"`
}

type aclPrivilegeName struct{ XMLName xml.Name }
type unknownACL struct{ XMLName xml.Name }

func ParseGrantACL(r *http.Request) ([]GrantACLEntry, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/xml" && mediaType != "text/xml" {
		return nil, HTTPErrorf(http.StatusBadRequest, "DAV: expected XML ACL body")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		return nil, HTTPErrorf(http.StatusBadRequest, "DAV: invalid ACL body")
	}
	var request aclDocument
	if err := xml.Unmarshal(body, &request); err != nil {
		return nil, HTTPErrorf(http.StatusBadRequest, "DAV: invalid ACL XML")
	}
	if request.XMLName != (xml.Name{Space: Namespace, Local: "acl"}) || len(request.Other) != 0 {
		return nil, HTTPErrorf(http.StatusBadRequest, "DAV: expected DAV:acl")
	}
	entries := make([]GrantACLEntry, 0, len(request.ACE))
	for _, ace := range request.ACE {
		if ace.Deny != nil {
			return nil, ACLPreconditionError("grant-only")
		}
		if ace.Inherited != nil {
			return nil, ACLPreconditionError("no-inherited-ace-conflict")
		}
		if len(ace.Principal.Href) != 1 || ace.Principal.Href[0] == "" || len(ace.Principal.Other) != 0 {
			return nil, ACLPreconditionError("allowed-principal")
		}
		if ace.Grant == nil || len(ace.Grant.Other) != 0 || len(ace.Other) != 0 || len(ace.Grant.Privileges) == 0 {
			return nil, ACLPreconditionError("no-ace-conflict")
		}
		href := ace.Principal.Href[0]
		parsed, err := url.Parse(href)
		if err != nil {
			return nil, HTTPErrorf(http.StatusBadRequest, "DAV: invalid ACL principal URL")
		}
		if parsed.IsAbs() {
			if parsed.Scheme != "http" && parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, r.Host) {
				return nil, ACLPreconditionError("allowed-principal")
			}
			href = parsed.Path
		}
		entry := GrantACLEntry{Principal: href, Protected: ace.Protected != nil}
		for _, privilege := range ace.Grant.Privileges {
			if len(privilege.Names) != 1 {
				return nil, HTTPErrorf(http.StatusBadRequest, "DAV: invalid privilege")
			}
			entry.Privileges = append(entry.Privileges, privilege.Names[0].XMLName)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (p aclPrivilegeXML) MarshalXML(encoder *xml.Encoder, start xml.StartElement) error {
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

func GrantACLProperty(acl *GrantACL) interface{} {
	property := &aclDocument{}
	for _, entry := range acl.Entries {
		ace := aclEntryXML{Principal: aclPrincipalXML{Href: []string{entry.Principal}}, Grant: &aclGrantXML{}}
		if entry.Protected {
			ace.Protected = &struct{}{}
		}
		for _, name := range entry.Privileges {
			ace.Grant.Privileges = append(ace.Grant.Privileges, aclPrivilegeXML{Names: []aclPrivilegeName{{XMLName: name}}})
		}
		property.ACE = append(property.ACE, ace)
	}
	return property
}

type ACLRestrictions struct {
	XMLName   xml.Name  `xml:"DAV: acl-restrictions"`
	GrantOnly *struct{} `xml:"DAV: grant-only"`
	NoInvert  *struct{} `xml:"DAV: no-invert"`
}

type PrincipalCollectionSet struct {
	XMLName xml.Name `xml:"DAV: principal-collection-set"`
	Href    string   `xml:"DAV: href"`
}

type PrincipalHrefSet struct {
	XMLName xml.Name
	Hrefs   []string `xml:"DAV: href"`
}

func PrincipalProperties(path, displayName string, members, memberships []string) map[xml.Name]PropFindFunc {
	name := func(local string) xml.Name { return xml.Name{Space: Namespace, Local: local} }
	return map[xml.Name]PropFindFunc{
		DisplayNameName:           PropFindValue(&DisplayName{Name: displayName}),
		name("principal-URL"):     PropFindValue(&PrincipalHrefSet{XMLName: name("principal-URL"), Hrefs: []string{path}}),
		name("alternate-URI-set"): PropFindValue(&PrincipalHrefSet{XMLName: name("alternate-URI-set")}),
		name("group-member-set"):  PropFindValue(&PrincipalHrefSet{XMLName: name("group-member-set"), Hrefs: members}),
		name("group-membership"):  PropFindValue(&PrincipalHrefSet{XMLName: name("group-membership"), Hrefs: memberships}),
	}
}

type SupportedPrivilegeSet struct {
	XMLName    xml.Name             `xml:"DAV: supported-privilege-set"`
	Privileges []SupportedPrivilege `xml:"DAV: supported-privilege"`
}

type SupportedPrivilege struct {
	XMLName     xml.Name                `xml:"DAV: supported-privilege"`
	Privilege   aclPrivilegeXML         `xml:"DAV: privilege"`
	Abstract    *struct{}               `xml:"DAV: abstract,omitempty"`
	Description ACLPrivilegeDescription `xml:"DAV: description"`
	Children    []SupportedPrivilege    `xml:"DAV: supported-privilege,omitempty"`
}

type ACLPrivilegeDescription struct {
	Lang string `xml:"http://www.w3.org/XML/1998/namespace lang,attr"`
	Text string `xml:",chardata"`
}

func aclPrivilegeDescription(name xml.Name) ACLPrivilegeDescription {
	return ACLPrivilegeDescription{Lang: "en", Text: name.Local}
}

func SupportedACLPrivileges(names []xml.Name) *SupportedPrivilegeSet {
	allName := xml.Name{Space: Namespace, Local: "all"}
	all := SupportedPrivilege{Privilege: aclPrivilegeXML{Names: []aclPrivilegeName{{XMLName: allName}}}, Abstract: &struct{}{}, Description: aclPrivilegeDescription(allName)}
	writeName := xml.Name{Space: Namespace, Local: "write"}
	write := SupportedPrivilege{Privilege: aclPrivilegeXML{Names: []aclPrivilegeName{{XMLName: writeName}}}, Abstract: &struct{}{}, Description: aclPrivilegeDescription(writeName)}
	for _, name := range names {
		privilege := SupportedPrivilege{Privilege: aclPrivilegeXML{Names: []aclPrivilegeName{{XMLName: name}}}, Description: aclPrivilegeDescription(name)}
		switch name {
		case (xml.Name{Space: Namespace, Local: "read"}):
			for _, local := range []string{"read-acl", "read-current-user-privilege-set"} {
				childName := xml.Name{Space: Namespace, Local: local}
				privilege.Children = append(privilege.Children, SupportedPrivilege{Privilege: aclPrivilegeXML{Names: []aclPrivilegeName{{XMLName: childName}}},
					Abstract: &struct{}{}, Description: aclPrivilegeDescription(childName)})
			}
			all.Children = append(all.Children, privilege)
		case (xml.Name{Space: Namespace, Local: "write-content"}), (xml.Name{Space: Namespace, Local: "write-properties"}),
			(xml.Name{Space: Namespace, Local: "bind"}), (xml.Name{Space: Namespace, Local: "unbind"}):
			write.Children = append(write.Children, privilege)
		default:
			all.Children = append(all.Children, privilege)
		}
	}
	if len(write.Children) != 0 {
		all.Children = append(all.Children, write)
	}
	return &SupportedPrivilegeSet{Privileges: []SupportedPrivilege{all}}
}
