package carddav

import (
	"context"
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/daptin/go-webdav/internal"
)

type AddressBookACE = internal.GrantACLEntry
type AddressBookACL = internal.GrantACL

type AddressBookACLBackend interface {
	GetAddressBookACL(context.Context, string) (*AddressBookACL, error)
	ReplaceAddressBookACL(context.Context, string, []AddressBookACE) error
	GetAddressBookGroupPrincipal(context.Context, string) error
	GetAddressBookAccountPrincipal(context.Context, string) error
}

func supportedAddressBookACLPrivileges() *internal.SupportedPrivilegeSet {
	return internal.SupportedACLPrivileges([]xml.Name{
		{Space: internal.Namespace, Local: "read"},
		{Space: internal.Namespace, Local: "write-content"},
		{Space: internal.Namespace, Local: "write-properties"},
		{Space: internal.Namespace, Local: "bind"},
		{Space: internal.Namespace, Local: "unbind"},
		{Space: internal.Namespace, Local: "write-acl"},
	})
}

func (h *Handler) handleACL(w http.ResponseWriter, r *http.Request) error {
	b := backend{Backend: h.Backend, Prefix: strings.TrimSuffix(h.Prefix, "/")}
	if b.resourceTypeAtPath(r.URL.Path) != resourceTypeAddressBook {
		return internal.HTTPErrorf(http.StatusMethodNotAllowed, "carddav: ACL requires an address book")
	}
	aclBackend, ok := h.Backend.(AddressBookACLBackend)
	if !ok {
		return internal.HTTPErrorf(http.StatusMethodNotAllowed, "carddav: ACL unavailable")
	}
	entries, err := internal.ParseGrantACL(r)
	if err != nil {
		return err
	}
	if err := aclBackend.ReplaceAddressBookACL(r.Context(), r.URL.Path, entries); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}
