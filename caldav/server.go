package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/daptin/go-webdav"
	"github.com/daptin/go-webdav/internal"
	"github.com/emersion/go-ical"
)

// TODO if nothing more Caldav-specific needs to be added this should be merged with carddav.PutAddressObjectOptions
type PutCalendarObjectOptions struct {
	// IfNoneMatch indicates that the client does not want to overwrite
	// an existing resource.
	IfNoneMatch webdav.ConditionalMatch
	// IfMatch provides the ETag of the resource that the client intends
	// to overwrite, can be ""
	IfMatch webdav.ConditionalMatch
}

// Backend is a CalDAV server backend.
type Backend interface {
	CalendarHomeSetPath(ctx context.Context, requestedPath string) (string, error)

	CreateCalendar(ctx context.Context, calendar *Calendar) error
	ListCalendars(ctx context.Context, requestedPath string) ([]Calendar, error)
	GetCalendar(ctx context.Context, path string) (*Calendar, error)

	GetCalendarObject(ctx context.Context, path string, req *CalendarCompRequest) (*CalendarObject, error)
	ListCalendarObjects(ctx context.Context, path string, req *CalendarCompRequest) ([]CalendarObject, error)
	QueryCalendarObjects(ctx context.Context, path string, query *CalendarQuery) ([]CalendarObject, error)
	FreeBusy(ctx context.Context, path string, start, end time.Time, includeChildren bool) (*ical.Calendar, error)
	PutCalendarObject(ctx context.Context, path string, calendar *ical.Calendar, opts *PutCalendarObjectOptions) (*CalendarObject, error)
	DeleteCalendarObject(ctx context.Context, path string) error

	webdav.UserPrincipalBackend
}

// Handler handles CalDAV HTTP requests. It can be used to create a CalDAV
// server.
type Handler struct {
	Backend Backend
	Prefix  string
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Backend == nil {
		http.Error(w, "caldav: no backend available", http.StatusInternalServerError)
		return
	}

	if r.URL.Path == "/.well-known/caldav" {
		principalPath, err := h.Backend.CurrentUserPrincipal(r.Context())
		if err != nil {
			http.Error(w, "caldav: failed to determine current user principal", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, principalPath, http.StatusPermanentRedirect)
		return
	}

	var err error
	switch r.Method {
	case "REPORT":
		err = h.handleReport(w, r)
	case "MKCALENDAR":
		err = h.handleMkcalendar(w, r)
	case "ACL":
		err = h.handleACL(w, r)
	default:
		b := backend{
			Backend: h.Backend,
			Prefix:  strings.TrimSuffix(h.Prefix, "/"),
		}
		hh := internal.Handler{Backend: &b}
		hh.ServeHTTP(w, r)
	}

	if err != nil {
		internal.ServeError(w, err)
	}
}

func (h *Handler) handleMkcalendar(w http.ResponseWriter, r *http.Request) error {
	b := backend{Backend: h.Backend, Prefix: strings.TrimSuffix(h.Prefix, "/")}
	if b.resourceTypeAtPath(r.URL.Path) != resourceTypeCalendar {
		return internal.HTTPErrorf(http.StatusForbidden, "caldav: calendar creation not allowed at given location")
	}
	cal := &Calendar{Path: r.URL.Path}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid MKCALENDAR body")
	}
	if len(body) != 0 {
		if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil ||
			(mediaType != "application/xml" && mediaType != "text/xml") {
			return internal.HTTPErrorf(http.StatusBadRequest, "caldav: expected XML MKCALENDAR body")
		}
		var request mkcalendarReq
		if err := xml.Unmarshal(body, &request); err != nil || len(request.Set) != 1 ||
			len(request.Other) != 0 || len(request.Set[0].Prop) != 1 || len(request.Set[0].Other) != 0 {
			return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid MKCALENDAR body")
		}
		seen := make(map[xml.Name]bool)
		for _, raw := range request.Set[0].Prop[0].Raw {
			name, ok := raw.XMLName()
			if !ok || seen[name] {
				return internal.HTTPErrorf(http.StatusBadRequest, "caldav: duplicate or invalid calendar property")
			}
			seen[name] = true
			var value struct {
				Text  string `xml:",chardata"`
				Inner string `xml:",innerxml"`
			}
			if err := raw.Decode(&value); err != nil || strings.Contains(value.Inner, "<") {
				return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid calendar property value")
			}
			switch name {
			case internal.DisplayNameName:
				cal.DisplayName = value.Text
			case calendarDescriptionName:
				cal.Description = value.Text
			case calendarColorName:
				if value.Text == "" {
					return internal.HTTPErrorf(http.StatusBadRequest, "caldav: empty calendar color")
				}
				cal.Color = value.Text
			default:
				return internal.HTTPErrorf(http.StatusForbidden, "caldav: unsupported calendar property")
			}
		}
	}
	if err := h.Backend.CreateCalendar(r.Context(), cal); err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusCreated)
	return nil
}

func (h *Handler) handleReport(w http.ResponseWriter, r *http.Request) error {
	var report reportReq
	if err := internal.DecodeXMLRequest(r, &report); err != nil {
		return err
	}

	if report.Query != nil {
		return h.handleQuery(r, w, report.Query)
	} else if report.Multiget != nil {
		return h.handleMultiget(r.Context(), w, report.Multiget)
	} else if report.Sync != nil {
		return h.handleSync(r, w, report.Sync)
	} else if report.ACL != nil {
		return h.handleACLReport(w, r, report.ACL)
	} else if report.FreeBusy != nil {
		if len(report.FreeBusy.TimeRanges) != 1 {
			return internal.HTTPErrorf(http.StatusBadRequest, "caldav: free-busy-query requires one time-range")
		}
		rangeValue := report.FreeBusy.TimeRanges[0]
		start, end := time.Time(rangeValue.Start), time.Time(rangeValue.End)
		if start.IsZero() || end.IsZero() || !start.Before(end) {
			return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid free-busy time-range")
		}
		depth := internal.DepthZero
		if header := r.Header.Get("Depth"); header != "" {
			parsed, err := internal.ParseDepth(header)
			if err != nil {
				return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid Depth header")
			}
			depth = parsed
		}
		calendar, err := h.Backend.FreeBusy(r.Context(), r.URL.Path, start, end, depth != internal.DepthZero)
		if err != nil {
			return err
		}
		var encoded bytes.Buffer
		if err := ical.NewEncoder(&encoded).Encode(calendar); err != nil {
			return err
		}
		w.Header().Set("Content-Type", ical.MIMEType)
		w.WriteHeader(http.StatusOK)
		_, err = w.Write(encoded.Bytes())
		return err
	}
	return internal.HTTPErrorf(http.StatusBadRequest, "caldav: unsupported REPORT request")
}

