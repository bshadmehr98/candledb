package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// This file is the translation layer between JSON on the wire and the
// domain's types. Keeping it in the adapter means the core never learns
// what the wire format looks like, and a second transport (say gRPC) could
// be added without touching it.

// ---- responses ----

type documentJSON struct {
	Path       string     `json:"path"`
	ID         string     `json:"id"`
	Data       domain.Map `json:"data"`
	CreateTime string     `json:"createTime"`
	UpdateTime string     `json:"updateTime"`
	Version    int64      `json:"version"`
}

func formatTime(t time.Time) string { return t.UTC().Format(domain.TimestampLayout) }

func toDocumentJSON(doc domain.Document) documentJSON {
	return documentJSON{
		Path:       string(doc.Path),
		ID:         doc.Path.ID(),
		Data:       doc.Data,
		CreateTime: formatTime(doc.CreateTime),
		UpdateTime: formatTime(doc.UpdateTime),
		Version:    doc.Version,
	}
}

type commitJSON struct {
	Version    int64  `json:"version"`
	CommitTime string `json:"commitTime"`
	// Path and ID are set when the server chose the document ID.
	Path string `json:"path,omitempty"`
	ID   string `json:"id,omitempty"`
}

type changeJSON struct {
	Kind app.ChangeKind `json:"kind"`
	Doc  *documentJSON  `json:"doc,omitempty"`
	Path string         `json:"path,omitempty"` // for "removed"
}

// eventJSON is one message on the listen stream.
type eventJSON struct {
	Type    string       `json:"type"` // ready, snapshot, change, error
	Session string       `json:"session,omitempty"`
	Target  *int         `json:"target,omitempty"`
	Version *int64       `json:"version,omitempty"`
	Changes []changeJSON `json:"changes,omitempty"`
	Code    string       `json:"code,omitempty"`
	Message string       `json:"message,omitempty"`
}

func toEventJSON(ev app.Event) eventJSON {
	out := eventJSON{Type: "change", Target: &ev.Target, Version: &ev.Version, Changes: []changeJSON{}}
	if ev.Reset {
		out.Type = "snapshot"
	}
	for _, c := range ev.Changes {
		if c.Kind == app.ChangeRemoved {
			out.Changes = append(out.Changes, changeJSON{Kind: c.Kind, Path: string(c.Doc.Path)})
			continue
		}
		doc := toDocumentJSON(c.Doc)
		out.Changes = append(out.Changes, changeJSON{Kind: c.Kind, Doc: &doc})
	}
	return out
}

// ---- requests ----

// decodeBody reads the request body as one JSON value into dst. Unknown
// fields are rejected: a misspelt field name becomes an error message
// instead of a silently ignored setting.
func decodeBody(r *http.Request, dst any) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err // includes *http.MaxBytesError when the body is too large
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: malformed request body: %v", domain.ErrInvalidArgument, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: unexpected data after request body", domain.ErrInvalidArgument)
	}
	return nil
}

type queryJSON struct {
	Collection string       `json:"collection"`
	Where      []filterJSON `json:"where"`
	OrderBy    []orderJSON  `json:"orderBy"`
	Limit      int          `json:"limit"`
	StartAt    *cursorJSON  `json:"startAt"`
	EndAt      *cursorJSON  `json:"endAt"`
}

type filterJSON struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	// RawMessage postpones decoding, so the value can be parsed by the
	// domain's own JSON reader, which keeps integers exact.
	Value json.RawMessage `json:"value"`
}

type orderJSON struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}

type cursorJSON struct {
	Values    []json.RawMessage `json:"values"`
	Inclusive bool              `json:"inclusive"`
}

func (qj queryJSON) toDomain() (domain.Query, error) {
	var q domain.Query
	var err error
	if q.Collection, err = domain.ParseCollectionPath(qj.Collection); err != nil {
		return q, err
	}
	for _, f := range qj.Where {
		field, err := domain.ParseFieldPath(f.Field)
		if err != nil {
			return q, err
		}
		if len(f.Value) == 0 {
			return q, fmt.Errorf("%w: filter on %s has no value", domain.ErrInvalidArgument, f.Field)
		}
		value, err := domain.ParseJSON(f.Value)
		if err != nil {
			return q, err
		}
		q.Filters = append(q.Filters, domain.Filter{Field: field, Op: domain.Operator(f.Op), Value: value})
	}
	for _, o := range qj.OrderBy {
		field, err := domain.ParseFieldPath(o.Field)
		if err != nil {
			return q, err
		}
		var desc bool
		switch o.Direction {
		case "", "asc":
		case "desc":
			desc = true
		default:
			return q, fmt.Errorf("%w: direction must be asc or desc", domain.ErrInvalidArgument)
		}
		q.Orders = append(q.Orders, domain.Order{Field: field, Desc: desc})
	}
	q.Limit = qj.Limit
	if q.Start, err = qj.StartAt.toDomain(); err != nil {
		return q, err
	}
	if q.End, err = qj.EndAt.toDomain(); err != nil {
		return q, err
	}
	return q, q.Validate()
}

