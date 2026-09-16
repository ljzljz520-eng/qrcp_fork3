// Package manifest implements the qrcp chunked transfer protocol (QCTP v1):
// manifest construction, SHA-256/Merkle hashing and session metadata.
package manifest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProtocolVersion is the version of the chunked transfer protocol served
// by this build of qrcp.
const ProtocolVersion = "qctp/1"

// EntryType distinguishes file entries from directory entries.
type EntryType string

const (
	// EntryFile is a regular file.
	EntryFile EntryType = "file"
	// EntryDir is a directory (kept so empty directories survive transfer).
	EntryDir EntryType = "dir"
)

// FileEntry is a single entry (file or directory) of a transfer manifest.
type FileEntry struct {
	// Index is the stable position of the entry inside the manifest.
	Index int `json:"index"`
	// Path is the transfer-relative, slash-separated path.
	Path string `json:"path"`
	// Type is EntryFile or EntryDir.
	Type EntryType `json:"type"`
	// Size is the size in bytes (0 for directories).
	Size int64 `json:"size"`
	// Mode holds the POSIX permission bits.
	Mode uint32 `json:"mode"`
	// ModTime is the modification time as Unix seconds.
	ModTime int64 `json:"modTime"`
	// Chunks is the number of chunks the file is split into.
	Chunks int64 `json:"chunks"`
	// SourcePath is the absolute path on the sending host; never serialized.
	SourcePath string `json:"-"`
}

// Manifest is the server-side, mutable representation of the transfer.
// Hash values are produced lazily by the Hasher and exposed through DTO.
type Manifest struct {
	// ChunkSize is the fixed chunk size in bytes.
	ChunkSize int64
	// Files is the ordered list of entries (files and directories).
	Files []*FileEntry
}

// FileEntryDTO is the serialized view of an entry; ChunkHashes is null until
// the server has finished hashing the entry.
type FileEntryDTO struct {
	Index       int       `json:"index"`
	Path        string    `json:"path"`
	Type        EntryType `json:"type"`
	Size        int64     `json:"size"`
	Mode        uint32    `json:"mode"`
	ModTime     int64     `json:"modTime"`
	Chunks      int64     `json:"chunks"`
	ChunkHashes []string  `json:"chunkHashes"`
	MerkleRoot  string    `json:"merkleRoot"`
}

// DTO is the serialized manifest served at /api/manifest.
type DTO struct {
	Protocol        string    `json:"protocol"`
	Token           string    `json:"token"`
	CreatedAt       time.Time `json:"createdAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
	ChunkSize       int64     `json:"chunkSize"`
	HashingDone     bool      `json:"hashingDone"`
	HashingProgress float64   `json:"hashingProgress"`
	MerkleRoot      string    `json:"merkleRoot"`
	// HashingError is set when the background hashing pass failed
	// permanently (e.g. a source file became unreadable); the global root
	// can never converge in that case and clients must stop waiting.
	HashingError string         `json:"hashingError,omitempty"`
	Files        []FileEntryDTO `json:"files"`
}

// Session holds the transfer token and its validity window.
type Session struct {
	Token     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NewSession creates a cryptographically random token valid for ttl.
func NewSession(ttl time.Duration) (*Session, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	now := time.Now()
	return &Session{
		Token:     hex.EncodeToString(raw),
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}, nil
}

// Expired reports whether the session is past its expiry at time now.
func (s *Session) Expired(now time.Time) bool {
	return now.After(s.ExpiresAt)
}

// ChunksForSize returns the number of chunks needed to hold size bytes.
func ChunksForSize(size, chunkSize int64) int64 {
	if size <= 0 {
		return 0
	}
	return (size + chunkSize - 1) / chunkSize
}

// Build constructs a Manifest by walking the given input paths.
//
// Path rules:
//   - a single file argument is exposed as its basename;
//   - a single directory argument is exposed as "<dirbasename>/...";
//   - multiple arguments are each exposed as their basename, directories are
//     walked recursively with the basename as prefix;
//   - top-level name collisions are de-duplicated as name(2), name(3)...;
//   - empty files are kept (0 chunks), empty directories are kept as entries,
//     symlinks and non-regular files are skipped.
func Build(paths []string, chunkSize int64) (*Manifest, error) {
	if chunkSize <= 0 {
		return nil, fmt.Errorf("chunk size must be positive, got %d", chunkSize)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("at least one path is required")
	}
	m := &Manifest{ChunkSize: chunkSize, Files: []*FileEntry{}}
	taken := map[string]bool{}
	for _, arg := range paths {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return nil, err
		}
		// Lstat (not Stat): a symlink passed explicitly as a top-level
		// argument must be skipped as well, never followed (see FR-1).
		info, err := os.Lstat(abs)
		if err != nil {
			return nil, err
		}
		name := uniqueName(filepath.Base(abs), taken)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			continue
		case info.IsDir():
			if err := m.addDir(name, abs, info, true); err != nil {
				return nil, err
			}
		case info.Mode().IsRegular():
			m.addFileEntry(name, abs, info)
		default:
			// Sockets/devices/etc. are intentionally skipped.
			continue
		}
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("no regular files or directories to send (symlinks are skipped)")
	}
	return m, nil
}

func (m *Manifest) addFileEntry(relPath, absPath string, info fs.FileInfo) {
	m.Files = append(m.Files, &FileEntry{
		Index:      len(m.Files),
		Path:       filepath.ToSlash(relPath),
		Type:       EntryFile,
		Size:       info.Size(),
		Mode:       uint32(info.Mode().Perm()),
		ModTime:    info.ModTime().Unix(),
		Chunks:     ChunksForSize(info.Size(), m.ChunkSize),
		SourcePath: absPath,
	})
}

func (m *Manifest) addDirEntry(relPath string, info fs.FileInfo) {
	m.Files = append(m.Files, &FileEntry{
		Index:   len(m.Files),
		Path:    filepath.ToSlash(relPath),
		Type:    EntryDir,
		Mode:    uint32(info.Mode().Perm()),
		ModTime: info.ModTime().Unix(),
	})
}

// addDir records the root directory (when includeRoot is true) and walks it
// recursively, mirroring every directory and regular file.
func (m *Manifest) addDir(rootName, absRoot string, rootInfo fs.FileInfo, includeRoot bool) error {
	if includeRoot {
		m.addDirEntry(rootName, rootInfo)
	}
	return filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == absRoot {
			return nil
		}
		rel, err := filepath.Rel(absRoot, p)
		if err != nil {
			return err
		}
		entryPath := rootName
		if rel != "." {
			entryPath = rootName + "/" + filepath.ToSlash(rel)
		}
		// WalkDir uses lstat: a symlink is never followed or descended.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			m.addDirEntry(entryPath, info)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		m.addFileEntry(entryPath, p, info)
		return nil
	})
}

// uniqueName returns name or, when name is already taken, name(2), name(3)…,
// inserting the counter before the file extension.
func uniqueName(name string, taken map[string]bool) string {
	if !taken[name] {
		taken[name] = true
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s(%d)%s", base, n, ext)
		if !taken[candidate] {
			taken[candidate] = true
			return candidate
		}
	}
}