func (h *Handler) handleSync(r *http.Request, w http.ResponseWriter, query *internal.SyncCollectionQuery) error {
	b := backend{Backend: h.Backend, Prefix: strings.TrimSuffix(h.Prefix, "/")}
	if b.resourceTypeAtPath(r.URL.Path) != resourceTypeCalendar {
		return internal.HTTPErrorf(http.StatusForbidden, "caldav: sync requires a calendar collection")
	}
	if depth := r.Header.Get("Depth"); depth != "" && depth != "0" {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: sync requires Depth: 0")
	}
	backend, ok := h.Backend.(CalendarSyncBackend)
	if !ok {
		return internal.HTTPErrorf(http.StatusNotImplemented, "caldav: collection sync not implemented")
	}
	if query.SyncLevel != "1" && query.SyncLevel != "infinite" || query.Prop == nil {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid sync report")
	}
	limit := 1000
	if query.Limit != nil {
		if query.Limit.NResults == 0 {
			return internal.HTTPErrorf(http.StatusBadRequest, "caldav: sync limit must be positive")
		}
		if query.Limit.NResults < uint(limit) {
			limit = int(query.Limit.NResults)
		}
	}
	result, err := backend.SyncCalendarObjects(r.Context(), r.URL.Path, query.SyncToken, limit)
	if errors.Is(err, ErrInvalidSyncToken) {
		w.Header().Set("Content-Type", "application/xml; charset=\"utf-8\"")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(xml.Header))
		return xml.NewEncoder(w).Encode(&internal.Error{Raw: []internal.RawXMLValue{
			*internal.NewRawXMLElement(xml.Name{Space: internal.Namespace, Local: "valid-sync-token"}, nil, nil),
		}})
	}
	if err != nil {
		return err
	}
	propfind := &internal.PropFind{Prop: query.Prop}
	responses := make([]internal.Response, 0, len(result.Changes))
	for _, change := range result.Changes {
		if change.Object == nil {
			responses = append(responses, internal.Response{
				Hrefs: []internal.Href{{Path: change.Path}}, Status: &internal.Status{Code: http.StatusNotFound},
			})
			continue
		}
		response, err := b.propFindCalendarObject(r.Context(), propfind, change.Object, true)
		if err != nil {
			return err
		}
		responses = append(responses, *response)
	}
	status := internal.NewMultiStatus(responses...)
	if result.More {
		status.Responses = append(status.Responses, internal.Response{
			Hrefs:  []internal.Href{{Path: r.URL.Path}},
			Status: &internal.Status{Code: http.StatusInsufficientStorage},
			Error: &internal.Error{Raw: []internal.RawXMLValue{
				*internal.NewRawXMLElement(xml.Name{Space: internal.Namespace, Local: "number-of-matches-within-limits"}, nil, nil),
			}},
		})
	}
	status.SyncToken = result.Token
	return internal.ServeMultiStatus(w, status)
}

func decodeParamFilter(el *paramFilter) (*ParamFilter, error) {
	pf := &ParamFilter{Name: el.Name}
	if el.IsNotDefined != nil {
		if el.TextMatch != nil {
			return nil, fmt.Errorf("caldav: failed to parse param-filter: if is-not-defined is provided, text-match can't be provided")
		}
		pf.IsNotDefined = true
	}
	if el.TextMatch != nil {
		match, err := decodeTextMatch(el.TextMatch)
		if err != nil {
			return nil, err
		}
		pf.TextMatch = match
	}
	return pf, nil
}

func decodePropFilter(el *propFilter) (*PropFilter, error) {
	pf := &PropFilter{Name: el.Name}
	if el.IsNotDefined != nil {
		if el.TextMatch != nil || el.TimeRange != nil || len(el.ParamFilter) > 0 {
			return nil, fmt.Errorf("caldav: failed to parse prop-filter: if is-not-defined is provided, text-match, time-range, or param-filter can't be provided")
		}
		pf.IsNotDefined = true
	}
	if el.TextMatch != nil {
		match, err := decodeTextMatch(el.TextMatch)
		if err != nil {
			return nil, err
		}
		pf.TextMatch = match
	}
	if el.TimeRange != nil {
		pf.Start = time.Time(el.TimeRange.Start)
		pf.End = time.Time(el.TimeRange.End)
	}
	for _, paramEl := range el.ParamFilter {
		paramFi, err := decodeParamFilter(&paramEl)
		if err != nil {
			return nil, err
		}
		pf.ParamFilter = append(pf.ParamFilter, *paramFi)
	}
	return pf, nil
}

func decodeTextMatch(el *textMatch) (*TextMatch, error) {
	collation := el.Collation
	if collation == "" || collation == "default" {
		collation = "i;ascii-casemap"
	}
	if collation != "i;ascii-casemap" && collation != "i;octet" {
		return nil, internal.HTTPErrorf(http.StatusForbidden, "caldav: unsupported collation %q", collation)
	}
	return &TextMatch{Text: el.Text, Collation: collation, NegateCondition: bool(el.NegateCondition)}, nil
}

