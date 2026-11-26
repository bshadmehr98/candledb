package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bshadmehr98/candledb/internal/adapters/sqlite"
	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/app/storetest"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// openDB creates a database in a temporary directory that the testing package
// deletes when the test ends. t.Cleanup registers the close to run at that
// point too, so tests need no teardown code of their own.
//
// It accepts testing.TB, the interface shared by *testing.T and
// *testing.B, so tests and benchmarks can use the same helper.
func openDB(tb testing.TB) *sqlite.DB {
	tb.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(tb.TempDir(), "test.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })
	return db
}

func TestStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) app.Store { return openDB(t) })
}

func TestUserStoreContract(t *testing.T) {
	storetest.RunUsers(t, func(t *testing.T) app.UserStore { return openDB(t).Users() })
}

func TestDataSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	doc := &domain.Document{Path: "users/ada", Data: domain.Map{"name": domain.String("Ada")}, Version: 7}
	if err := db.Apply(ctx, 7, []app.Mutation{{Path: doc.Path, Doc: doc}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening again must find the data and must not re-run the schema
	// migrations (which would fail on the existing tables).
	db, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	got, err := db.Get(ctx, "users/ada")
	if err != nil || got.Data["name"] != domain.String("Ada") {
		t.Errorf("after reopen: %+v, %v", got, err)
	}
	if v, _ := db.Version(ctx); v != 7 {
		t.Errorf("version after reopen = %d, want 7", v)
	}
}

func TestFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows has no owner/group/other permission bits; access there is
		// governed by the ACL the file inherits from its directory.
		t.Skip("Unix file permissions do not exist on Windows")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := db.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	// Both files hold password hashes, so only the owner may read them.
	for _, file := range []string{path, backup} {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has permissions %o, want 600", filepath.Base(file), perm)
		}
	}
}

func TestBackup(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	doc := &domain.Document{Path: "users/ada", Data: domain.Map{"n": domain.Int(1)}, Version: 1}
	if err := db.Apply(ctx, 1, []app.Mutation{{Path: doc.Path, Doc: doc}}); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := db.Backup(ctx, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	copyDB, err := sqlite.Open(ctx, dest)
	if err != nil {
		t.Fatalf("opening the backup: %v", err)
	}
	defer copyDB.Close()
	if _, err := copyDB.Get(ctx, "users/ada"); err != nil {
		t.Errorf("document missing from the backup: %v", err)
	}
}
