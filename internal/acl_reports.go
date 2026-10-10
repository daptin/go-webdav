package internal

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// ACLReportQuery contains the report bodies required by RFC 3744. Report
// execution obtains principal paths from the application, never from XML.
type ACLReportQuery struct {
	XMLName           xml.Name
	Prop              *Prop              `xml:"DAV: prop"`
	Self              *struct{}          `xml:"DAV: self"`
	PrincipalProperty *PrincipalProperty `xml:"DAV: principal-property"`
	Searches          []PrincipalSearch  `xml:"DAV: property-search"`
	Apply             *struct{}          `xml:"DAV: apply-to-principal-collection-set"`
	Expand            []ExpandProperty   `xml:"DAV: property"`
}

type PrincipalProperty struct {
	Names []RawXMLValue `xml:",any"`
}

type PrincipalSearch struct {
	Prop  Prop   `xml:"DAV: prop"`
	Match string `xml:"DAV: match"`
}

type ExpandProperty struct {
	Name      string           `xml:"name,attr"`
	Namespace string           `xml:"namespace,attr"`
	Children  []ExpandProperty `xml:"DAV: property"`
}

func IsACLReport(name xml.Name) bool {
	if name.Space != Namespace {
		return false
	}
	switch name.Local {
	case "acl-principal-prop-set", "principal-match", "principal-property-search", "principal-search-property-set", "expand-property":
		return true
	}
	return false
}

func ServePrincipalReport(w http.ResponseWriter, r *http.Request, query *ACLReportQuery, paths []string,
	propfind func(string, *PropFind) (*Response, error)) error {
	if depth := r.Header.Get("Depth"); depth != "" && depth != "0" {
		return HTTPErrorf(http.StatusBadRequest, "DAV: principal report requires Depth: 0")
	}
	if query.XMLName == (xml.Name{Space: Namespace, Local: "principal-search-property-set"}) {
		return ServeXML(w).Encode(&struct {
			XMLName    xml.Name `xml:"DAV: principal-search-property-set"`
			Properties []struct {
				Prop struct {
					DisplayName *struct{} `xml:"DAV: displayname"`
				} `xml:"DAV: prop"`
				Description ACLPrivilegeDescription `xml:"DAV: description"`
			} `xml:"DAV: principal-search-property"`
		}{Properties: []struct {
			Prop struct {
				DisplayName *struct{} `xml:"DAV: displayname"`
			} `xml:"DAV: prop"`
			Description ACLPrivilegeDescription `xml:"DAV: description"`
		}{{Prop: struct {
			DisplayName *struct{} `xml:"DAV: displayname"`
		}{DisplayName: &struct{}{}}, Description: ACLPrivilegeDescription{Lang: "en", Text: "Display name"}}}})
	}
	seen := make(map[string]bool, len(paths))
	unique := make([]string, 0, len(paths))
	for _, path := range paths {
		if path != "" && !seen[path] {
			seen[path] = true
			unique = append(unique, path)
		}
	}
	sort.Strings(unique)
	requested := &PropFind{Prop: query.Prop}
	if requested.Prop == nil {
		requested.PropName = &struct{}{}
	}
	responses := make([]Response, 0, len(unique))
	for _, path := range unique {
		response, err := propfind(path, requested)
		if err != nil {
			responses = append(responses, *NewErrorResponse(path, err))
			continue
		}
		responses = append(responses, *response)
	}
	return ServeMultiStatus(w, NewMultiStatus(responses...))
}