func decodeCompFilter(el *compFilter) (*CompFilter, error) {
	cf := &CompFilter{Name: el.Name}
	if el.IsNotDefined != nil {
		if el.TimeRange != nil || len(el.PropFilters) > 0 || len(el.CompFilters) > 0 {
			return nil, fmt.Errorf("caldav: failed to parse comp-filter: if is-not-defined is provided, time-range, prop-filter, or comp-filter can't be provided")
		}
		cf.IsNotDefined = true
	}
	if el.TimeRange != nil {
		cf.Start = time.Time(el.TimeRange.Start)
		cf.End = time.Time(el.TimeRange.End)
	}
	for _, pfEl := range el.PropFilters {
		pf, err := decodePropFilter(&pfEl)
		if err != nil {
			return nil, err
		}
		cf.Props = append(cf.Props, *pf)
	}
	for _, childEl := range el.CompFilters {
		child, err := decodeCompFilter(&childEl)
		if err != nil {
			return nil, err
		}
		cf.Comps = append(cf.Comps, *child)
	}
	return cf, nil
}

func decodeComp(comp *comp) (*CalendarCompRequest, error) {
	if comp == nil {
		return nil, internal.HTTPErrorf(http.StatusBadRequest, "caldav: unexpected empty calendar-data in request")
	}
	if comp.Allprop != nil && len(comp.Prop) > 0 {
		return nil, internal.HTTPErrorf(http.StatusBadRequest, "caldav: only one of allprop or prop can be specified in calendar-data")
	}
	if comp.Allcomp != nil && len(comp.Comp) > 0 {
		return nil, internal.HTTPErrorf(http.StatusBadRequest, "caldav: only one of allcomp or comp can be specified in calendar-data")
	}

	req := &CalendarCompRequest{
		AllProps: comp.Allprop != nil,
		AllComps: comp.Allcomp != nil,
	}
	for _, p := range comp.Prop {
		req.Props = append(req.Props, p.Name)
	}
	for _, c := range comp.Comp {
		comp, err := decodeComp(&c)
		if err != nil {
			return nil, err
		}
		req.Comps = append(req.Comps, *comp)
	}
	return req, nil
}

func decodeCalendarDataReq(calendarData *calendarDataReq) (*CalendarCompRequest, error) {
	if calendarData.Comp == nil {
		return &CalendarCompRequest{
			AllProps: true,
			AllComps: true,
		}, nil
	}
	return decodeComp(calendarData.Comp)
}

func (h *Handler) handleQuery(r *http.Request, w http.ResponseWriter, query *calendarQuery) error {
	var q CalendarQuery
	// TODO: calendar-data in query.Prop
	cf, err := decodeCompFilter(&query.Filter.CompFilter)
	if err != nil {
		return err
	}
	q.CompFilter = *cf

	cos, err := h.Backend.QueryCalendarObjects(r.Context(), r.URL.Path, &q)
	if err != nil {
		return err
	}

	var resps []internal.Response
	for _, co := range cos {
		b := backend{
			Backend: h.Backend,
			Prefix:  strings.TrimSuffix(h.Prefix, "/"),
		}
		propfind := internal.PropFind{
			Prop:     query.Prop,
			AllProp:  query.AllProp,
			PropName: query.PropName,
		}
		resp, err := b.propFindCalendarObject(r.Context(), &propfind, &co, true)
		if err != nil {
			return err
		}
		resps = append(resps, *resp)
	}

	ms := internal.NewMultiStatus(resps...)

	return internal.ServeMultiStatus(w, ms)
}

func (h *Handler) handleMultiget(ctx context.Context, w http.ResponseWriter, multiget *calendarMultiget) error {
	var dataReq CalendarCompRequest
	if multiget.Prop != nil {
		var calendarData calendarDataReq
		if err := multiget.Prop.Decode(&calendarData); err != nil && !internal.IsNotFound(err) {
			return err
		}
		decoded, err := decodeCalendarDataReq(&calendarData)
		if err != nil {
			return err
		}
		dataReq = *decoded
	}

	var resps []internal.Response
	for _, href := range multiget.Hrefs {
		co, err := h.Backend.GetCalendarObject(ctx, href.Path, &dataReq)
		if err != nil {
			resp := internal.NewErrorResponse(href.Path, err)
			resps = append(resps, *resp)
			continue
		}

		b := backend{
			Backend: h.Backend,
			Prefix:  strings.TrimSuffix(h.Prefix, "/"),
		}
		propfind := internal.PropFind{
			Prop:     multiget.Prop,
			AllProp:  multiget.AllProp,
			PropName: multiget.PropName,
		}
		resp, err := b.propFindCalendarObject(ctx, &propfind, co, true)
		if err != nil {
			return err
		}
		resps = append(resps, *resp)
	}

	ms := internal.NewMultiStatus(resps...)
	return internal.ServeMultiStatus(w, ms)
}

type backend struct {
	Backend Backend
	Prefix  string
}

type resourceType int

const (
	resourceTypeRoot resourceType = iota
	resourceTypeUserPrincipal
	resourceTypeCalendarHomeSet
	resourceTypeCalendar
	resourceTypeCalendarObject
	resourceTypeGroupPrincipal
)

func (b *backend) resourceTypeAtPath(reqPath string) resourceType {
	p := path.Clean(reqPath)
	p = strings.TrimPrefix(p, b.Prefix)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if p == "/" {
		return resourceTypeRoot
	}
	if strings.HasPrefix(p, "/groups/") && len(strings.Split(strings.Trim(p, "/"), "/")) == 2 {
		return resourceTypeGroupPrincipal
	}
	return resourceType(len(strings.Split(p, "/")) - 1)
}

