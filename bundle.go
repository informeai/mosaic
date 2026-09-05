package mosaic

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// Bundle is a self-contained reconstruction unit: the manifest plus every
// chunk it references, embedded directly, instead of split across
// manifest.json + chunks/*.unit on disk. Reach for it when a directory or
// a zip isn't practical — an API response/request body, for instance —
// since receiving a Bundle alone is enough to reconstruct, nothing else to
// fetch.
type Bundle struct {
	Manifest
	Chunks map[string][]byte `json:"chunks"` // hex hash -> zstd-compressed chunk bytes
}

// EncodeBundle behaves like Encode but keeps everything in memory and
// returns a single self-contained Bundle instead of writing pattern files.
func EncodeBundle(inputPath string) (*Bundle, error) {
	raw, err := os.ReadFile(inputPath)
	if err != nil {
		return nil, err
	}

	chunkHashes, units, err := chunkAndCompress(raw)
	if err != nil {
		return nil, err
	}

	chunks := make(map[string][]byte, len(units))
	for h, compressed := range units {
		chunks[h.String()] = compressed
	}

	return &Bundle{
		Manifest: Manifest{
			Name:        filepath.Base(inputPath),
			Size:        int64(len(raw)),
			FileHash:    hashBytes(raw),
			ChunkHashes: chunkHashes,
		},
		Chunks: chunks,
	}, nil
}

// DecodeBundle reconstructs the file b describes, using store as the chunk
// cache exactly like Decode does — a chunk store already has is reused
// as-is, and every chunk b carries gets verified and added to store before
// assembly.
//
// Unlike Decode's partial-pattern case, a Bundle is expected to be
// complete on its own: if a chunk its manifest references isn't in
// b.Chunks or in store, that's an error, not "scan more and try again".
func DecodeBundle(b *Bundle, store *Store) ([]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	for hashHex, compressed := range b.Chunks {
		h, err := ParseHash(hashHex)
		if err != nil {
			return nil, fmt.Errorf("mosaic: invalid chunk hash %q: %w", hashHex, err)
		}
		if store.Has(h) {
			continue
		}
		raw, err := dec.DecodeAll(compressed, nil)
		if err != nil {
			return nil, fmt.Errorf("mosaic: chunk %s failed to decompress: %w", h, err)
		}
		if hashBytes(raw) != h {
			return nil, fmt.Errorf("mosaic: chunk %s failed its integrity check (corrupted or tampered)", h)
		}
		if err := store.Put(h, raw); err != nil {
			return nil, err
		}
	}

	missing := store.Missing(b.ChunkHashes)
	if len(missing) > 0 {
		return nil, fmt.Errorf("mosaic: bundle is incomplete — %d/%d unique chunks missing", len(missing), len(b.UniqueChunks()))
	}

	var buf bytes.Buffer
	for _, h := range b.ChunkHashes {
		data, err := store.Get(h)
		if err != nil {
			return nil, err
		}
		buf.Write(data)
	}
	if hashBytes(buf.Bytes()) != b.FileHash {
		return nil, fmt.Errorf("mosaic: reconstructed file does not match file_hash — corrupted or tampered")
	}
	return buf.Bytes(), nil
}
