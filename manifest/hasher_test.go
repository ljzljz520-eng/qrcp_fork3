package manifest

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

func waitDone(t *testing.T, h *Hasher) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.HashingDone() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("hasher did not finish in time")
}

// expectedHashes independently hashes every chunk of every file entry.
func expectedHashes(t *testing.T, m *Manifest) [][]string {
	t.Helper()
	out := make([][]string, len(m.Files))
	for fi, e := range m.Files {
		if e.Type != EntryFile {
			continue
		}
		f, err := os.Open(e.SourcePath)
		if err != nil {
			t.Fatal(err)
		}
		for ci := int64(0); ci < e.Chunks; ci++ {
			buf := make([]byte, m.ChunkSize)
			n, err := io.ReadFull(f, buf)
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				t.Fatal(err)
			}
			out[fi] = append(out[fi], HashHex(buf[:n]))
		}
		f.Close()
	}
	return out
}

func expectedRoots(t *testing.T, m *Manifest, hashes [][]string) (fileRoots []string, global string) {
	t.Helper()
	bindings := []string{}
	for fi, e := range m.Files {
		var root string
		if e.Type == EntryFile {
			r, err := MerkleRoot(hashes[fi])
			if err != nil {
				t.Fatal(err)
			}
			root = r
		}
		fileRoots = append(fileRoots, root)
		bindings = append(bindings, FileBindingHash(e.Path, e.Size, e.Mode, e.ModTime, root))
	}
	g, err := MerkleRoot(bindings)
	if err != nil {
		t.Fatal(err)
	}
	return fileRoots, g
}

func TestHasherBackgroundPass(t *testing.T) {
	root, other := buildFixture(t)
	m, err := Build([]string{root, other}, 4)
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := NewSession(time.Minute)
	h := NewHasher(m)

	// Before hashing starts: no per-file hashes, not done.
	pre := h.Snapshot(sess)
	if pre.HashingDone {
		t.Fatal("snapshot unexpectedly done before Start")
	}
	for _, f := range pre.Files {
		if f.Type == EntryFile && f.ChunkHashes != nil {
			t.Fatalf("%s: chunkHashes non-nil before hashing", f.Path)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Start(ctx)
	waitDone(t, h)

	wantHashes := expectedHashes(t, m)
	wantFileRoots, wantGlobal := expectedRoots(t, m, wantHashes)

	dto := h.Snapshot(sess)
	if !dto.HashingDone || dto.MerkleRoot != wantGlobal {
		t.Fatalf("dto done=%v root=%s want=%s", dto.HashingDone, dto.MerkleRoot, wantGlobal)
	}
	if dto.Protocol != ProtocolVersion || dto.Token != sess.Token ||
		dto.ChunkSize != 4 || !dto.ExpiresAt.Equal(sess.ExpiresAt) {
		t.Fatalf("dto session fields wrong: %+v", dto)
	}
	for fi, e := range m.Files {
		g := dto.Files[fi]
		if e.Type == EntryDir {
			if g.ChunkHashes != nil {
				t.Fatalf("dir %s has non-nil chunkHashes", e.Path)
			}
			continue
		}
		if len(g.ChunkHashes) != len(wantHashes[fi]) {
			t.Fatalf("%s: %d hashes, want %d", e.Path, len(g.ChunkHashes), len(wantHashes[fi]))
		}
		for ci, want := range wantHashes[fi] {
			if g.ChunkHashes[ci] != want {
				t.Fatalf("%s chunk %d = %s, want %s", e.Path, ci, g.ChunkHashes[ci], want)
			}
		}
		if g.MerkleRoot != wantFileRoots[fi] {
			t.Fatalf("%s file root = %s, want %s", e.Path, g.MerkleRoot, wantFileRoots[fi])
		}
	}
	// Empty file root must be sha256("").
	for _, g := range dto.Files {
		if g.Type == EntryFile && g.Chunks == 0 && g.MerkleRoot != HashHex(nil) {
			t.Fatalf("empty file %s root = %s", g.Path, g.MerkleRoot)
		}
	}
	// DTO must serialize with chunkHashes as null for unhashed entries:
	// re-check the pre-start snapshot JSON shape explicitly.
	raw, err := json.Marshal(pre)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]interface{}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	files := probe["files"].([]interface{})
	if files[0].(map[string]interface{})["chunkHashes"] != nil {
		t.Fatal("dir chunkHashes should serialize as null")
	}
}

func TestChunkHashWhileServeConcurrent(t *testing.T) {
	root, _ := buildFixture(t)
	m, err := Build([]string{root}, 4)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHasher(m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Request all chunks concurrently *before* starting the background pass.
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := make([][]string, len(m.Files))
	for fi, e := range m.Files {
		if e.Type != EntryFile {
			continue
		}
		got[fi] = make([]string, e.Chunks)
		for ci := int64(0); ci < e.Chunks; ci++ {
			wg.Add(1)
			go func(fi, ci int64) {
				defer wg.Done()
				hash, err := h.ChunkHash(ctx, fi, ci)
				if err != nil {
					t.Errorf("ChunkHash(%d,%d): %v", fi, ci, err)
					return
				}
				mu.Lock()
				got[fi][ci] = hash
				mu.Unlock()
			}(int64(fi), ci)
		}
	}
	wg.Wait()

	want := expectedHashes(t, m)
	for fi, e := range m.Files {
		if e.Type != EntryFile {
			continue
		}
		for ci := range want[fi] {
			if got[fi][ci] != want[fi][ci] {
				t.Fatalf("lazy chunk %s#%d mismatch", e.Path, ci)
			}
		}
	}

	// Starting the background pass after on-demand hashing must still
	// finalize roots without re-reading.
	h.Start(ctx)
	waitDone(t, h)
	_, wantGlobal := expectedRoots(t, m, want)
	if !h.HashingDone() || h.Snapshot(&Session{Token: "x"}).MerkleRoot != wantGlobal {
		t.Fatal("global root not finalized after lazy hashing")
	}
}

func TestChunkHashInvalidIndices(t *testing.T) {
	root, _ := buildFixture(t)
	m, _ := Build([]string{root}, 4)
	h := NewHasher(m)
	if _, err := h.ChunkHash(context.Background(), 999, 0); err == nil {
		t.Fatal("expected error for bad file index")
	}
	// First entry is the root directory.
	if _, err := h.ChunkHash(context.Background(), 0, 0); err == nil {
		t.Fatal("expected error when chunking a directory")
	}
}
