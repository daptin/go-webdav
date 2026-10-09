// Package caldav provides a client and server CalDAV implementation.
//
// CalDAV is defined in RFC 4791.
package caldav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"time"

	"github.com/daptin/go-webdav"
	"github.com/daptin/go-webdav/internal"
	"github.com/emersion/go-ical"
)

var CapabilityCalendar = webdav.Capability("calendar-access")

func NewCalendarHomeSet(path string) webdav.BackendSuppliedHomeSet {
	return &calendarHomeSet{Href: internal.Href{Path: path}}
}

// ValidateCalendarObject checks the validity of a calendar object according to
// the contraints layed out in RFC 4791 section 4.1 and returns the only event
// type and UID occuring in this calendar, or an error if the calendar could
// not be validated.
func ValidateCalendarObject(cal *ical.Calendar) (eventType string, uid string, err error) {
	// Calendar object resources contained in calendar collections
	// MUST NOT specify the iCalendar METHOD property.
	if prop := cal.Props.Get(ical.PropMethod); prop != nil {
		return "", "", fmt.Errorf("calendar resource must not specify METHOD property")
	}

	for _, comp := range cal.Children {
		// Calendar object resources contained in calendar collections
		// MUST NOT contain more than one type of calendar component
		// (e.g., VEVENT, VTODO, VJOURNAL, VFREEBUSY, etc.) with the
		// exception of VTIMEZONE components, which MUST be specified
		// for each unique TZID parameter value specified in the
		// iCalendar object.
		if comp.Name != ical.CompTimezone {
			if eventType == "" {
				eventType = comp.Name
			}
			if eventType != comp.Name {
				return "", "", fmt.Errorf("conflicting event types in calendar: %s, %s", eventType, comp.Name)
			}
			// TODO check VTIMEZONE for each TZID?
		}

		// Calendar components in a calendar collection that have
		// different UID property values MUST be stored in separate
		// calendar object resources.
		compUID, err := comp.Props.Text(ical.PropUID)
		if err != nil {
			return "", "", fmt.Errorf("error checking component UID: %v", err)
		}
		if uid == "" {
			uid = compUID
		}
		if compUID != "" && uid != compUID {
			return "", "", fmt.Errorf("conflicting UID values in calendar: %s, %s", uid, compUID)
		}
	}
	return eventType, uid, nil
}

type Calendar struct {
	Path                  string
	Name                  string
	DisplayName           string
	Description           string
	MaxResourceSize       int64
	SupportedComponentSet []string
	Read                  bool
	ReadFreeBusy          bool
	WriteContent          bool
	Bind                  bool
	Unbind                bool
}

// CalendarPropertyUpdate is a requested change to a writable calendar property.
type CalendarPropertyUpdate struct {
	Name   xml.Name
	Value  string
	Remove bool
}

// CalendarPropertyBackend is optional so existing CalDAV backends need not
// implement writable collection metadata.
type CalendarPropertyBackend interface {
	PatchCalendarProperties(context.Context, string, []CalendarPropertyUpdate) error
}

// CalendarObjectTransferBackend is optional for calendar object COPY and MOVE.
type CalendarObjectTransferBackend interface {
	CopyCalendarObject(context.Context, string, string, bool) (bool, error)
	MoveCalendarObject(context.Context, string, string, bool) (bool, error)
}

// CalendarCollectionTransferBackend is optional for calendar collection
// COPY and MOVE. COPY receives whether the request has infinite depth.
type CalendarCollectionTransferBackend interface {
	CopyCalendarCollection(context.Context, string, string, bool, bool) (bool, error)
	MoveCalendarCollection(context.Context, string, string, bool) (bool, error)
}

// ErrInvalidSyncToken asks the DAV handler to return DAV:valid-sync-token.
var ErrInvalidSyncToken = errors.New("invalid calendar sync token")

type CalendarSyncChange struct {
	Path   string
	Object *CalendarObject // nil for a removed or no-longer-readable member
}

type CalendarSyncResult struct {
	Token   string
	Changes []CalendarSyncChange
	More    bool
}

// CalendarSyncBackend is optional. Tokens and results are scoped to the
// authenticated user and the calendar collection path.
type CalendarSyncBackend interface {
	CalendarSyncToken(context.Context, string) (string, error)
	SyncCalendarObjects(context.Context, string, string, int) (*CalendarSyncResult, error)
}

type CalendarCompRequest struct {
	Name string

	AllProps bool
	Props    []string

	AllComps bool
	Comps    []CalendarCompRequest

	Expand *CalendarExpandRequest
}

type CalendarExpandRequest struct {
	Start, End time.Time
}

type CompFilter struct {
	Name         string
	IsNotDefined bool
	Start, End   time.Time
	Props        []PropFilter
	Comps        []CompFilter
}

type ParamFilter struct {
	Name         string
	IsNotDefined bool
	TextMatch    *TextMatch
}

type PropFilter struct {
	Name         string
	IsNotDefined bool
	Start, End   time.Time
	TextMatch    *TextMatch
	ParamFilter  []ParamFilter
}

type TextMatch struct {
	Text            string
	NegateCondition bool
}

type CalendarQuery struct {
	CompRequest CalendarCompRequest
	CompFilter  CompFilter
}

type CalendarMultiGet struct {
	Paths       []string
	CompRequest CalendarCompRequest
}

type CalendarObject struct {
	Path          string
	ModTime       time.Time
	ContentLength int64
	ETag          string
	ScheduleTag   string
	Data          *ical.Calendar
}
