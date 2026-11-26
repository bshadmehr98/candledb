package app_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bshadmehr98/candledb/internal/adapters/memory"
	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

func TestCommitStoresDocumentWithMetadata(t *testing.T) {
	svc, clock := newService(t, "")
	created := clock.Now()

	res := commit(t, svc, anon, setWrite(t, "users/ada", `{"name": "Ada"}`))
	if res.Version != 1 || !res.Time.Equal(created) {
		t.Errorf("result = %+v", res)
	}

	clock.Advance(time.Minute)
	commit(t, svc, anon, setWrite(t, "users/ada", `{"name": "Ada L."}`))

	doc, err := svc.Get(ctx, anon, "users/ada")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != 2 {
		t.Errorf("Version = %d, want 2", doc.Version)
	}
	if !doc.CreateTime.Equal(created) {
		t.Errorf("CreateTime changed on update: %v", doc.CreateTime)
	}
	if !doc.UpdateTime.Equal(created.Add(time.Minute)) {
		t.Errorf("UpdateTime = %v", doc.UpdateTime)
	}
	if doc.Data["name"] != domain.String("Ada L.") {
		t.Errorf("data = %v", doc.Data)
	}
}

func TestCommitIsAtomic(t *testing.T) {
	svc, _ := newService(t, "")
	commit(t, svc, anon, setWrite(t, "a/exists", `{}`))

	_, err := svc.Commit(ctx, anon, []domain.Write{
		setWrite(t, "a/first", `{}`),
		{Op: domain.OpCreate, Path: "a/exists", Data: domain.Map{}}, // fails
		setWrite(t, "a/third", `{}`),
	})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("error = %v, want ErrAlreadyExists", err)
	}
	if _, err := svc.Get(ctx, anon, "a/first"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a write from a failed batch was stored (Get error = %v)", err)
	}
	if svc.Stats().Version != 1 {
		t.Errorf("a failed commit advanced the version to %d", svc.Stats().Version)
	}
}

func TestLaterWritesInABatchSeeEarlierOnes(t *testing.T) {
	svc, _ := newService(t, "")
	commit(t, svc, anon,
		domain.Write{Op: domain.OpCreate, Path: "c/doc", Data: obj(t, `{"n": 1}`)},
		incrementWrite("c/doc", "n", 5),
		incrementWrite("c/doc", "n", 10),
	)
	doc, _ := svc.Get(ctx, anon, "c/doc")
	if doc.Data["n"] != domain.Int(16) {
		t.Errorf("n = %v, want 16", doc.Data["n"])
	}

	// Create and delete in one batch leaves nothing behind.
	commit(t, svc, anon, setWrite(t, "c/temp", `{}`), deleteWrite("c/temp"))
	if _, err := svc.Get(ctx, anon, "c/temp"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("document created and deleted in one batch exists: %v", err)
	}
}

func TestCommitPreconditions(t *testing.T) {
	svc, _ := newService(t, "")
	first := commit(t, svc, anon, setWrite(t, "c/doc", `{"n": 1}`))

	stale := first.Version
	commit(t, svc, anon, setWrite(t, "c/doc", `{"n": 2}`))

	w := setWrite(t, "c/doc", `{"n": 3}`)
	w.Pre.Version = &stale
	if _, err := svc.Commit(ctx, anon, []domain.Write{w}); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("write based on a stale read: error = %v, want ErrFailedPrecondition", err)
	}

	current := first.Version + 1
	w.Pre.Version = &current
	if _, err := svc.Commit(ctx, anon, []domain.Write{w}); err != nil {
		t.Errorf("write based on the current version failed: %v", err)
	}
}