func (b *backend) Options(r *http.Request) (caps []string, allow []string, err error) {
	caps = []string{"calendar-access"}
	switch b.resourceTypeAtPath(r.URL.Path) {
	case resourceTypeRoot, resourceTypeUserPrincipal, resourceTypeGroupPrincipal:
		return caps, []string{http.MethodOptions, "PROPFIND"}, nil
	case resourceTypeCalendarHomeSet:
		return caps, []string{http.MethodOptions, "PROPFIND", "MKCOL", "MKCALENDAR"}, nil
	case resourceTypeCalendar:
		allow := []string{http.MethodOptions, "PROPFIND", "REPORT", http.MethodDelete}
		if _, ok := b.Backend.(CalendarACLBackend); ok {
			allow = append(allow, "ACL")
		}
		if _, ok := b.Backend.(CalendarPropertyBackend); ok {
			allow = append(allow, "PROPPATCH")
		}
		if _, ok := b.Backend.(CalendarCollectionTransferBackend); ok {
			allow = append(allow, "COPY", "MOVE")
		}
		return caps, allow, nil
	case resourceTypeCalendarObject:
	default:
		return nil, nil, internal.HTTPErrorf(http.StatusNotFound, "caldav: unknown resource path")
	}

	var dataReq CalendarCompRequest
	_, err = b.Backend.GetCalendarObject(r.Context(), r.URL.Path, &dataReq)
	if httpErr, ok := err.(*internal.HTTPError); ok && httpErr.Code == http.StatusNotFound {
		return caps, []string{http.MethodOptions, http.MethodPut}, nil
	} else if err != nil {
		return nil, nil, err
	}

	allow = []string{
		http.MethodOptions,
		http.MethodHead,
		http.MethodGet,
		http.MethodPut,
		http.MethodDelete,
		"PROPFIND",
	}
	if _, ok := b.Backend.(CalendarObjectTransferBackend); ok {
		allow = append(allow, "COPY", "MOVE")
	}
	return caps, allow, nil
}

func (b *backend) HeadGet(w http.ResponseWriter, r *http.Request) error {
	var dataReq CalendarCompRequest
	if r.Method != http.MethodHead {
		dataReq.AllProps = true
	}
	co, err := b.Backend.GetCalendarObject(r.Context(), r.URL.Path, &dataReq)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", ical.MIMEType)
	if co.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(co.ContentLength, 10))
	}
	if co.ETag != "" {
		w.Header().Set("ETag", internal.ETag(co.ETag).String())
	}
	if co.ScheduleTag != "" {
		w.Header().Set("Schedule-Tag", strconv.Quote(co.ScheduleTag))
	}
	if !co.ModTime.IsZero() {
		w.Header().Set("Last-Modified", co.ModTime.UTC().Format(http.TimeFormat))
	}

	if r.Method != http.MethodHead {
		return ical.NewEncoder(w).Encode(co.Data)
	}
	return nil
}

func (b *backend) PropFind(r *http.Request, propfind *internal.PropFind, depth internal.Depth) (*internal.MultiStatus, error) {
	resType := b.resourceTypeAtPath(r.URL.Path)

	var dataReq CalendarCompRequest
	var resps []internal.Response

	switch resType {
	case resourceTypeRoot:
		resp, err := b.propFindRoot(r.Context(), propfind)
		if err != nil {
			return nil, err
		}
		resps = append(resps, *resp)
	case resourceTypeUserPrincipal:
		principalPath, err := b.Backend.CurrentUserPrincipal(r.Context())
		if err != nil {
			return nil, err
		}
		if r.URL.Path == principalPath {
			homeSetPath, err := b.Backend.CalendarHomeSetPath(r.Context(), r.URL.Path)
			if err != nil {
				return nil, err
			}
			resp, err := b.propFindUserPrincipal(r.Context(), propfind, r.URL.Path)
			if err != nil {
				return nil, err
			}
			resps = append(resps, *resp)
			if depth != internal.DepthZero {
				resp, err := b.propFindHomeSet(r.Context(), propfind, homeSetPath)
				if err != nil {
					return nil, err
				}
				resps = append(resps, *resp)
				if depth == internal.DepthInfinity {
					resps_, err := b.propFindAllCalendars(r.Context(), propfind, true, homeSetPath)
					if err != nil {
						return nil, err
					}
					resps = append(resps, resps_...)
				}
			}
		}
		if r.URL.Path != principalPath {
			aclBackend, ok := b.Backend.(CalendarAccountPrincipalBackend)
			if !ok {
				return nil, internal.HTTPErrorf(http.StatusNotFound, "caldav: account principal unavailable")
			}
			if err := aclBackend.GetAccountPrincipal(r.Context(), r.URL.Path); err != nil {
				return nil, err
			}
			props := map[xml.Name]internal.PropFindFunc{
				internal.ResourceTypeName: internal.PropFindValue(internal.NewResourceType(internal.CollectionName, internal.PrincipalName)),
				xml.Name{Space: internal.Namespace, Local: "principal-URL"}: internal.PropFindValue(&struct {
					XMLName xml.Name `xml:"DAV: principal-URL"`
					Href    string   `xml:"DAV: href"`
				}{Href: r.URL.Path}),
			}
			if directory, ok := b.Backend.(webdav.DAVPrincipalBackend); ok {
				info, err := directory.DAVPrincipal(r.Context(), r.URL.Path)
				if err != nil {
					return nil, err
				}
				for name, property := range internal.PrincipalProperties(r.URL.Path, info.DisplayName, info.Members, info.Memberships) {
					props[name] = property
				}
			}
			resp, err := internal.NewPropFindResponse(r.URL.Path, propfind, props)
			if err != nil {
				return nil, err
			}
			resps = append(resps, *resp)
		}
	case resourceTypeGroupPrincipal:
		aclBackend, ok := b.Backend.(CalendarACLBackend)
		if !ok {
			return nil, internal.HTTPErrorf(http.StatusNotFound, "caldav: group principal unavailable")
		}
		if err := aclBackend.GetGroupPrincipal(r.Context(), r.URL.Path); err != nil {
			return nil, err
		}
		props := map[xml.Name]internal.PropFindFunc{
			internal.ResourceTypeName: internal.PropFindValue(internal.NewResourceType(internal.PrincipalName)),
			xml.Name{Space: internal.Namespace, Local: "principal-URL"}: internal.PropFindValue(&struct {
				XMLName xml.Name `xml:"DAV: principal-URL"`
				Href    string   `xml:"DAV: href"`
			}{Href: r.URL.Path}),
		}
		if directory, ok := b.Backend.(webdav.DAVPrincipalBackend); ok {
			info, err := directory.DAVPrincipal(r.Context(), r.URL.Path)
			if err != nil {
				return nil, err
			}
			for name, property := range internal.PrincipalProperties(r.URL.Path, info.DisplayName, info.Members, info.Memberships) {
				props[name] = property
			}
		}
		resp, err := internal.NewPropFindResponse(r.URL.Path, propfind, props)
		if err != nil {
			return nil, err
		}
		resps = append(resps, *resp)
	case resourceTypeCalendarHomeSet:
		homeSetPath, err := b.Backend.CalendarHomeSetPath(r.Context(), r.URL.Path)
		if err != nil {
			return nil, err
		}
		if r.URL.Path == homeSetPath {
			resp, err := b.propFindHomeSet(r.Context(), propfind, homeSetPath)
			if err != nil {
				return nil, err
			}
			resps = append(resps, *resp)
			if depth != internal.DepthZero {
				recurse := depth == internal.DepthInfinity
				resps_, err := b.propFindAllCalendars(r.Context(), propfind, recurse, homeSetPath)
				if err != nil {
					return nil, err
				}
				resps = append(resps, resps_...)
			}
		}
	case resourceTypeCalendar:
		ab, err := b.Backend.GetCalendar(r.Context(), r.URL.Path)
		if err != nil {
			return nil, err
		}
		resp, err := b.propFindCalendar(r.Context(), propfind, ab)
		if err != nil {
			return nil, err
		}
		resps = append(resps, *resp)
		if depth != internal.DepthZero {
			resps_, err := b.propFindAllCalendarObjects(r.Context(), propfind, ab)
			if err != nil {
				return nil, err
			}
			resps = append(resps, resps_...)
		}
	case resourceTypeCalendarObject:
		ao, err := b.Backend.GetCalendarObject(r.Context(), r.URL.Path, &dataReq)
		if err != nil {
			return nil, err
		}

		resp, err := b.propFindCalendarObject(r.Context(), propfind, ao, false)
		if err != nil {
			return nil, err
		}
		resps = append(resps, *resp)
	}

	return internal.NewMultiStatus(resps...), nil
}

