package manifest

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strconv"
	"sync"
)

// ErrSourceChanged is returned when a source file's size no longer matches
// the manifest snapshot: the file was modified (grown/shrunk/replaced)
// after the transfer started, so its bytes can no longer match the
// manifest. The transfer must be restarted by the sender.
var ErrSourceChanged = errors.New("manifest: source file changed since the manifest was built")

// Hasher computes and caches per-chunk SHA-256 digests, per-file Merkle roots
// and the global manifest Merkle root. Hashing happens in the background but
// a chunk requested before the background pass reaches it is hashed on
// demand and cached (hash-while-serve), so a chunk is never read twice.
type Hasher struct {
	m *Manifest

	mu          sync.RWMutex
	hashes      [][]string // hashes[fi][ci], empty string until cached
	fileHashed  []bool     // entry finished by the background pass
	roots       []string   // file Merkle roots ("" for directories)
	root        string     // global Merkle root ("" until done)
	done        bool
	fatalErr    string // permanent background-pass failure, surfaced via DTO
	doneChunks  int64
	totalChunks int64

	startOnce sync.Once

	flightMu sync.Mutex
	flights  map[string]*hashFlight
}

type hashFlight struct {
	done chan struct{}
	hash string
	err  error
}

// NewHasher creates a Hasher for m.
func NewHasher(m *Manifest) *Hasher {
	h := &Hasher{
		m:          m,
		hashes:     make([][]string, len(m.Files)),
		fileHashed: make([]bool, len(m.Files)),
		roots:      make([]string, len(m.Files)),
		flights:    map[string]*hashFlight{},
	}
	for fi, e := range m.Files {
		if e.Type == EntryFile {
			h.hashes[fi] = make([]string, e.Chunks)
			h.totalChunks += e.Chunks
		}
	}
	return h
}

// Start launches the background hashing goroutine at most once.
func (h *Hasher) Start(ctx context.Context) {
	h.startOnce.Do(func() {
		go h.run(ctx)
	})
}

// HashingDone reports whether the global root is available.
func (h *Hasher) HashingDone() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.done
}

