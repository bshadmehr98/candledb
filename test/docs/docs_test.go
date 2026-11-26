// Package docs_test checks the documentation against the repository, so
// that a renamed file or heading breaks a test instead of a reader's click.
package docs_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// root is the repository root, relative to this package's directory. "go
// test" always runs a test with its package directory as the working
// directory, which is what makes a relative path safe here.
const root = "../.."

var (
	// fence matches a fenced code block; links inside code are examples.
	fence = regexp.MustCompile("(?s)```.*?```")
	// link matches the target of an inline markdown link: [text](target).
	link = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	// heading matches a markdown heading line and captures its text.
	heading = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*$`)
	// notSlug matches what GitHub removes from a heading to build its anchor.
	notSlug = regexp.MustCompile(`[^\p{L}\p{N} _-]`)
)

// markdownFiles returns every markdown file in the repository, skipping
// directories that hold other people's files.
func markdownFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
	if len(files) < 30 {
		t.Fatalf("found only %d markdown files; is root wrong?", len(files))
	}
	return files
}

// anchors returns the set of anchors GitHub generates for the headings of
// a markdown document: lower case, punctuation removed, spaces to hyphens.
func anchors(text string) map[string]bool {
	set := make(map[string]bool)
	for _, m := range heading.FindAllStringSubmatch(fence.ReplaceAllString(text, ""), -1) {
		slug := strings.ToLower(m[1])
		slug = notSlug.ReplaceAllString(slug, "")
		slug = strings.ReplaceAll(slug, " ", "-")
		set[slug] = true
	}
	return set
}

// TestLinksResolve follows every relative link in every markdown file and
// fails if the file, or the heading within it, does not exist.
func TestLinksResolve(t *testing.T) {
	texts := make(map[string]string) // cleaned path -> content, read once
	read := func(path string) (string, bool) {
		path = filepath.Clean(path)
		if text, ok := texts[path]; ok {
			return text, true
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", false
		}
		texts[path] = string(data)
		return texts[path], true
	}

	for _, file := range markdownFiles(t) {
		text, ok := read(file)
		if !ok {
			t.Errorf("%s: cannot be read", file)
			continue
		}
		name := filepath.ToSlash(strings.TrimPrefix(filepath.Clean(file), filepath.Clean(root)+string(filepath.Separator)))
		for _, m := range link.FindAllStringSubmatch(fence.ReplaceAllString(text, ""), -1) {
			target := m[1]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "mailto:") {
				continue // the network is not available to tests
			}
			pathPart, anchor, _ := strings.Cut(target, "#")
			dest := file
			if pathPart != "" {
				dest = filepath.Join(filepath.Dir(file), filepath.FromSlash(pathPart))
				if _, err := os.Stat(dest); err != nil {
					t.Errorf("%s: link to %q: no such file", name, target)
					continue
				}
			}
			if anchor == "" || !strings.HasSuffix(dest, ".md") {
				continue
			}
			destText, ok := read(dest)
			if !ok {
				t.Errorf("%s: link to %q: cannot read the target", name, target)
				continue
			}
			if !anchors(destText)[anchor] {
				t.Errorf("%s: link to %q: no heading produces that anchor", name, target)
			}
		}
	}
}