func (b *backend) propFindRoot(ctx context.Context, propfind *internal.PropFind) (*internal.Response, error) {
	principalPath, err := b.Backend.CurrentUserPrincipal(ctx)
	if err != nil {
		return nil, err
	}

	props := map[xml.Name]internal.PropFindFunc{
		internal.CurrentUserPrincipalName: internal.PropFindValue(&internal.CurrentUserPrincipal{
			Href: internal.Href{Path: principalPath},
		}),
		internal.ResourceTypeName: internal.PropFindValue(internal.NewResourceType(internal.CollectionName)),
	}
	return internal.NewPropFindResponse(principalPath, propfind, props)
}

func (b *backend) propFindUserPrincipal(ctx context.Context, propfind *internal.PropFind, requestedPath string) (*internal.Response, error) {
	principalPath, err := b.Backend.CurrentUserPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	homeSetPath, err := b.Backend.CalendarHomeSetPath(ctx, requestedPath)
	if err != nil {
		return nil, err
	}

	props := map[xml.Name]internal.PropFindFunc{
		internal.CurrentUserPrincipalName: internal.PropFindValue(&internal.CurrentUserPrincipal{
			Href: internal.Href{Path: principalPath},
		}),
		xml.Name{Space: internal.Namespace, Local: "principal-URL"}: internal.PropFindValue(&struct {
			XMLName xml.Name `xml:"DAV: principal-URL"`
			Href    string   `xml:"DAV: href"`
		}{Href: requestedPath}),
		calendarHomeSetName: internal.PropFindValue(&calendarHomeSet{
			Href: internal.Href{Path: homeSetPath},
		}),
		internal.ResourceTypeName: internal.PropFindValue(internal.NewResourceType(internal.CollectionName, internal.PrincipalName)),
	}
	if directory, ok := b.Backend.(webdav.DAVPrincipalBackend); ok {
		info, err := directory.DAVPrincipal(ctx, requestedPath)
		if err != nil {
			return nil, err
		}
		for name, property := range internal.PrincipalProperties(requestedPath, info.DisplayName, info.Members, info.Memberships) {
			props[name] = property
		}
	}
	return internal.NewPropFindResponse(requestedPath, propfind, props)
}

func (b *backend) propFindHomeSet(ctx context.Context, propfind *internal.PropFind, requestedPath string) (*internal.Response, error) {
	principalPath, err := b.Backend.CurrentUserPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	homeSetPath, err := b.Backend.CalendarHomeSetPath(ctx, requestedPath)
	if err != nil {
		return nil, err
	}

	// TODO anything else to return here?
	props := map[xml.Name]internal.PropFindFunc{
		internal.CurrentUserPrincipalName: internal.PropFindValue(&internal.CurrentUserPrincipal{
			Href: internal.Href{Path: principalPath},
		}),
		internal.ResourceTypeName: internal.PropFindValue(internal.NewResourceType(internal.CollectionName)),
	}
	return internal.NewPropFindResponse(homeSetPath, propfind, props)
}

