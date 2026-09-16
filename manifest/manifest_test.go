package manifest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildFixture creates:
//
//	root/
//	  a.txt              ("hello")
//	  empty.txt          (empty file)
//	  emptydir/          (empty directory)
//	  link -> a.txt      (symlink, must be skipped)
//	  sub/
//	    b.bin            (chunkSize+10 bytes)
//	otherroot/inside.txt
func buildFixture(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.bin"), make([]byte, 14), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(tmp, "otherroot")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "inside.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, other
}

type expectedEntry struct {
	path   string
	typ    EntryType
	size   int64
	chunks int64
}

func TestBuildDirectoryManifest(t *testing.T) {
	root, other := buildFixture(t)
	m, err := Build([]string{root, other}, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []expectedEntry{
		{"root", EntryDir, 0, 0},
		{"root/a.txt", EntryFile, 5, 2},
		{"root/empty.txt", EntryFile, 0, 0},
		{"root/emptydir", EntryDir, 0, 0},
		{"root/sub", EntryDir, 0, 0},
		{"root/sub/b.bin", EntryFile, 14, 4},
		{"otherroot", EntryDir, 0, 0},
		{"otherroot/inside.txt", EntryFile, 5, 2},
	}
	if len(m.Files) != len(want) {
		t.Fatalf("entries = %d, want %d: %+v", len(m.Files), len(want), pathsOf(m))
	}
	for i, w := range want {
		got := m.Files[i]
		if got.Index != i {
			t.Errorf("entry %d: index = %d", i, got.Index)
		}
		if got.Path != w.path || got.Type != w.typ || got.Size != w.size || got.Chunks != w.chunks {
			t.Errorf("entry %d = {%s %s size=%d chunks=%d}, want {%s %s size=%d chunks=%d}",
				i, got.Path, got.Type, got.Size, got.Chunks, w.path, w.typ, w.size, w.chunks)
		}
		if got.Type == EntryFile && got.SourcePath == "" {
			t.Errorf("entry %d: SourcePath empty", i)
		}
	}
}

func TestBuildSingleFileAndDedup(t *testing.T) {
	root, other := buildFixture(t)
	// Two arguments with the same basename "a.txt".
	a := filepath.Join(root, "a.txt")
	otherA := filepath.Join(other, "a.txt")
	if err := os.WriteFile(otherA, []byte("xx"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Build([]string{a, otherA}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 2 {
		t.Fatalf("entries = %d, want 2", len(m.Files))
	}
	if m.Files[0].Path != "a.txt" || m.Files[1].Path != "a(2).txt" {
		t.Fatalf("dedup paths = %q, %q", m.Files[0].Path, m.Files[1].Path)
	}
}

func TestBuildInvalidChunkSize(t *testing.T) {
	root, _ := buildFixture(t)
	if _, err := Build([]string{root}, 0); err == nil {
		t.Fatal("expected error for chunk size 0")
	}
	if _, err := Build([]string{"does-not-exist"}, 4); err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestBuildSkipsTopLevelSymlinks(t *testing.T) {
	root, _ := buildFixture(t)
	linkFile := filepath.Join(t.TempDir(), "link-to-file")
	if err := os.Symlink(filepath.Join(root, "a.txt"), linkFile); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(t.TempDir(), "link-to-dir")
	if err := os.Symlink(root, linkDir); err != nil {
		t.Fatal(err)
	}
	// A symlink passed alongside a real file is skipped entirely.
	m, err := Build([]string{linkFile, filepath.Join(root, "a.txt")}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 1 || m.Files[0].Path != "a.txt" {
		t.Fatalf("want exactly one a.txt entry, got %+v", pathsOf(m))
	}
	// Only-symlink arguments produce an explicit error instead of an empty
	// manifest or a phantom directory entry.
	if _, err := Build([]string{linkFile, linkDir}, 4); err == nil {
		t.Fatal("expected error when all arguments are symlinks")
	}
}

func TestBuildCreatesNoZip(t *testing.T) {
	root, _ := buildFixture(t)
	before, err := filepath.Glob(filepath.Join(os.TempDir(), "*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build([]string{root}, 4); err != nil {
		t.Fatal(err)
	}
	after, err := filepath.Glob(filepath.Join(os.TempDir(), "*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("zip files appeared in temp dir: before=%d after=%d", len(before), len(after))
	}
}

func TestNewSessionAndExpiry(t *testing.T) {
	s, err := NewSession(50 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Token) != 64 {
		t.Fatalf("token length = %d, want 64", len(s.Token))
	}
	if s.Expired(time.Now()) {
		t.Fatal("fresh session reported expired")
	}
	time.Sleep(80 * time.Millisecond)
	if !s.Expired(time.Now()) {
		t.Fatal("session past TTL not reported expired")
	}
	seen := map[string]bool{s.Token: true}
	for i := 0; i < 100; i++ {
		other, err := NewSession(time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if seen[other.Token] {
			t.Fatal("duplicate session token generated")
		}
		seen[other.Token] = true
	}
}

func pathsOf(m *Manifest) []string {
	out := make([]string, len(m.Files))
	for i, e := range m.Files {
		out[i] = e.Path
	}
	return out
}
