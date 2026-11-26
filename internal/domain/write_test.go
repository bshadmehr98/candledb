package domain_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/bshadmehr98/candledb/internal/domain"
)

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// mustMap parses a JSON object into a Map, failing the test on bad input.
// t.Helper() makes a failure point at the caller's line instead of here.
func mustMap(t *testing.T, s string) domain.Map {
	t.Helper()
	m, err := domain.ParseMapJSON([]byte(s))
	if err != nil {
		t.Fatalf("bad test data %s: %v", s, err)
	}
	return m
}

func fp(s string) domain.FieldPath {
	p, err := domain.ParseFieldPath(s)
	if err != nil {
		panic(err)
	}
	return p
}

func TestWriteApply(t *testing.T) {
	stored := func(t *testing.T) *domain.Document {
		return &domain.Document{Path: "c/d", Data: mustMap(t, `{"n": 1, "tags": ["a", "b"], "nested": {"x": 1, "y": 2}}`)}
	}
	set := func(v string) domain.Transform {
		parsed, err := domain.ParseJSON([]byte(v))
		if err != nil {
			panic(err)
		}
		return domain.SetValue{Value: parsed}
	}

	tests := []struct {
		name     string
		existing bool
		write    domain.Write
		want     string // expected data as JSON; "" means the document is deleted
		wantErr  error
	}{
		{
			name:     "set replaces everything",
			existing: true,
			write:    domain.Write{Op: domain.OpSet, Data: domain.Map{"only": domain.Bool(true)}},
			want:     `{"only": true}`,
		},
		{
			name:  "set creates",
			write: domain.Write{Op: domain.OpSet, Data: domain.Map{"a": domain.Int(1)}},
			want:  `{"a": 1}`,
		},
		{
			name:     "create fails when the document exists",
			existing: true,
			write:    domain.Write{Op: domain.OpCreate, Data: domain.Map{}},
			wantErr:  domain.ErrAlreadyExists,
		},
		{
			name:    "update fails when the document is missing",
			write:   domain.Write{Op: domain.OpUpdate, Fields: []domain.FieldOp{{Path: fp("a"), Transform: set(`1`)}}},
			wantErr: domain.ErrNotFound,
		},
		{
			name:     "update changes a nested field and keeps its siblings",
			existing: true,
			write:    domain.Write{Op: domain.OpUpdate, Fields: []domain.FieldOp{{Path: fp("nested.x"), Transform: set(`9`)}}},
			want:     `{"n": 1, "tags": ["a", "b"], "nested": {"x": 9, "y": 2}}`,
		},
		{
			name:  "merge creates a missing document",
			write: domain.Write{Op: domain.OpMerge, Fields: []domain.FieldOp{{Path: fp("a.b"), Transform: set(`1`)}}},
			want:  `{"a": {"b": 1}}`,
		},
		{
			name:     "delete field",
			existing: true,
			write:    domain.Write{Op: domain.OpUpdate, Fields: []domain.FieldOp{{Path: fp("nested.y"), Transform: domain.DeleteField{}}}},
			want:     `{"n": 1, "tags": ["a", "b"], "nested": {"x": 1}}`,
		},
		{
			name:     "increment an integer",
			existing: true,
			write:    domain.Write{Op: domain.OpUpdate, Fields: []domain.FieldOp{{Path: fp("n"), Transform: domain.Increment{By: domain.Int(41)}}}},
			want:     `{"n": 42, "tags": ["a", "b"], "nested": {"x": 1, "y": 2}}`,
		},
		{
			name:     "increment by a double makes a double",
			existing: true,
			write:    domain.Write{Op: domain.OpUpdate, Fields: []domain.FieldOp{{Path: fp("n"), Transform: domain.Increment{By: domain.Double(0.5)}}}},
			want:     `{"n": 1.5, "tags": ["a", "b"], "nested": {"x": 1, "y": 2}}`,
		},
		{
			name:     "increment a missing field starts from zero",
			existing: true,
			write:    domain.Write{Op: domain.OpUpdate, Fields: []domain.FieldOp{{Path: fp("fresh"), Transform: domain.Increment{By: domain.Int(3)}}}},
			want:     `{"fresh": 3, "n": 1, "tags": ["a", "b"], "nested": {"x": 1, "y": 2}}`,
		},
		{
			name:     "array union adds only new elements",
			existing: true,
			write: domain.Write{Op: domain.OpUpdate, Fields: []domain.FieldOp{{Path: fp("tags"),
				Transform: domain.ArrayUnion{Elements: domain.Array{domain.String("b"), domain.String("c")}}}}},
			want: `{"n": 1, "tags": ["a", "b", "c"], "nested": {"x": 1, "y": 2}}`,
		},
		{
			name:     "array remove",
			existing: true,
			write: domain.Write{Op: domain.OpUpdate, Fields: []domain.FieldOp{{Path: fp("tags"),
				Transform: domain.ArrayRemove{Elements: domain.Array{domain.String("a")}}}}},
			want: `{"n": 1, "tags": ["b"], "nested": {"x": 1, "y": 2}}`,
		},
		{
			name:  "server timestamp",
			write: domain.Write{Op: domain.OpSet, Data: domain.Map{}, Fields: []domain.FieldOp{{Path: fp("at"), Transform: domain.ServerTimestamp{}}}},
			want:  `{"at": {"$timestamp": "2026-01-02T03:04:05Z"}}`,
		},
		{
			name:     "delete",
			existing: true,
			write:    domain.Write{Op: domain.OpDelete},
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var existing *domain.Document
			var before []byte
			if tt.existing {
				existing = stored(t)
				before, _ = json.Marshal(existing.Data)
			}
			tt.write.Path = "c/d"

			got, err := tt.write.Apply(existing, now)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if tt.want == "" {
				if got != nil {
					t.Fatalf("expected a delete, got %#v", got)
				}
				return
			}
			if want := mustMap(t, tt.want); !domain.Equal(got, want) {
				gotJSON, _ := json.Marshal(got)
				t.Errorf("data = %s\nwant   %s", gotJSON, tt.want)
			}
			if existing != nil {
				if after, _ := json.Marshal(existing.Data); string(after) != string(before) {
					t.Errorf("Apply modified the stored document: %s -> %s", before, after)
				}
			}
		})
	}
}