// ChunkHash returns the SHA-256 hex digest of chunk ci of file fi, computing
// and caching it on demand when necessary. Concurrent requests for the same
// chunk are coalesced.
func (h *Hasher) ChunkHash(ctx context.Context, fi, ci int64) (string, error) {
	if fi < 0 || int(fi) >= len(h.m.Files) {
		return "", &EntryNotFoundError{FileIndex: fi}
	}
	e := h.m.Files[fi]
	if e.Type != EntryFile || ci < 0 || ci >= e.Chunks {
		return "", &ChunkNotFoundError{FileIndex: fi, ChunkIndex: ci}
	}
	h.mu.RLock()
	if cached := h.hashes[fi][ci]; cached != "" {
		h.mu.RUnlock()
		return cached, nil
	}
	h.mu.RUnlock()

	key := flightKey(fi, ci)
	h.flightMu.Lock()
	if fl, ok := h.flights[key]; ok {
		h.flightMu.Unlock()
		select {
		case <-fl.done:
			return fl.hash, fl.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	fl := &hashFlight{done: make(chan struct{})}
	h.flights[key] = fl
	h.flightMu.Unlock()

	hash, err := h.computeChunkHash(fi, ci)
	if err == nil {
		h.mu.Lock()
		if h.hashes[fi][ci] == "" {
			h.hashes[fi][ci] = hash
			h.doneChunks++
		} else {
			hash = h.hashes[fi][ci]
		}
		h.mu.Unlock()
	}

	h.flightMu.Lock()
	fl.hash = hash
	fl.err = err
	close(fl.done)
	delete(h.flights, key)
	h.flightMu.Unlock()
	return hash, err
}

// computeChunkHash reads exactly one chunk from disk and hashes it. The
// returned slice is never larger than ChunkSize.
func (h *Hasher) computeChunkHash(fi, ci int64) (string, error) {
	e := h.m.Files[fi]
	f, err := os.Open(e.SourcePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// Reject a source file whose size changed after the manifest snapshot,
	// otherwise growth within the same chunk count would silently pass the
	// hash/root verification with bytes that no longer match the sender's
	// file (TOCTOU guard, FR-2).
	if info, err := f.Stat(); err != nil || info.Size() != e.Size {
		return "", ErrSourceChanged
	}
	if _, err := f.Seek(ci*h.m.ChunkSize, io.SeekStart); err != nil {
		return "", err
	}
	buf := make([]byte, h.m.ChunkSize)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	return HashHex(buf[:n]), nil
}

// run is the sequential background hashing pass.
func (h *Hasher) run(ctx context.Context) {
	for fi, e := range h.m.Files {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if e.Type == EntryDir {
			h.finishEntry(fi, "")
			continue
		}
		var failed error
		for ci := int64(0); ci < e.Chunks; ci++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if _, err := h.ChunkHash(ctx, int64(fi), ci); err != nil {
				failed = err
				break
			}
		}
		if failed != nil {
			log.Printf("qctp: background hashing of %q failed: %v", e.SourcePath, failed)
			h.setFatal("源文件 " + e.Path + " 校验和计算失败: " + failed.Error())
			return
		}
		h.mu.RLock()
		leaves := make([]string, len(h.hashes[fi]))
		copy(leaves, h.hashes[fi])
		h.mu.RUnlock()
		root, err := MerkleRoot(leaves)
		if err != nil {
			log.Printf("qctp: merkle root of %q failed: %v", e.SourcePath, err)
			h.setFatal("文件 " + e.Path + " 的 Merkle root 计算失败")
			return
		}
		h.finishEntry(fi, root)
	}
	leaves := make([]string, 0, len(h.m.Files))
	h.mu.RLock()
	for fi, e := range h.m.Files {
		if !h.fileHashed[fi] {
			// An entry failed background hashing; the global root cannot be
			// finalized. On-demand serving still works for reachable chunks.
			h.mu.RUnlock()
			h.setFatal("存在校验和计算失败的条目，无法完成完整性验证，请重新发起传输")
			return
		}
		leaves = append(leaves, FileBindingHash(e.Path, e.Size, e.Mode, e.ModTime, h.roots[fi]))
	}
	h.mu.RUnlock()
	root, err := MerkleRoot(leaves)
	if err != nil {
		log.Printf("qctp: global merkle root failed: %v", err)
		h.setFatal("全局 Merkle root 计算失败")
		return
	}
	h.mu.Lock()
	h.root = root
	h.done = true
	h.mu.Unlock()
}

func (h *Hasher) finishEntry(fi int, root string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.fileHashed[fi] {
		h.fileHashed[fi] = true
		h.roots[fi] = root
	}
}

// setFatal records a permanent background-pass failure so it can be surfaced
// to polling clients via the manifest DTO.
func (h *Hasher) setFatal(msg string) {
	h.mu.Lock()
	if h.fatalErr == "" {
		h.fatalErr = msg
	}
	h.mu.Unlock()
}

// Snapshot returns the serializable view of the manifest.
func (h *Hasher) Snapshot(s *Session) DTO {
	h.mu.RLock()
	defer h.mu.RUnlock()
	progress := 1.0
	if h.totalChunks > 0 {
		progress = float64(h.doneChunks) / float64(h.totalChunks)
	}
	dto := DTO{
		Protocol:        ProtocolVersion,
		Token:           s.Token,
		CreatedAt:       s.CreatedAt,
		ExpiresAt:       s.ExpiresAt,
		ChunkSize:       h.m.ChunkSize,
		HashingDone:     h.done,
		HashingProgress: progress,
		MerkleRoot:      h.root,
		HashingError:    h.fatalErr,
		Files:           make([]FileEntryDTO, len(h.m.Files)),
	}
	for fi, e := range h.m.Files {
		fd := FileEntryDTO{
			Index:   e.Index,
			Path:    e.Path,
			Type:    e.Type,
			Size:    e.Size,
			Mode:    e.Mode,
			ModTime: e.ModTime,
			Chunks:  e.Chunks,
		}
		if e.Type == EntryFile && h.fileHashed[fi] {
			fd.ChunkHashes = make([]string, len(h.hashes[fi]))
			copy(fd.ChunkHashes, h.hashes[fi])
			fd.MerkleRoot = h.roots[fi]
		}
		dto.Files[fi] = fd
	}
	return dto
}

func flightKey(fi, ci int64) string {
	return strconv.FormatInt(fi, 36) + "|" + strconv.FormatInt(ci, 36)
}

// EntryNotFoundError indicates an invalid file index.
type EntryNotFoundError struct{ FileIndex int64 }

func (e *EntryNotFoundError) Error() string {
	return "manifest: file entry not found"
}

// ChunkNotFoundError indicates an invalid chunk index.
type ChunkNotFoundError struct {
	FileIndex  int64
	ChunkIndex int64
}

func (e *ChunkNotFoundError) Error() string {
	return "manifest: chunk not found"
}
