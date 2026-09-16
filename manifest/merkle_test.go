package manifest

import (
	"encoding/hex"
	"testing"
)

// referenceMerkle is an intentionally independent, straightforward
// implementation used to cross-check the production MerkleRoot.
func referenceMerkle(leaves []string) string {
	if len(leaves) == 0 {
		return HashHex(nil)
	}
	level := append([]string(nil), leaves...)
	for len(level) > 1 {
		if len(level)%2 == 1 {
			level = append(level, level[len(level)-1])
		}
		next := make([]string, 0, len(level)/2)
		for i := 0; i < len(level); i += 2 {
			a, _ := hex.DecodeString(level[i])
			b, _ := hex.DecodeString(level[i+1])
			next = append(next, HashHex(append(a, b...)))
		}
		level = next
	}
	return level[0]
}

func TestMerkleRootVectors(t *testing.T) {
	h0 := HashHex(nil)
	if h0 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("sha256(empty) = %s", h0)
	}
	leaves := []string{HashHex([]byte("a")), HashHex([]byte("b")), HashHex([]byte("c")), HashHex([]byte("d"))}
	cases := [][]string{
		nil,
		leaves[:1],
		leaves[:2],
		leaves[:3], // odd: last node duplicated
		leaves[:4],
	}
	for _, in := range cases {
		got, err := MerkleRoot(in)
		if err != nil {
			t.Fatal(err)
		}
		want := referenceMerkle(in)
		if got != want {
			t.Errorf("MerkleRoot(%d leaves) = %s, want %s", len(in), got, want)
		}
	}
	// Single leaf must be returned unchanged.
	if got, _ := MerkleRoot([]string{leaves[0]}); got != leaves[0] {
		t.Errorf("single leaf root = %s, want %s", got, leaves[0])
	}
	// Bad hex must surface as an error.
	if _, err := MerkleRoot([]string{"not-hex"}); err == nil {
		t.Fatal("expected error for non-hex leaf")
	}
}

func TestFileBindingHashStable(t *testing.T) {
	h := FileBindingHash("root/a.txt", 5, 0o644, 1700000000, HashHex([]byte("a")))
	if len(h) != 64 {
		t.Fatalf("binding hash length = %d, want 64", len(h))
	}
	// Any change in path/size/metadata/root changes the binding.
	others := []string{
		FileBindingHash("root/b.txt", 5, 0o644, 1700000000, HashHex([]byte("a"))),
		FileBindingHash("root/a.txt", 6, 0o644, 1700000000, HashHex([]byte("a"))),
		FileBindingHash("root/a.txt", 5, 0o600, 1700000000, HashHex([]byte("a"))),
		FileBindingHash("root/a.txt", 5, 0o644, 1700000001, HashHex([]byte("a"))),
		FileBindingHash("root/a.txt", 5, 0o644, 1700000000, HashHex([]byte("b"))),
	}
	for _, o := range others {
		if o == h {
			t.Fatal("binding hash did not change when an input changed")
		}
	}
}
