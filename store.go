package mosaic

import (
	"fmt"
	"os"
	"path/filepath"
)

// Store is a local, content-addressed cache of chunk bytes keyed by hash.
// It is the "saved somewhere" in the reconstruction: whatever Decode
// assembles is built by pulling chunks out of it, and whatever it already
// holds never needs to be captured again — from this file, a previous
// version of it, or an unrelated file that happens to share content.
type Store struct {
	dir string
}

// OpenStore opens (creating if needed) a directory-backed Store.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(h Hash) string {
	return filepath.Join(s.dir, h.String()+".chunk")
}

// Has reports whether the store already holds this chunk.
func (s *Store) Has(h Hash) bool {
	_, err := os.Stat(s.path(h))
	return err == nil
}

// Put saves data under h. Callers are expected to have already verified
// hashBytes(data) == h; Put itself does not re-check, so it stays cheap to
// call from a hot loop. Writing is a no-op if the chunk is already there.
func (s *Store) Put(h Hash, data []byte) error {
	if s.Has(h) {
		return nil
	}
	tmp := s.path(h) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(h))
}

// Get reads back a previously stored chunk.
func (s *Store) Get(h Hash) ([]byte, error) {
	data, err := os.ReadFile(s.path(h))
	if err != nil {
		return nil, fmt.Errorf("mosaic: chunk %s not in store: %w", h, err)
	}
	return data, nil
}

// Missing filters hashes down to the ones not yet present in the store —
// this is the "what am I still waiting for" list a decode session polls.
func (s *Store) Missing(hashes []Hash) []Hash {
	seen := make(map[Hash]bool, len(hashes))
	var missing []Hash
	for _, h := range hashes {
		if seen[h] {
			continue
		}
		seen[h] = true
		if !s.Has(h) {
			missing = append(missing, h)
		}
	}
	return missing
}