// toDomain is safe to call on a nil *cursorJSON: a method with a pointer
// receiver can be invoked on nil, and here nil simply means "no cursor".
func (cj *cursorJSON) toDomain() (*domain.Cursor, error) {
	if cj == nil {
		return nil, nil
	}
	c := &domain.Cursor{Inclusive: cj.Inclusive}
	for _, raw := range cj.Values {
		v, err := domain.ParseJSON(raw)
		if err != nil {
			return nil, err
		}
		c.Values = append(c.Values, v)
	}
	return c, nil
}

type writeRequest struct {
	Op           string          `json:"op"`
	Path         string          `json:"path"`
	Data         json.RawMessage `json:"data"`
	Merge        bool            `json:"merge"`
	Precondition *struct {
		Exists  *bool  `json:"exists"`
		Version *int64 `json:"version"`
	} `json:"precondition"`
}

type commitRequest struct {
	Writes []writeRequest `json:"writes"`
}

func (wj writeRequest) toDomain() (domain.Write, error) {
	path, err := domain.ParseDocPath(wj.Path)
	if err != nil {
		return domain.Write{}, err
	}
	w := domain.Write{Path: path}
	if wj.Precondition != nil {
		w.Pre = domain.Precondition{Exists: wj.Precondition.Exists, Version: wj.Precondition.Version}
	}
	switch wj.Op {
	case "delete", "verify":
		w.Op = domain.OpDelete
		if wj.Op == "verify" {
			w.Op = domain.OpVerify
		}
		if wj.Merge || len(wj.Data) > 0 {
			return w, fmt.Errorf("%w: op %s takes no data", domain.ErrInvalidArgument, wj.Op)
		}
		return w, nil
	case "set":
		w.Op = domain.OpSet
		if wj.Merge {
			w.Op = domain.OpMerge
		}
	case "create":
		w.Op = domain.OpCreate
	case "update":
		w.Op = domain.OpUpdate
	default:
		return w, fmt.Errorf("%w: unknown write op %q (use set, create, update, delete or verify)", domain.ErrInvalidArgument, wj.Op)
	}
	if wj.Merge && wj.Op != "set" {
		return w, fmt.Errorf("%w: merge is only valid with op set", domain.ErrInvalidArgument)
	}
	return w, parseWriteData(&w, wj.Data)
}

// The reserved one-key objects that request a server-side transform, for
// example {"visits": {"$increment": 1}}.
const (
	keyServerTimestamp = "$serverTimestamp"
	keyIncrement       = "$increment"
	keyArrayUnion      = "$arrayUnion"
	keyArrayRemove     = "$arrayRemove"
	keyDelete          = "$delete"
)

// parseWriteData fills w.Data and w.Fields from the JSON data of a write.
//
//   - set and create: the data is the new document. Transforms found inside
//     it are lifted out into w.Fields and applied on top.
//   - set with merge: every leaf of the data becomes a field operation, so
//     nested maps are merged with what is stored instead of replacing it.
//   - update: the top-level keys are dotted field paths ("address.city"),
//     each replacing that field as a whole.
func parseWriteData(w *domain.Write, raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("%w: write to %s has no data", domain.ErrInvalidArgument, w.Path)
	}
	tree, err := domain.DecodeJSONTree(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	obj, ok := tree.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: document data must be a JSON object", domain.ErrInvalidArgument)
	}

	if w.Op == domain.OpUpdate {
		for _, key := range sortedKeys(obj) {
			path, err := domain.ParseFieldPath(key)
			if err != nil {
				return err
			}
			transform, err := parseTransform(obj[key], true)
			if err != nil {
				return fmt.Errorf("%w (field %s)", err, key)
			}
			if transform == nil {
				value, err := domain.FromJSON(obj[key])
				if err != nil {
					return err
				}
				transform = domain.SetValue{Value: value}
			}
			w.Fields = append(w.Fields, domain.FieldOp{Path: path, Transform: transform})
		}
		return nil
	}

	merge := w.Op == domain.OpMerge
	data, err := liftTransforms(obj, nil, merge, &w.Fields)
	if err != nil {
		return err
	}
	if !merge {
		w.Data = data
	}
	return nil
}