func (b *backend) propFindCalendar(ctx context.Context, propfind *internal.PropFind, cal *Calendar) (*internal.Response, error) {
	props := map[xml.Name]internal.PropFindFunc{
		internal.CurrentUserPrincipalName: func(*internal.RawXMLValue) (interface{}, error) {
			path, err := b.Backend.CurrentUserPrincipal(ctx)
			if err != nil {
				return nil, err
			}
			return &internal.CurrentUserPrincipal{Href: internal.Href{Path: path}}, nil
		},
		internal.ResourceTypeName: internal.PropFindValue(internal.NewResourceType(internal.CollectionName, calendarName)),
		calendarDescriptionName: internal.PropFindValue(&calendarDescription{
			Description: cal.Description,
		}),
		supportedCalendarDataName: internal.PropFindValue(&supportedCalendarData{
			Types: []calendarDataType{
				{ContentType: ical.MIMEType, Version: "2.0"},
			},
		}),
		supportedCollationSetName: internal.PropFindValue(&supportedCollationSet{
			Collations: []string{"i;ascii-casemap", "i;octet"},
		}),
		supportedCalendarComponentSetName: func(*internal.RawXMLValue) (interface{}, error) {
			components := []comp{}
			if cal.SupportedComponentSet != nil {
				for _, name := range cal.SupportedComponentSet {
					components = append(components, comp{Name: name})
				}
			} else {
				components = append(components, comp{Name: ical.CompEvent})
			}
			return &supportedCalendarComponentSet{
				Comp: components,
			}, nil
		},
		internal.CurrentUserPrivilegeSetName: func(*internal.RawXMLValue) (interface{}, error) {
			privileges := make([]internal.Privilege, 0, 5)
			if cal.Read {
				privileges = append(privileges, internal.Privilege{Read: &struct{}{}})
			}
			if cal.ReadFreeBusy {
				privileges = append(privileges, internal.Privilege{ReadFreeBusy: &struct{}{}})
			}
			if cal.WriteContent {
				privileges = append(privileges, internal.Privilege{WriteContent: &struct{}{}})
			}
			if cal.WriteProperties {
				privileges = append(privileges, internal.Privilege{WriteProperties: &struct{}{}})
			}
			if cal.Bind {
				privileges = append(privileges, internal.Privilege{Bind: &struct{}{}})
			}
			if cal.Unbind {
				privileges = append(privileges, internal.Privilege{Unbind: &struct{}{}})
			}
			if aclBackend, ok := b.Backend.(CalendarACLBackend); ok {
				acl, err := aclBackend.GetCalendarACL(ctx, cal.Path)
				if err != nil {
					return nil, err
				}
				if acl.CanWrite {
					privileges = append(privileges, internal.Privilege{WriteACL: &struct{}{}})
				}
			}
			return &internal.CurrentUserPrivilegeSet{Privilege: privileges}, nil
		},
	}
	if cal.Color != "" {
		props[calendarColorName] = internal.PropFindValue(&calendarColor{Color: cal.Color})
	}
	if aclBackend, ok := b.Backend.(CalendarACLBackend); ok {
		props[xml.Name{Space: internal.Namespace, Local: "acl-restrictions"}] = internal.PropFindValue(&aclRestrictionsXML{
			GrantOnly: &struct{}{}, NoInvert: &struct{}{},
		})
		props[xml.Name{Space: internal.Namespace, Local: "supported-privilege-set"}] = internal.PropFindValue(supportedCalendarACLPrivileges())
		props[xml.Name{Space: internal.Namespace, Local: "principal-collection-set"}] = internal.PropFindValue(&principalCollectionSetXML{Href: b.Prefix + "/"})
		props[xml.Name{Space: internal.Namespace, Local: "acl"}] = func(*internal.RawXMLValue) (interface{}, error) {
			acl, err := aclBackend.GetCalendarACL(ctx, cal.Path)
			if err != nil {
				return nil, err
			}
			return calendarACLProperty(acl), nil
		}
		props[xml.Name{Space: internal.Namespace, Local: "owner"}] = func(*internal.RawXMLValue) (interface{}, error) {
			acl, err := aclBackend.GetCalendarACL(ctx, cal.Path)
			if err != nil {
				return nil, err
			}
			return &struct {
				XMLName xml.Name `xml:"DAV: owner"`
				Href    string   `xml:"DAV: href"`
			}{Href: acl.Owner}, nil
		}
	}

	displayName := cal.DisplayName
	if displayName == "" {
		displayName = cal.Name
	}
	if displayName != "" {
		props[internal.DisplayNameName] = internal.PropFindValue(&internal.DisplayName{
			Name: displayName,
		})
	}
	if cal.Description != "" {
		props[calendarDescriptionName] = internal.PropFindValue(&calendarDescription{
			Description: cal.Description,
		})
	}
	if cal.MaxResourceSize > 0 {
		props[maxResourceSizeName] = internal.PropFindValue(&maxResourceSize{
			Size: cal.MaxResourceSize,
		})
	}
	if syncBackend, ok := b.Backend.(CalendarSyncBackend); ok {
		props[xml.Name{Space: internal.Namespace, Local: "sync-token"}] = func(*internal.RawXMLValue) (interface{}, error) {
			token, err := syncBackend.CalendarSyncToken(ctx, cal.Path)
			if err != nil {
				return nil, err
			}
			return &syncTokenProperty{Value: token}, nil
		}
		reports := []supportedSyncReport{
			{Report: supportedSyncReportName{SyncCollection: &struct{}{}}},
			{Report: supportedSyncReportName{CalendarQuery: &struct{}{}}},
			{Report: supportedSyncReportName{CalendarMultiget: &struct{}{}}},
			{Report: supportedSyncReportName{FreeBusyQuery: &struct{}{}}},
		}
		if _, ok := b.Backend.(webdav.DAVPrincipalBackend); ok {
			reports = append(reports,
				supportedSyncReport{Report: supportedSyncReportName{ACLPrincipals: &struct{}{}}},
				supportedSyncReport{Report: supportedSyncReportName{PrincipalMatch: &struct{}{}}},
				supportedSyncReport{Report: supportedSyncReportName{PrincipalSearch: &struct{}{}}},
				supportedSyncReport{Report: supportedSyncReportName{SearchProperties: &struct{}{}}},
				supportedSyncReport{Report: supportedSyncReportName{ExpandProperty: &struct{}{}}})
		}
		props[xml.Name{Space: internal.Namespace, Local: "supported-report-set"}] = internal.PropFindValue(
			&supportedSyncReports{Reports: reports})
	}

	// TODO: CALDAV:calendar-timezone, CALDAV:supported-calendar-component-set, CALDAV:min-date-time, CALDAV:max-date-time, CALDAV:max-instances, CALDAV:max-attendees-per-instance

	return internal.NewPropFindResponse(cal.Path, propfind, props)
}

func (b *backend) propFindAllCalendars(ctx context.Context, propfind *internal.PropFind, recurse bool, requestedPath string) ([]internal.Response, error) {
	abs, err := b.Backend.ListCalendars(ctx, requestedPath)
	if err != nil {
		return nil, err
	}

	var resps []internal.Response
	for _, ab := range abs {
		resp, err := b.propFindCalendar(ctx, propfind, &ab)
		if err != nil {
			return nil, err
		}
		resps = append(resps, *resp)
		if recurse {
			resps_, err := b.propFindAllCalendarObjects(ctx, propfind, &ab)
			if err != nil {
				return nil, err
			}
			resps = append(resps, resps_...)
		}
	}
	return resps, nil
}