func ACLReportPaths(query *ACLReportQuery, requestPath, prefix, current, owner string, aces []GrantACLEntry,
	memberships []string, search func(string) ([]string, error)) ([]string, error) {
	switch query.XMLName.Local {
	case "acl-principal-prop-set":
		paths := make([]string, 0, len(aces))
		for _, ace := range aces {
			paths = append(paths, ace.Principal)
		}
		return paths, nil
	case "principal-match":
		if query.Self != nil {
			if requestPath == prefix || requestPath == current {
				return append([]string{current}, memberships...), nil
			}
			for _, member := range memberships {
				if member == requestPath {
					return []string{requestPath}, nil
				}
			}
			return nil, nil
		}
		if query.PrincipalProperty == nil || len(query.PrincipalProperty.Names) != 1 {
			return nil, HTTPErrorf(http.StatusBadRequest, "DAV: principal-match needs one principal property")
		}
		name, ok := query.PrincipalProperty.Names[0].XMLName()
		if !ok || name != (xml.Name{Space: Namespace, Local: "owner"}) {
			return nil, nil
		}
		if owner == current || stringIn(memberships, owner) {
			return []string{requestPath}, nil
		}
		return nil, nil
	case "principal-property-search":
		if len(query.Searches) == 0 {
			return nil, HTTPErrorf(http.StatusBadRequest, "DAV: principal search needs a criterion")
		}
		var matches []string
		for index, criterion := range query.Searches {
			if len(criterion.Prop.Raw) != 1 {
				return nil, HTTPErrorf(http.StatusBadRequest, "DAV: principal search needs one property")
			}
			name, ok := criterion.Prop.Raw[0].XMLName()
			if !ok || name != DisplayNameName {
				return nil, nil
			}
			found, err := search(strings.TrimSpace(criterion.Match))
			if err != nil {
				return nil, err
			}
			if index == 0 {
				matches = found
			} else {
				filtered := matches[:0]
				for _, path := range matches {
					if stringIn(found, path) {
						filtered = append(filtered, path)
					}
				}
				matches = filtered
			}
		}
		return matches, nil
	case "principal-search-property-set":
		return nil, nil
	}
	return nil, HTTPErrorf(http.StatusBadRequest, "DAV: unsupported ACL report")
}

func stringIn(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func ServeExpandPropertyReport(w http.ResponseWriter, r *http.Request, query *ACLReportQuery, prefix string,
	propfind func(string, *PropFind) (*Response, error)) error {
	if depth := r.Header.Get("Depth"); depth != "" && depth != "0" {
		return HTTPErrorf(http.StatusBadRequest, "DAV: expand-property requires Depth: 0")
	}
	remaining := 100
	var expand func(string, []ExpandProperty, int) (*Response, error)
	expand = func(requestPath string, properties []ExpandProperty, level int) (*Response, error) {
		if level > 8 || remaining < 1 {
			return nil, HTTPErrorf(http.StatusInsufficientStorage, "DAV: property expansion limit reached")
		}
		remaining--
		names := make([]xml.Name, 0, len(properties))
		for _, property := range properties {
			if property.Name == "" {
				return nil, HTTPErrorf(http.StatusBadRequest, "DAV: expanded property has no name")
			}
			space := property.Namespace
			if space == "" {
				space = Namespace
			}
			names = append(names, xml.Name{Space: space, Local: property.Name})
		}
		response, err := propfind(requestPath, NewPropNamePropFind(names...))
		if err != nil {
			return nil, err
		}
		for statIndex := range response.PropStats {
			stat := &response.PropStats[statIndex]
			if stat.Status.Code != http.StatusOK {
				continue
			}
			for rawIndex := range stat.Prop.Raw {
				raw := &stat.Prop.Raw[rawIndex]
				encoded, err := xml.Marshal(raw)
				if err != nil {
					return nil, err
				}
				start, err := xml.NewDecoder(bytes.NewReader(encoded)).Token()
				if err != nil {
					continue
				}
				name, ok := start.(xml.StartElement)
				if !ok {
					continue
				}
				for _, property := range properties {
					space := property.Namespace
					if space == "" {
						space = Namespace
					}
					if name.Name != (xml.Name{Space: space, Local: property.Name}) || len(property.Children) == 0 {
						continue
					}
					var value struct {
						Hrefs []string `xml:"DAV: href"`
					}
					if err := xml.Unmarshal(encoded, &value); err != nil {
						return nil, err
					}
					children := make([]RawXMLValue, 0, len(value.Hrefs))
					for _, href := range value.Hrefs {
						target, err := url.Parse(href)
						if err != nil || target.Path == "" || target.RawQuery != "" || target.Fragment != "" ||
							(target.Host != "" && !strings.EqualFold(target.Host, r.Host)) ||
							!strings.HasPrefix(target.Path, prefix) {
							continue
						}
						resolved, err := expand(target.Path, property.Children, level+1)
						if err != nil {
							resolved = NewErrorResponse(target.Path, err)
						}
						child, err := EncodeRawXMLElement(resolved)
						if err != nil {
							return nil, err
						}
						children = append(children, *child)
					}
					stat.Prop.Raw[rawIndex] = *NewRawXMLElement(name.Name, nil, children)
				}
			}
		}
		return response, nil
	}
	response, err := expand(r.URL.Path, query.Expand, 0)
	if err != nil {
		return err
	}
	return ServeMultiStatus(w, NewMultiStatus(*response))
}