func TestIncrementSaturates(t *testing.T) {
	up, _ := domain.Increment{By: domain.Int(10)}.Apply(domain.Int(math.MaxInt64-1), now)
	if up != domain.Int(math.MaxInt64) {
		t.Errorf("overflow gave %v", up)
	}
	down, _ := domain.Increment{By: domain.Int(-10)}.Apply(domain.Int(math.MinInt64+1), now)
	if down != domain.Int(math.MinInt64) {
		t.Errorf("underflow gave %v", down)
	}
}

func TestIncrementWithoutANumberChangesNothing(t *testing.T) {
	// The HTTP layer rejects such an increment, but the domain must not
	// depend on its callers having checked.
	bad := domain.Increment{By: domain.String("1")}
	if v, remove := bad.Apply(domain.Int(5), now); v != domain.Int(5) || remove {
		t.Errorf("existing field became %v (remove = %v)", v, remove)
	}
	if _, remove := bad.Apply(nil, now); !remove {
		t.Error("a missing field was created")
	}
}

func TestPrecondition(t *testing.T) {
	yes, no := true, false
	v3, v4 := int64(3), int64(4)
	doc := &domain.Document{Version: 3}

	tests := []struct {
		name     string
		pre      domain.Precondition
		existing *domain.Document
		ok       bool
	}{
		{"none, missing", domain.Precondition{}, nil, true},
		{"none, present", domain.Precondition{}, doc, true},
		{"must exist, present", domain.Precondition{Exists: &yes}, doc, true},
		{"must exist, missing", domain.Precondition{Exists: &yes}, nil, false},
		{"must not exist, missing", domain.Precondition{Exists: &no}, nil, true},
		{"must not exist, present", domain.Precondition{Exists: &no}, doc, false},
		{"version matches", domain.Precondition{Version: &v3}, doc, true},
		{"version differs", domain.Precondition{Version: &v4}, doc, false},
		{"version, missing", domain.Precondition{Version: &v3}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.pre.Check(tt.existing)
			if (err == nil) != tt.ok {
				t.Errorf("Check = %v, want ok = %v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, domain.ErrFailedPrecondition) {
				t.Errorf("error %v does not wrap ErrFailedPrecondition", err)
			}
		})
	}
}