func (b *backend) propFindCalendarObject(ctx context.Context, propfind *internal.PropFind, co *CalendarObject, report bool) (*internal.Response, error) {
	props := map[xml.Name]internal.PropFindFunc{
		internal.CurrentUserPrincipalName: func(*internal.RawXMLValue) (interface{}, error) {
			path, err := b.Backend.CurrentUserPrincipal(ctx)
			if err != nil {
				return nil, err
			}
			return &internal.CurrentUserPrincipal{Href: internal.Href{Path: path}}, nil
		},
		internal.GetContentTypeName: internal.PropFindValue(&internal.GetContentType{
			Type: ical.MIMEType,
		}),
	}
	if report {
		props[calendarDataName] = func(raw *internal.RawXMLValue) (interface{}, error) {
			var request calendarDataReq
			if err := raw.Decode(&request); err != nil {
				return nil, internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid calendar-data selection: %v", err)
			}
			selected, err := projectCalendarData(co.Data, &request)
			if err != nil {
				return nil, err
			}
			return &calendarDataResp{Data: selected}, nil
		}
	}

	if co.ContentLength > 0 {
		props[internal.GetContentLengthName] = internal.PropFindValue(&internal.GetContentLength{
			Length: co.ContentLength,
		})
	}
	if !co.ModTime.IsZero() {
		props[internal.GetLastModifiedName] = internal.PropFindValue(&internal.GetLastModified{
			LastModified: internal.Time(co.ModTime),
		})
	}

	if co.ETag != "" {
		props[internal.GetETagName] = internal.PropFindValue(&internal.GetETag{
			ETag: internal.ETag(co.ETag),
		})
	}
	if co.ScheduleTag != "" {
		props[scheduleTagName] = internal.PropFindValue(&scheduleTag{Value: strconv.Quote(co.ScheduleTag)})
	}

	return internal.NewPropFindResponse(co.Path, propfind, props)
}

func (b *backend) propFindAllCalendarObjects(ctx context.Context, propfind *internal.PropFind, cal *Calendar) ([]internal.Response, error) {
	var dataReq CalendarCompRequest
	aos, err := b.Backend.ListCalendarObjects(ctx, cal.Path, &dataReq)
	if err != nil {
		return nil, err
	}

	var resps []internal.Response
	for _, ao := range aos {
		resp, err := b.propFindCalendarObject(ctx, propfind, &ao, false)
		if err != nil {
			return nil, err
		}
		resps = append(resps, *resp)
	}
	return resps, nil
}

