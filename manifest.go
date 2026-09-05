package mosaic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Hash is a content hash (SHA-256) identifying either a chunk or a whole
// reconstructed file. Two chunks with identical bytes always produce the
// same Hash — that equality is what deduplication is built on.
type Hash [32]byte

func hashBytes(b []byte) Hash {
	return Hash(sha256.Sum256(b))
}

func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

func (h Hash) MarshalJSON() ([]byte, error) {
	return json.Marshal(h.String())
}

func (h *Hash) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := ParseHash(s)
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}

// ParseHash parses the hex representation produced by Hash.String.
func ParseHash(s string) (Hash, error) {
	var h Hash
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, err
	}
	if len(b) != len(h) {
		return h, fmt.Errorf("mosaic: invalid hash length %d", len(b))
	}
	copy(h[:], b)
	return h, nil
}

// Manifest is the reconstruction recipe for one file: the ordered list of
// chunk hashes that, concatenated in order, reproduce the original bytes
// exactly. It carries no chunk data itself — only references — so it stays
// small even for a large file.
type Manifest struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	FileHash    Hash   `json:"file_hash"`
	ChunkHashes []Hash `json:"chunk_hashes"`
}

// UniqueChunks returns the distinct chunk hashes referenced by the
// manifest, collapsing any repeats (a chunk that appears N times in the
// file only needs to be transported once).
func (m *Manifest) UniqueChunks() []Hash {
	seen := make(map[Hash]bool, len(m.ChunkHashes))
	var uniq []Hash
	for _, h := range m.ChunkHashes {
		if !seen[h] {
			seen[h] = true
			uniq = append(uniq, h)
		}
	}
	return uniq
}
