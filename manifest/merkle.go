package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// HashHex returns the lowercase hex SHA-256 digest of data.
func HashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// pairHash returns SHA-256(decodeHex(a) || decodeHex(b)).
func pairHash(a, b string) (string, error) {
	ab, err := hex.DecodeString(a)
	if err != nil {
		return "", fmt.Errorf("invalid hex digest: %w", err)
	}
	bb, err := hex.DecodeString(b)
	if err != nil {
		return "", fmt.Errorf("invalid hex digest: %w", err)
	}
	h := sha256.New()
	h.Write(ab)
	h.Write(bb)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// MerkleRoot builds a Merkle tree over the ordered hex leaf digests and
// returns the hex root. When a level has an odd number of nodes the last
// node is duplicated. With no leaves the root is SHA-256("").
func MerkleRoot(leaves []string) (string, error) {
	if len(leaves) == 0 {
		return HashHex(nil), nil
	}
	for _, leaf := range leaves {
		if _, err := hex.DecodeString(leaf); err != nil {
			return "", fmt.Errorf("invalid hex digest: %w", err)
		}
	}
	level := make([]string, len(leaves))
	copy(level, leaves)
	for len(level) > 1 {
		if len(level)%2 == 1 {
			level = append(level, level[len(level)-1])
		}
		next := make([]string, 0, len(level)/2)
		for i := 0; i < len(level); i += 2 {
			p, err := pairHash(level[i], level[i+1])
			if err != nil {
				return "", err
			}
			next = append(next, p)
		}
		level = next
	}
	return level[0], nil
}

// FileBindingHash binds an entry's path and metadata to its content root so
// that renaming or reordering entries changes the global Merkle root.
// Directories pass an empty fileRoot.
func FileBindingHash(path string, size int64, mode uint32, modTime int64, fileRoot string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "qctp-v1\n%s\n%d\n%d\n%d\n%s", path, size, mode, modTime, fileRoot)
	return HashHex([]byte(b.String()))
}
