package app_test

import (
	"errors"
	"testing"
	"time"

	"github.com/bshadmehr98/candledb/internal/adapters/memory"
	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

const notesRules = `
match /notes/{id} {
	allow read: if resource == null || resource.data.public == true || resource.data.owner == request.auth.uid;
	allow create: if request.auth != null && request.resource.data.owner == request.auth.uid;
	allow update: if resource.data.owner == request.auth.uid && request.resource.data.owner == request.auth.uid;
	allow delete: if resource.data.owner == request.auth.uid;
}
match /secrets/{id} {
	allow read: if false;
}
match /staff/{id} {
	allow read, write: if get("roles/" + request.auth.uid).data.role == "admin";
}
`

func TestDefaultRulesDenyEverything(t *testing.T) {
	// No WithRules option: a Service that was not given rules must be locked.
	svc, err := app.New(ctx, memory.NewStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, user("u"), "a/b"); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("Get under default rules: %v, want ErrPermissionDenied", err)
	}
	if _, err := svc.Commit(ctx, user("u"), []domain.Write{setWrite(t, "a/b", `{}`)}); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("Commit under default rules: %v, want ErrPermissionDenied", err)
	}
}

func TestWriteRules(t *testing.T) {
	svc, _ := newService(t, notesRules)
	mine := setWrite(t, "notes/n1", `{"owner": "ann", "text": "hi"}`)

	if _, err := svc.Commit(ctx, anon, []domain.Write{mine}); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("anonymous create: %v", err)
	}
	if _, err := svc.Commit(ctx, user("bob"), []domain.Write{mine}); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("create with someone else as owner: %v", err)
	}
	commit(t, svc, user("ann"), mine)

	// The same write is now an update, and the update rule applies.
	commit(t, svc, user("ann"), setWrite(t, "notes/n1", `{"owner": "ann", "text": "edited"}`))
	if _, err := svc.Commit(ctx, user("bob"), []domain.Write{setWrite(t, "notes/n1", `{"owner": "bob"}`)}); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("update by a stranger: %v", err)
	}
	if _, err := svc.Commit(ctx, user("ann"), []domain.Write{setWrite(t, "notes/n1", `{"owner": "bob"}`)}); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("giving the note away: %v", err)
	}

	if _, err := svc.Commit(ctx, user("bob"), []domain.Write{deleteWrite("notes/n1")}); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("delete by a stranger: %v", err)
	}
	commit(t, svc, user("ann"), deleteWrite("notes/n1"))

	// One denied write sinks the whole batch.
	_, err := svc.Commit(ctx, user("ann"), []domain.Write{
		setWrite(t, "notes/ok", `{"owner": "ann"}`),
		setWrite(t, "secrets/s", `{}`),
	})
	if !errors.Is(err, domain.ErrPermissionDenied) {
		t.Fatalf("batch with a denied write: %v", err)
	}
	if _, err := svc.Get(ctx, admin, "notes/ok"); !errors.Is(err, domain.ErrNotFound) {
		t.Error("the allowed half of a denied batch was stored")
	}
}

func TestReadRules(t *testing.T) {
	svc, _ := newService(t, notesRules)
	commit(t, svc, admin,
		setWrite(t, "notes/private", `{"owner": "ann"}`),
		setWrite(t, "notes/public", `{"owner": "ann", "public": true}`),
		setWrite(t, "secrets/s", `{}`),
	)

	if _, err := svc.Get(ctx, user("bob"), "notes/public"); err != nil {
		t.Errorf("public note: %v", err)
	}
	if _, err := svc.Get(ctx, user("bob"), "notes/private"); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("someone else's private note: %v", err)
	}
	if _, err := svc.Get(ctx, user("ann"), "notes/private"); err != nil {
		t.Errorf("own private note: %v", err)
	}
	// Where reading is allowed, a missing document is reported as missing...
	if _, err := svc.Get(ctx, user("bob"), "notes/none"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing note: %v, want ErrNotFound", err)
	}
	// ...but where it is not, existing and missing look the same, so the
	// error cannot be used to probe for documents.
	_, existing := svc.Get(ctx, user("bob"), "secrets/s")
	_, missing := svc.Get(ctx, user("bob"), "secrets/none")
	if !errors.Is(existing, domain.ErrPermissionDenied) || !errors.Is(missing, domain.ErrPermissionDenied) {
		t.Errorf("secrets: existing = %v, missing = %v; both must be ErrPermissionDenied", existing, missing)
	}
	if _, err := svc.Get(ctx, admin, "secrets/s"); err != nil {
		t.Errorf("admin bypass: %v", err)
	}
}

