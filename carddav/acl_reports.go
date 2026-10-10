package carddav

import (
	"net/http"
	"strings"

	"github.com/daptin/go-webdav"
	"github.com/daptin/go-webdav/internal"
)

func (h *Handler) handleACLReport(w http.ResponseWriter, r *http.Request, query *internal.ACLReportQuery) error {
	directory, ok := h.Backend.(webdav.DAVPrincipalBackend)
	if !ok {
		return internal.HTTPErrorf(http.StatusNotImplemented, "carddav: principal reports unavailable")
	}
	b := backend{Backend: h.Backend, Prefix: strings.TrimSuffix(h.Prefix, "/")}
	find := func(path string, requested *internal.PropFind) (*internal.Response, error) {
		copy := r.Clone(r.Context())
		copy.URL.Path = path
		result, err := b.PropFind(copy, requested, internal.DepthZero)
		if err != nil {
			return nil, err
		}
		if len(result.Responses) != 1 {
			return nil, internal.HTTPErrorf(http.StatusNotFound, "carddav: principal unavailable")
		}
		return &result.Responses[0], nil
	}
	if query.XMLName.Local == "expand-property" {
		return internal.ServeExpandPropertyReport(w, r, query, b.Prefix+"/", find)
	}
	current, err := h.Backend.CurrentUserPrincipal(r.Context())
	if err != nil {
		return err
	}
	info, err := directory.DAVPrincipal(r.Context(), current)
	if err != nil {
		return err
	}
	var acl *AddressBookACL
	if b.resourceTypeAtPath(r.URL.Path) == resourceTypeAddressBook {
		if aclBackend, ok := h.Backend.(AddressBookACLBackend); ok {
			acl, err = aclBackend.GetAddressBookACL(r.Context(), r.URL.Path)
			if err != nil {
				return err
			}
		}
	}
	if query.XMLName.Local == "acl-principal-prop-set" && acl == nil {
		return internal.HTTPErrorf(http.StatusMethodNotAllowed, "carddav: ACL report requires an address book")
	}
	owner := ""
	var entries []AddressBookACE
	if acl != nil {
		owner, entries = acl.Owner, acl.Entries
	}
	paths, err := internal.ACLReportPaths(query, r.URL.Path, b.Prefix+"/", current, owner, entries,
		info.Memberships, func(term string) ([]string, error) {
			found, err := directory.SearchDAVPrincipals(r.Context(), b.Prefix+"/", term, 100)
			if err != nil {
				return nil, err
			}
			paths := make([]string, 0, len(found))
			for _, principal := range found {
				paths = append(paths, principal.Path)
			}
			return paths, nil
		})
	if err != nil {
		return err
	}
	return internal.ServePrincipalReport(w, r, query, paths, find)
}
