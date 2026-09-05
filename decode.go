package mosaic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// Decode reads the pattern directory produced by Encode and reconstructs
// the original file into store, returning the manifest and the
// reconstructed bytes once every chunk it references is available.
//
// The units in patternDir need not be complete and may arrive across many
// calls: any unit already in store is skipped without touching disk again,
// and any unit missing from patternDir is simply skipped this round. If
// chunks are still missing afterwards, Decode returns an error alongside
// the manifest so the caller can report progress and try again later —
// this is what makes it safe to feed it a partial scan.
func Decode(patternDir string, store *Store) (*Manifest, []byte, error) {
	manifestBytes, err := os.ReadFile(filepath.Join(patternDir, "manifest.json"))
	if err != nil {
		return nil, nil, err
	}
	var m Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, nil, err
	}

	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, nil, err
	}
	defer dec.Close()

	for _, h := range m.UniqueChunks() {
		if store.Has(h) {
			continue // already reconstructed in a previous session or file
		}
		unitPath := filepath.Join(patternDir, "chunks", h.String()+".unit")
		compressed, err := os.ReadFile(unitPath)
		if err != nil {
			continue // not captured yet — fine, try again on the next pass
		}
		raw, err := dec.DecodeAll(compressed, nil)
		if err != nil {
			return &m, nil, fmt.Errorf("mosaic: unit %s failed to decompress: %w", h, err)
		}
		if hashBytes(raw) != h {
			return &m, nil, fmt.Errorf("mosaic: unit %s failed its integrity check (corrupted or tampered)", h)
		}
		if err := store.Put(h, raw); err != nil {
			return &m, nil, err
		}
	}

	missing := store.Missing(m.ChunkHashes)
	if len(missing) > 0 {
		return &m, nil, fmt.Errorf("mosaic: %d/%d unique chunks still missing", len(missing), len(m.UniqueChunks()))
	}

	var buf bytes.Buffer
	for _, h := range m.ChunkHashes {
		data, err := store.Get(h)
		if err != nil {
			return &m, nil, err
		}
		buf.Write(data)
	}

	if hashBytes(buf.Bytes()) != m.FileHash {
		return &m, nil, fmt.Errorf("mosaic: reconstructed file does not match file_hash — corrupted or tampered")
	}

	return &m, buf.Bytes(), nil
}

// Progress reports how many of the manifest's unique chunks are already
// available in store, e.g. for showing "23/40 blocos" while scanning.
func Progress(m *Manifest, store *Store) (have, total int) {
	uniq := m.UniqueChunks()
	total = len(uniq)
	for _, h := range uniq {
		if store.Has(h) {
			have++
		}
	}
	return have, total
}