func (b *backend) PropPatch(r *http.Request, update *internal.PropertyUpdate) (*internal.Response, error) {
	if b.resourceTypeAtPath(r.URL.Path) != resourceTypeCalendar {
		return nil, internal.HTTPErrorf(http.StatusMethodNotAllowed, "caldav: only calendar metadata can be changed")
	}
	backend, ok := b.Backend.(CalendarPropertyBackend)
	if !ok {
		return nil, internal.HTTPErrorf(http.StatusNotImplemented, "caldav: PropPatch not implemented")
	}
	changes := make([]CalendarPropertyUpdate, 0)
	failed := make(map[xml.Name]int)
	seen := make(map[xml.Name]bool)
	appendChange := func(raw internal.RawXMLValue, remove bool) error {
		name, ok := raw.XMLName()
		if !ok {
			return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid property")
		}
		if seen[name] {
			return internal.HTTPErrorf(http.StatusBadRequest, "caldav: property repeated in PROPPATCH")
		}
		seen[name] = true
		if name != internal.DisplayNameName && name != calendarDescriptionName && name != calendarColorName {
			failed[name] = http.StatusForbidden
			return nil
		}
		change := CalendarPropertyUpdate{Name: name, Remove: remove}
		if !remove {
			var value struct {
				Text  string `xml:",chardata"`
				Inner string `xml:",innerxml"`
			}
			if err := raw.Decode(&value); err != nil {
				return internal.HTTPErrorf(http.StatusBadRequest, "caldav: invalid property value: %v", err)
			}
			if strings.Contains(value.Inner, "<") {
				return internal.HTTPErrorf(http.StatusBadRequest, "caldav: calendar property requires text")
			}
			change.Value = value.Text
		}
		changes = append(changes, change)
		return nil
	}
	for _, group := range update.Remove {
		for _, raw := range group.Prop.Raw {
			if err := appendChange(raw, true); err != nil {
				return nil, err
			}
		}
	}
	for _, group := range update.Set {
		for _, raw := range group.Prop.Raw {
			if err := appendChange(raw, false); err != nil {
				return nil, err
			}
		}
	}
	if len(seen) == 0 {
		return nil, internal.HTTPErrorf(http.StatusBadRequest, "caldav: empty PROPPATCH")
	}
	if len(failed) == 0 {
		if err := backend.PatchCalendarProperties(r.Context(), r.URL.Path, changes); err != nil {
			code := internal.HTTPErrorFromError(err).Code
			for name := range seen {
				failed[name] = code
			}
		}
	}
	resp := &internal.Response{Hrefs: []internal.Href{{Path: r.URL.Path}}}
	for name := range seen {
		code := http.StatusOK
		if failed[name] != 0 {
			code = failed[name]
		} else if len(failed) != 0 {
			code = http.StatusFailedDependency
		}
		if err := resp.EncodeProp(code, internal.NewRawXMLElement(name, nil, nil)); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func (b *backend) Put(w http.ResponseWriter, r *http.Request) error {
	ifNoneMatch := webdav.ConditionalMatch(r.Header.Get("If-None-Match"))
	ifMatch := webdav.ConditionalMatch(r.Header.Get("If-Match"))

	opts := PutCalendarObjectOptions{
		IfNoneMatch: ifNoneMatch,
		IfMatch:     ifMatch,
	}

	t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: malformed Content-Type: %v", err)
	}
	if t != ical.MIMEType {
		// TODO: send CALDAV:supported-calendar-data error
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: unsupported Content-Type %q", t)
	}

	// TODO: check CALDAV:max-resource-size precondition
	cal, err := ical.NewDecoder(r.Body).Decode()
	if err != nil {
		// TODO: send CALDAV:valid-calendar-data error
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: failed to parse iCalendar: %v", err)
	}

	co, err := b.Backend.PutCalendarObject(r.Context(), r.URL.Path, cal, &opts)
	if err != nil {
		return err
	}

	if co.ETag != "" {
		w.Header().Set("ETag", internal.ETag(co.ETag).String())
	}
	if co.ScheduleTag != "" {
		w.Header().Set("Schedule-Tag", strconv.Quote(co.ScheduleTag))
	}
	if !co.ModTime.IsZero() {
		w.Header().Set("Last-Modified", co.ModTime.UTC().Format(http.TimeFormat))
	}
	if co.Path != "" {
		w.Header().Set("Location", co.Path)
	}

	// TODO: http.StatusNoContent if the resource already existed
	w.WriteHeader(http.StatusCreated)

	return nil
}

func (b *backend) Delete(r *http.Request) error {
	return b.Backend.DeleteCalendarObject(r.Context(), r.URL.Path)
}

func (b *backend) Mkcol(r *http.Request) error {
	if b.resourceTypeAtPath(r.URL.Path) != resourceTypeCalendar {
		return internal.HTTPErrorf(http.StatusForbidden, "caldav: calendar creation not allowed at given location")
	}

	cal := Calendar{
		Path: r.URL.Path,
	}

	if !internal.IsRequestBodyEmpty(r) {
		var m mkcolReq
		if err := internal.DecodeXMLRequest(r, &m); err != nil {
			return internal.HTTPErrorf(http.StatusBadRequest, "carddav: error parsing mkcol request: %s", err.Error())
		}

		if !m.ResourceType.Is(internal.CollectionName) || !m.ResourceType.Is(calendarName) {
			return internal.HTTPErrorf(http.StatusBadRequest, "carddav: unexpected resource type")
		}
		cal.Name = m.DisplayName
		// TODO ...
	}

	return b.Backend.CreateCalendar(r.Context(), &cal)
}

func (b *backend) Copy(r *http.Request, dest *internal.Href, recursive, overwrite bool) (created bool, err error) {
	return b.transfer(r, dest, recursive, overwrite, false)
}

func (b *backend) Move(r *http.Request, dest *internal.Href, overwrite bool) (created bool, err error) {
	return b.transfer(r, dest, true, overwrite, true)
}

func (b *backend) transfer(r *http.Request, dest *internal.Href, recursive, overwrite, move bool) (bool, error) {
	sourceType, destinationType := b.resourceTypeAtPath(r.URL.Path), b.resourceTypeAtPath(dest.Path)
	if sourceType != destinationType || sourceType != resourceTypeCalendarObject && sourceType != resourceTypeCalendar {
		return false, internal.HTTPErrorf(http.StatusMethodNotAllowed, "caldav: COPY and MOVE require matching calendar resources")
	}
	if dest.Host != "" && !strings.EqualFold(dest.Host, r.Host) || dest.User != nil || dest.RawQuery != "" || dest.Fragment != "" {
		return false, internal.HTTPErrorf(http.StatusForbidden, "caldav: destination is outside this server")
	}
	if dest.Scheme != "" && dest.Scheme != "http" && dest.Scheme != "https" {
		return false, internal.HTTPErrorf(http.StatusForbidden, "caldav: unsupported destination scheme")
	}
	if sourceType == resourceTypeCalendar {
		backend, ok := b.Backend.(CalendarCollectionTransferBackend)
		if !ok {
			return false, internal.HTTPErrorf(http.StatusNotImplemented, "caldav: collection transfer not implemented")
		}
		if move {
			return backend.MoveCalendarCollection(r.Context(), r.URL.Path, dest.Path, overwrite)
		}
		return backend.CopyCalendarCollection(r.Context(), r.URL.Path, dest.Path, recursive, overwrite)
	}
	backend, ok := b.Backend.(CalendarObjectTransferBackend)
	if !ok {
		return false, internal.HTTPErrorf(http.StatusNotImplemented, "caldav: object transfer not implemented")
	}
	if move {
		return backend.MoveCalendarObject(r.Context(), r.URL.Path, dest.Path, overwrite)
	}
	return backend.CopyCalendarObject(r.Context(), r.URL.Path, dest.Path, overwrite)
}

// https://datatracker.ietf.org/doc/html/rfc4791#section-5.3.2.1
type PreconditionType string

const (
	PreconditionNoUIDConflict                PreconditionType = "no-uid-conflict"
	PreconditionSupportedCalendarData        PreconditionType = "supported-calendar-data"
	PreconditionSupportedCalendarComponent   PreconditionType = "supported-calendar-component"
	PreconditionValidCalendarData            PreconditionType = "valid-calendar-data"
	PreconditionValidCalendarObjectResource  PreconditionType = "valid-calendar-object-resource"
	PreconditionCalendarCollectionLocationOk PreconditionType = "calendar-collection-location-ok"
	PreconditionMaxResourceSize              PreconditionType = "max-resource-size"
	PreconditionMinDateTime                  PreconditionType = "min-date-time"
	PreconditionMaxDateTime                  PreconditionType = "max-date-time"
	PreconditionMaxInstances                 PreconditionType = "max-instances"
	PreconditionMaxAttendeesPerInstance      PreconditionType = "max-attendees-per-instance"
)

func NewPreconditionError(err PreconditionType) error {
	name := xml.Name{Space: "urn:ietf:params:xml:ns:caldav", Local: string(err)}
	elem := internal.NewRawXMLElement(name, nil, nil)
	return &internal.HTTPError{
		Code: 409,
		Err: &internal.Error{
			Raw: []internal.RawXMLValue{*elem},
		},
	}
}