// liftTransforms walks a JSON object. It returns the plain data and appends
// to ops a field operation for every transform it finds, and, when
// flatten is set, for every leaf value as well.
func liftTransforms(obj map[string]any, prefix domain.FieldPath, flatten bool, ops *[]domain.FieldOp) (domain.Map, error) {
	if len(prefix) >= domain.MaxDepth {
		return nil, fmt.Errorf("%w: data is nested too deeply", domain.ErrInvalidArgument)
	}
	out := make(domain.Map, len(obj))
	for _, name := range sortedKeys(obj) {
		child := obj[name]
		// slices.Concat copies, so sibling fields never share (and
		// overwrite) one underlying array.
		path := slices.Concat(prefix, domain.FieldPath{name})

		transform, err := parseTransform(child, flatten)
		if err != nil {
			return nil, fmt.Errorf("%w (field %s)", err, path)
		}
		if transform != nil {
			if err := domain.ValidateFieldName(name); err != nil {
				return nil, err
			}
			*ops = append(*ops, domain.FieldOp{Path: path, Transform: transform})
			continue
		}

		nested, isObject := child.(map[string]any)
		_, isTimestamp := nested["$timestamp"]
		if isObject && !isTimestamp && (len(nested) > 0 || !flatten) {
			if err := domain.ValidateFieldName(name); err != nil {
				return nil, err
			}
			sub, err := liftTransforms(nested, path, flatten, ops)
			if err != nil {
				return nil, err
			}
			out[name] = sub
			continue
		}

		if err := domain.ValidateFieldName(name); err != nil {
			return nil, err
		}
		value, err := domain.FromJSON(child)
		if err != nil {
			return nil, err
		}
		out[name] = value
		if flatten {
			*ops = append(*ops, domain.FieldOp{Path: path, Transform: domain.SetValue{Value: value}})
		}
	}
	return out, nil
}

// parseTransform recognises a transform object. It returns nil, nil when
// the value is ordinary data.
func parseTransform(v any, allowDelete bool) (domain.Transform, error) {
	obj, ok := v.(map[string]any)
	if !ok || len(obj) != 1 {
		return nil, nil
	}
	for key, arg := range obj {
		switch key {
		case keyServerTimestamp:
			return domain.ServerTimestamp{}, nil
		case keyDelete:
			if !allowDelete {
				return nil, fmt.Errorf("%w: %s is only valid in an update or a merge", domain.ErrInvalidArgument, keyDelete)
			}
			return domain.DeleteField{}, nil
		case keyIncrement:
			n, err := domain.FromJSON(arg)
			if err != nil || n.Kind() != domain.KindNumber {
				return nil, fmt.Errorf("%w: %s needs a number", domain.ErrInvalidArgument, keyIncrement)
			}
			return domain.Increment{By: n}, nil
		case keyArrayUnion, keyArrayRemove:
			list, err := domain.FromJSON(arg)
			elements, isList := list.(domain.Array)
			if err != nil || !isList {
				return nil, fmt.Errorf("%w: %s needs a list", domain.ErrInvalidArgument, key)
			}
			if key == keyArrayUnion {
				return domain.ArrayUnion{Elements: elements}, nil
			}
			return domain.ArrayRemove{Elements: elements}, nil
		}
		if strings.HasPrefix(key, "$") && key != "$timestamp" {
			return nil, fmt.Errorf("%w: unknown transform %s", domain.ErrInvalidArgument, key)
		}
	}
	return nil, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

type targetRequest struct {
	ID       int        `json:"id"`
	Document string     `json:"document"`
	Query    *queryJSON `json:"query"`
}

func (tr targetRequest) toDomain() (app.Target, error) {
	switch {
	case tr.Document != "" && tr.Query != nil:
		return app.Target{}, fmt.Errorf("%w: give either document or query, not both", domain.ErrInvalidArgument)
	case tr.Document != "":
		path, err := domain.ParseDocPath(tr.Document)
		return app.Target{Doc: path}, err
	case tr.Query != nil:
		q, err := tr.Query.toDomain()
		return app.Target{Query: &q}, err
	}
	return app.Target{}, fmt.Errorf("%w: a target needs a document or a query", domain.ErrInvalidArgument)
}