func TestCommitValidation(t *testing.T) {
	svc, _ := newService(t, "", app.WithLimits(app.Limits{
		MaxBatchWrites: 2, MaxQueryResults: 10, MaxSessions: 10, MaxTargetsPerSession: 10, SessionBuffer: 10,
	}))
	if _, err := svc.Commit(ctx, anon, nil); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("empty commit: %v", err)
	}
	three := []domain.Write{setWrite(t, "a/1", `{}`), setWrite(t, "a/2", `{}`), setWrite(t, "a/3", `{}`)}
	if _, err := svc.Commit(ctx, anon, three); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("oversized batch: %v", err)
	}
	big := domain.Write{Op: domain.OpSet, Path: "a/big", Data: domain.Map{"blob": domain.String(make([]byte, domain.MaxDocumentBytes+1))}}
	if _, err := svc.Commit(ctx, anon, []domain.Write{big}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("oversized document: %v", err)
	}
}

// TestConcurrentCommits is the test for the single-writer design. Many
// goroutines increment one counter; if two commits could interleave, some
// increments would be computed from the same old value and get lost.
func TestConcurrentCommits(t *testing.T) {
	svc, _ := newService(t, "")
	const workers, each = 20, 25

	versions := make(chan int64, workers*each)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range each {
				res, err := svc.Commit(ctx, anon, []domain.Write{incrementWrite("counters/c", "n", 1)})
				if err != nil {
					t.Errorf("Commit: %v", err)
					return
				}
				versions <- res.Version
			}
		})
	}
	wg.Wait()
	close(versions)

	doc, err := svc.Get(ctx, anon, "counters/c")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Data["n"] != domain.Int(workers*each) {
		t.Errorf("counter = %v, want %d: increments were lost", doc.Data["n"], workers*each)
	}
	seen := map[int64]bool{}
	for v := range versions {
		if seen[v] {
			t.Fatalf("version %d was handed to two commits", v)
		}
		seen[v] = true
	}
	if len(seen) != workers*each {
		t.Errorf("%d distinct versions, want %d", len(seen), workers*each)
	}
}

func TestVersionContinuesAfterRestart(t *testing.T) {
	store := memory.NewStore()
	first, err := app.New(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := first.Commit(ctx, admin, []domain.Write{setWrite(t, "a/b", `{}`)}); err != nil {
			t.Fatal(err)
		}
	}

	// A second Service on the same store stands in for a restarted server.
	second, err := app.New(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	res, err := second.Commit(ctx, admin, []domain.Write{setWrite(t, "a/b", `{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 4 {
		t.Errorf("first version after restart = %d, want 4", res.Version)
	}
}

func TestVerifyGuardsDocumentsThatWereOnlyRead(t *testing.T) {
	svc, _ := newService(t, notesRules)
	commit(t, svc, admin, setWrite(t, "notes/config", `{"owner": "ann", "rate": 2}`))
	seen := int64(1)

	// A transaction read notes/config and writes notes/result. The verify
	// makes the commit fail if the config changed after it was read.
	transaction := []domain.Write{
		{Op: domain.OpVerify, Path: "notes/config", Pre: domain.Precondition{Version: &seen}},
		setWrite(t, "notes/result", `{"owner": "ann", "value": 20}`),
	}
	res := commit(t, svc, user("ann"), transaction...)

	config, _ := svc.Get(ctx, admin, "notes/config")
	if config.Version != 1 {
		t.Errorf("verify changed the document: version %d", config.Version)
	}
	if res.Version != 2 {
		t.Errorf("commit version = %d", res.Version)
	}

	commit(t, svc, admin, setWrite(t, "notes/config", `{"owner": "ann", "rate": 3}`))
	if _, err := svc.Commit(ctx, user("ann"), transaction); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("verify of a changed document: %v, want ErrFailedPrecondition", err)
	}

	// Verifying needs read permission, or it would reveal the version of a
	// document the caller may not see.
	if _, err := svc.Commit(ctx, user("bob"), []domain.Write{{Op: domain.OpVerify, Path: "notes/config"}}); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("verify without read access: %v, want ErrPermissionDenied", err)
	}
}
