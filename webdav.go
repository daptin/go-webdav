// Package webdav provides a client and server WebDAV filesystem implementation.
//
// WebDAV is defined in RFC 4918.
package webdav

import (
	"context"
	"time"

	"github.com/daptin/go-webdav/internal"
)

// DAVPrincipal describes an application principal exposed by a DAV adapter.
type DAVPrincipal struct {
	Path        string
	DisplayName string
	Members     []string
	Memberships []string
}

// DAVPrincipalBackend resolves principals through the application's identity
// and permission authority. Search returns only principals visible to caller.
type DAVPrincipalBackend interface {
	DAVPrincipal(context.Context, string) (*DAVPrincipal, error)
	SearchDAVPrincipals(context.Context, string, string, int) ([]DAVPrincipal, error)
}

// FileInfo holds information about a WebDAV file.
type FileInfo struct {
	Path     string
	Size     int64
	ModTime  time.Time
	IsDir    bool
	MIMEType string
	ETag     string
}

type CreateOptions struct {
	IfMatch     ConditionalMatch
	IfNoneMatch ConditionalMatch
}

type RemoveAllOptions struct {
	IfMatch     ConditionalMatch
	IfNoneMatch ConditionalMatch
}

type CopyOptions struct {
	NoRecursive bool
	NoOverwrite bool
}

type MoveOptions struct {
	NoOverwrite bool
}

// ConditionalMatch represents the value of a conditional header
// according to RFC 2068 section 14.25 and RFC 2068 section 14.26
// The (optional) value can either be a wildcard or an ETag.
type ConditionalMatch string

func (val ConditionalMatch) IsSet() bool {
	return val != ""
}

func (val ConditionalMatch) IsWildcard() bool {
	return val == "*"
}

func (val ConditionalMatch) ETag() (string, error) {
	var e internal.ETag
	if err := e.UnmarshalText([]byte(val)); err != nil {
		return "", err
	}
	return string(e), nil
}

func (val ConditionalMatch) MatchETag(etag string) (bool, error) {
	if etag == "" {
		return false, nil
	}
	if val.IsWildcard() {
		return true, nil
	}
	t, err := val.ETag()
	return t == etag, err
}
