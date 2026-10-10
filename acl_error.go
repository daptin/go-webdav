package webdav

import "github.com/daptin/go-webdav/internal"

// NewACLPreconditionError reports a WebDAV ACL constraint using RFC 3744's
// DAV:error body. The caller must select the matching constraint name.
func NewACLPreconditionError(name string) error {
	return internal.ACLPreconditionError(name)
}