func TestQueryReturnsOnlyReadableDocuments(t *testing.T) {
	svc, _ := newService(t, notesRules)
	commit(t, svc, admin,
		setWrite(t, "notes/a", `{"owner": "ann", "n": 1}`),
		setWrite(t, "notes/b", `{"owner": "bob", "n": 2}`),
		setWrite(t, "notes/c", `{"owner": "ann", "n": 3, "public": true}`),
		setWrite(t, "notes/d", `{"owner": "bob", "n": 4}`),
		setWrite(t, "notes/e", `{"owner": "bob", "n": 5}`),
	)
	run := func(p app.Principal, limit int) string {
		docs, err := svc.Query(ctx, p, domain.Query{Collection: "notes", Limit: limit})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		out := ""
		for _, d := range docs {
			out += d.Path.ID()
		}
		return out
	}
	if got := run(user("bob"), 0); got != "bcde" {
		t.Errorf("bob sees %q, want bcde", got)
	}
	if got := run(user("ann"), 0); got != "ac" {
		t.Errorf("ann sees %q, want ac", got)
	}
	if got := run(anon, 0); got != "c" {
		t.Errorf("anonymous sees %q, want c", got)
	}
	// The limit counts readable documents: bob's first two are b and c,
	// even though a sorts first.
	if got := run(user("bob"), 2); got != "bc" {
		t.Errorf("bob's first two = %q, want bc", got)
	}
	if got := run(admin, 0); got != "abcde" {
		t.Errorf("admin sees %q, want abcde", got)
	}
}

func TestRulesCanLookUpOtherDocuments(t *testing.T) {
	svc, _ := newService(t, notesRules)
	commit(t, svc, admin,
		setWrite(t, "roles/ann", `{"role": "admin"}`),
		setWrite(t, "roles/bob", `{"role": "user"}`),
	)
	commit(t, svc, user("ann"), setWrite(t, "staff/s1", `{}`))
	if _, err := svc.Commit(ctx, user("bob"), []domain.Write{setWrite(t, "staff/s2", `{}`)}); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("non-admin write: %v", err)
	}
	if _, err := svc.Commit(ctx, user("nobody"), []domain.Write{setWrite(t, "staff/s2", `{}`)}); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("write by a user without a role: %v", err)
	}
}

// A rule can insist on a server timestamp by comparing the field with
// request.time, because both are the commit time.
func TestRequestTimeIsTheCommitTime(t *testing.T) {
	svc, clock := newService(t, `
		match /posts/{id} {
			allow create: if request.resource.data.created == request.time;
		}`)
	stamped := domain.Write{Op: domain.OpSet, Path: "posts/p1", Data: domain.Map{},
		Fields: []domain.FieldOp{{Path: domain.FieldPath{"created"}, Transform: domain.ServerTimestamp{}}}}
	commit(t, svc, user("ann"), stamped)

	forged := domain.Write{Op: domain.OpSet, Path: "posts/p2",
		Data: domain.Map{"created": domain.NewTimestamp(clock.Now().Add(-24 * time.Hour))}}
	if _, err := svc.Commit(ctx, user("ann"), []domain.Write{forged}); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("a client-chosen timestamp was accepted: %v", err)
	}
}

func TestQueryLimits(t *testing.T) {
	svc, _ := newService(t, "", app.WithLimits(app.Limits{
		MaxBatchWrites: 100, MaxQueryResults: 3, MaxSessions: 10, MaxTargetsPerSession: 10, SessionBuffer: 10,
	}))
	commit(t, svc, anon, setWrite(t, "c/1", `{}`), setWrite(t, "c/2", `{}`), setWrite(t, "c/3", `{}`))

	if docs, err := svc.Query(ctx, anon, domain.Query{Collection: "c"}); err != nil || len(docs) != 3 {
		t.Errorf("query at the maximum: %d docs, %v", len(docs), err)
	}
	commit(t, svc, anon, setWrite(t, "c/4", `{}`))
	if _, err := svc.Query(ctx, anon, domain.Query{Collection: "c"}); !errors.Is(err, domain.ErrResourceExhausted) {
		t.Errorf("unlimited query over the maximum: %v, want ErrResourceExhausted", err)
	}
	if docs, err := svc.Query(ctx, anon, domain.Query{Collection: "c", Limit: 2}); err != nil || len(docs) != 2 {
		t.Errorf("limited query: %d docs, %v", len(docs), err)
	}
	if _, err := svc.Query(ctx, anon, domain.Query{Collection: "c", Limit: 4}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("limit above the maximum: %v, want ErrInvalidArgument", err)
	}
}
