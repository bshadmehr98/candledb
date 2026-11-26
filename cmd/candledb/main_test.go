//go:build unix

// The end-to-end test sends itself SIGHUP with syscall.Kill, which exists
// only on Unix-like systems. The build constraint above keeps the package
// compiling for Windows, where these tests are skipped.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer that is safe to write from the server's
// goroutines while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls until cond is true. Polling with a deadline is how a test
// waits for something that happens in another goroutine at an unknown time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSimpleCommands(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"version"}, &out, &errOut); err != nil || strings.TrimSpace(out.String()) != version {
		t.Errorf("version: %q, %v", out.String(), err)
	}
	if err := run(context.Background(), nil, &out, &errOut); err == nil {
		t.Error("no arguments should be an error")
	}
	if err := run(context.Background(), []string{"frobnicate"}, &out, &errOut); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("unknown command: %v", err)
	}
}

func TestCheckRules(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.rules")
	bad := filepath.Join(dir, "bad.rules")
	os.WriteFile(good, []byte(`match /a/{b} { allow read: if request.auth != null; }`), 0o600)
	os.WriteFile(bad, []byte("match /a/{b} {\n  allow read: if nobody;\n}"), 0o600)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"check-rules", good}, &out, io.Discard); err != nil {
		t.Errorf("valid rules: %v", err)
	}
	if !strings.Contains(out.String(), "ok") {
		t.Errorf("output = %q", out.String())
	}
	err := run(context.Background(), []string{"check-rules", bad}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "2:18") || !strings.Contains(err.Error(), "unknown name nobody") {
		t.Errorf("invalid rules: %v; want an error naming line 2, column 18", err)
	}
	if err := run(context.Background(), []string{"check-rules", filepath.Join(dir, "missing")}, &out, io.Discard); err == nil {
		t.Error("a missing file should be an error")
	}
}

func TestServeFlagValidation(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":     {"serve", "-nope"},
		"cert without key": {"serve", "-tls-cert", "x.pem"},
		"short admin key":  {"serve", "-admin-key", "short"},
		"bad log level":    {"serve", "-data-dir", t.TempDir(), "-log-level", "loud"},
		"bad log format":   {"serve", "-data-dir", t.TempDir(), "-log-format", "xml"},
		"short jwt secret": {"serve", "-data-dir", t.TempDir(), "-jwt-secret", "short", "-addr", "127.0.0.1:0"},
		"missing rules":    {"serve", "-data-dir", t.TempDir(), "-rules", "/no/such/file", "-addr", "127.0.0.1:0"},
		"zero auth rate":   {"serve", "-auth-rate", "0"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(context.Background(), args, io.Discard, io.Discard); err == nil {
				t.Error("serve started with invalid configuration")
			}
		})
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "candledb.rules"), []byte("match /a {"), 0o600)
	if err := run(context.Background(), []string{"serve", "-data-dir", dir, "-addr", "127.0.0.1:0"}, io.Discard, io.Discard); err == nil {
		t.Error("serve started with a rules file that does not parse")
	}
}

// TestServeEndToEnd runs the real "serve" command: it starts on a free
// port, answers requests under a rules file, reloads the rules on SIGHUP,
// shuts down cleanly when its context is cancelled, and can be backed up.
func TestServeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	rulesPath := filepath.Join(dir, "candledb.rules")
	os.WriteFile(rulesPath, []byte(`match /notes/{id} { allow read; }`), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve", "-addr", "127.0.0.1:0", "-data-dir", dir,
			"-admin-key", "end-to-end-admin-key-0123456789", "-log-format", "json"}, io.Discard, logs)
	}()

	// The server logs the address it bound; that is how we learn the port.
	addrPattern := regexp.MustCompile(`"addr":"([^"]+)"`)
	waitFor(t, "the server to start", func() bool { return addrPattern.MatchString(logs.String()) })
	base := "http://" + addrPattern.FindStringSubmatch(logs.String())[1]

	request := func(method, path, token, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(raw)
	}

	if status, _ := request("GET", "/healthz", "", ""); status != 200 {
		t.Fatalf("healthz = %d", status)
	}
	if status, body := request("PUT", "/v1/documents/notes/n1", "", `{"text": "hi"}`); status != 403 {
		t.Errorf("write under read-only rules = %d %s", status, body)
	}
	if status, body := request("PUT", "/v1/documents/notes/n1", "end-to-end-admin-key-0123456789", `{"text": "hi"}`); status != 200 {
		t.Fatalf("admin write = %d %s", status, body)
	}
	if status, _ := request("GET", "/v1/documents/notes/n1", "", ""); status != 200 {
		t.Errorf("read = %d", status)
	}

	// The generated signing secret is private to the server's user.
	info, err := os.Stat(filepath.Join(dir, "jwt.secret"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("jwt.secret: %v, mode %v; want 0600", err, info.Mode().Perm())
	}

	// Reload: open up writes, signal, and the same request now succeeds.
	os.WriteFile(rulesPath, []byte(`match /notes/{id} { allow read, write; }`), 0o600)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	waitFor(t, "the rules to reload", func() bool {
		status, _ := request("PUT", "/v1/documents/notes/n2", "", `{"text": "open"}`)
		return status == 200
	})

	// A broken rules file is rejected and the working rules stay active.
	os.WriteFile(rulesPath, []byte(`match /notes/{id} { allow read, write: if ; }`), 0o600)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	waitFor(t, "the failed reload to be logged", func() bool {
		return strings.Contains(logs.String(), "rules reload failed")
	})
	if status, _ := request("PUT", "/v1/documents/notes/n3", "", `{}`); status != 200 {
		t.Errorf("after a failed reload, write = %d; the previous rules should still apply", status)
	}

	// Backup while the server is running.
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"backup", "-data-dir", dir, backupPath}, &out, io.Discard); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if info, err := os.Stat(backupPath); err != nil || info.Size() == 0 {
		t.Errorf("backup file: %v", err)
	}
	if err := run(context.Background(), []string{"backup", "-data-dir", dir, backupPath}, &out, io.Discard); err == nil {
		t.Error("backup overwrote an existing file")
	}

	// Shutdown with a listen stream open: it must not hold the server up.
	stream, err := http.Get(base + "/v1/listen")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	first := make([]byte, 256)
	n, _ := stream.Body.Read(first)
	var ready struct{ Type string }
	json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(first[:n]), []byte("data: ")), &ready)
	if ready.Type != "ready" {
		t.Errorf("first stream message = %q", first[:n])
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v after a clean shutdown", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not shut down")
	}

	// The data is in the backup: serve it and read the document back.
	restoreDir := t.TempDir()
	data, _ := os.ReadFile(backupPath)
	os.WriteFile(filepath.Join(restoreDir, "candledb.db"), data, 0o600)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	logs2 := &syncBuffer{}
	done2 := make(chan error, 1)
	go func() {
		done2 <- run(ctx2, []string{"serve", "-addr", "127.0.0.1:0", "-data-dir", restoreDir, "-dev-open-rules", "-log-format", "json"}, io.Discard, logs2)
	}()
	waitFor(t, "the restored server to start", func() bool { return addrPattern.MatchString(logs2.String()) })
	base = "http://" + addrPattern.FindStringSubmatch(logs2.String())[1]
	if status, body := request("GET", "/v1/documents/notes/n1", "", ""); status != 200 || !strings.Contains(body, `"text":"hi"`) {
		t.Errorf("document from the backup = %d %s", status, body)
	}
	cancel2()
	<-done2
}
