package domain

import (
	"fmt"
	"time"
)

// Document is one stored record: its data plus the metadata the server
// maintains for it.
type Document struct {
	Path       DocPath
	Data       Map
	CreateTime time.Time
	UpdateTime time.Time
	// Version is the commit version of the write that last changed the
	// document. Versions increase by one with every committed batch, across
	// the whole database, so they order all writes.
	Version int64
}

// ValidateData enforces the size and depth limits on document data.
func ValidateData(data Map) error {
	if d := Depth(data); d > MaxDepth {
		return fmt.Errorf("%w: document is nested %d levels deep, the limit is %d", ErrInvalidArgument, d, MaxDepth)
	}
	if n := Size(data); n > MaxDocumentBytes {
		return fmt.Errorf("%w: document is about %d bytes, the limit is %d", ErrInvalidArgument, n, MaxDocumentBytes)
	}
	return nil
}
