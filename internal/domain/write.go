package domain

import (
	"fmt"
	"math"
	"time"
)

// WriteOp says how a Write treats the document that is already stored.
type WriteOp int

const (
	// OpSet replaces the whole document, creating it if needed.
	OpSet WriteOp = iota + 1
	// OpCreate creates the document and fails if it already exists.
	OpCreate
	// OpMerge changes the listed fields, creating the document if needed.
	OpMerge
	// OpUpdate changes the listed fields and fails if the document is missing.
	OpUpdate
	// OpDelete removes the document. Deleting a missing document succeeds.
	OpDelete
	// OpVerify changes nothing; it only checks its precondition. It lets a
	// transaction say "and this document I merely read must not have
	// changed either".
	OpVerify
)

func (op WriteOp) String() string {
	switch op {
	case OpSet:
		return "set"
	case OpCreate:
		return "create"
	case OpMerge:
		return "merge"
	case OpUpdate:
		return "update"
	case OpDelete:
		return "delete"
	case OpVerify:
		return "verify"
	}
	return "unknown"
}

// Write is one requested change to one document. It is a value describing
// the change, separate from the code that carries it out, so that a batch of
// writes can be validated, authorised and applied as a unit.
type Write struct {
	Op   WriteOp
	Path DocPath
	// Data is the new content for OpSet and OpCreate.
	Data Map
	// Fields are applied in order on top of Data (OpSet, OpCreate) or on top
	// of the stored document (OpMerge, OpUpdate).
	Fields []FieldOp
	// Pre is checked against the stored document before the write applies.
	Pre Precondition
}

// FieldOp applies a Transform to one field.
type FieldOp struct {
	Path      FieldPath
	Transform Transform
}

// Precondition makes a write conditional. The zero value imposes nothing.
type Precondition struct {
	// Exists, when set, requires the document to exist (true) or not (false).
	Exists *bool
	// Version, when set, requires the stored document to be at exactly this
	// version. It is how clients build optimistic transactions: read, then
	// write only if nothing changed in between.
	Version *int64
}

// Check tests the precondition against the stored document (nil if absent).
func (p Precondition) Check(existing *Document) error {
	if p.Exists != nil {
		switch {
		case *p.Exists && existing == nil:
			return fmt.Errorf("%w: document does not exist", ErrFailedPrecondition)
		case !*p.Exists && existing != nil:
			return fmt.Errorf("%w: document already exists", ErrFailedPrecondition)
		}
	}
	if p.Version != nil {
		if existing == nil {
			return fmt.Errorf("%w: document does not exist", ErrFailedPrecondition)
		}
		if existing.Version != *p.Version {
			return fmt.Errorf("%w: document is at version %d, expected %d",
				ErrFailedPrecondition, existing.Version, *p.Version)
		}
	}
	return nil
}

// Transform computes the new value of a field from its previous value. Each
// implementation is a small command object: the data it needs travels with
// it and Apply carries it out.
type Transform interface {
	// Apply returns the new value. prev is nil when the field does not
	// exist. Returning remove=true deletes the field.
	Apply(prev Value, now time.Time) (v Value, remove bool)
}

// SetValue stores a fixed value.
type SetValue struct{ Value Value }

// DeleteField removes the field.
type DeleteField struct{}

// ServerTimestamp stores the time at which the server commits the write.
type ServerTimestamp struct{}

// Increment adds By (an Int or Double) to a numeric field. A field that is
// missing or not a number is treated as zero.
type Increment struct{ By Value }

// ArrayUnion appends the elements that are not already present.
type ArrayUnion struct{ Elements Array }

// ArrayRemove removes every occurrence of the given elements.
type ArrayRemove struct{ Elements Array }

func (t SetValue) Apply(Value, time.Time) (Value, bool)            { return t.Value, false }
func (DeleteField) Apply(Value, time.Time) (Value, bool)           { return nil, true }
func (ServerTimestamp) Apply(_ Value, now time.Time) (Value, bool) { return NewTimestamp(now), false }

func (t Increment) Apply(prev Value, _ time.Time) (Value, bool) {
	if t.By.Kind() != KindNumber {
		return prev, prev == nil // a malformed increment changes nothing
	}
	by, _ := NumberKey(t.By)
	switch p := prev.(type) {
	case Int:
		if byInt, ok := t.By.(Int); ok {
			return addSaturating(p, byInt), false
		}
		return Double(float64(p) + by), false
	case Double:
		return Double(float64(p) + by), false
	}
	return t.By, false
}

// addSaturating adds two integers, clamping at the int64 limits rather than
// wrapping around on overflow.
func addSaturating(a, b Int) Int {
	sum := a + b
	if b > 0 && sum < a {
		return math.MaxInt64
	}
	if b < 0 && sum > a {
		return math.MinInt64
	}
	return sum
}

func (t ArrayUnion) Apply(prev Value, _ time.Time) (Value, bool) {
	old, _ := prev.(Array)
	out := make(Array, len(old), len(old)+len(t.Elements))
	copy(out, old)
	for _, e := range t.Elements {
		if !out.Contains(e) {
			out = append(out, e)
		}
	}
	return out, false
}

func (t ArrayRemove) Apply(prev Value, _ time.Time) (Value, bool) {
	old, _ := prev.(Array)
	out := make(Array, 0, len(old))
	for _, e := range old {
		if !t.Elements.Contains(e) {
			out = append(out, e)
		}
	}
	return out, false
}

// Apply computes the document data that results from this write.
//
// existing is the stored document, or nil. The returned map is nil when the
// write deletes the document (or, for OpVerify, when there is none). Apply
// never modifies existing: the result shares the parts that did not change.
func (w Write) Apply(existing *Document, now time.Time) (Map, error) {
	var data Map
	switch w.Op {
	case OpDelete:
		return nil, nil
	case OpVerify:
		if existing == nil {
			return nil, nil
		}
		return existing.Data, nil
	case OpCreate:
		if existing != nil {
			return nil, fmt.Errorf("%w: document %s", ErrAlreadyExists, w.Path)
		}
		data = w.Data
	case OpSet:
		data = w.Data
	case OpMerge:
		if existing != nil {
			data = existing.Data
		}
	case OpUpdate:
		if existing == nil {
			return nil, fmt.Errorf("%w: document %s", ErrNotFound, w.Path)
		}
		data = existing.Data
	default:
		return nil, fmt.Errorf("%w: unknown write operation", ErrInvalidArgument)
	}
	if data == nil {
		data = Map{}
	}
	for _, f := range w.Fields {
		if len(f.Path) == 0 || f.Transform == nil {
			return nil, fmt.Errorf("%w: incomplete field operation", ErrInvalidArgument)
		}
		prev, _ := data.Get(f.Path)
		v, remove := f.Transform.Apply(prev, now)
		if remove {
			data = data.Without(f.Path)
		} else {
			data = data.With(f.Path, v)
		}
	}
	if err := ValidateData(data); err != nil {
		return nil, err
	}
	return data, nil
}
