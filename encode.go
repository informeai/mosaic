package mosaic

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// ChunkAndCompressReader is the streaming counterpart to ChunkAndCompress:
// it reads r exactly once, chunk by chunk, hashing and zstd-compressing as
// it goes and handing each newly-seen unique chunk to onChunk immediately.
// Memory use stays bounded by chunk size and the set of chunk hashes seen
// so far (32 bytes each) — never by the size of r — which is what lets a
// caller (a SQLite store writing rows as chunks arrive, say) encode a file
// far larger than available RAM.
//
// chunkHashes still records every occurrence in order, duplicates
// included — reconstruction needs that full sequence — but a duplicate is
// not recompressed or handed to onChunk again. size is the total bytes
// read from r (i.e. the original file size).
func ChunkAndCompressReader(r io.Reader, size ChunkSize, onChunk func(h Hash, compressed []byte) error) (fileHash Hash, chunkHashes []Hash, totalSize int64, err error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return Hash{}, nil, 0, err
	}
	defer enc.Close()

	hasher := sha256.New()
	seen := map[Hash]bool{}

	err = splitChunks(io.TeeReader(r, hasher), size, func(data []byte) error {
		totalSize += int64(len(data))
		h := HashBytes(data)
		chunkHashes = append(chunkHashes, h)
		if seen[h] {
			return nil // repeated inside this file — already queued
		}
		seen[h] = true
		return onChunk(h, enc.EncodeAll(data, nil))
	})
	if err != nil {
		return Hash{}, nil, 0, fmt.Errorf("mosaic: chunking failed: %w", err)
	}

	copy(fileHash[:], hasher.Sum(nil))
	return fileHash, chunkHashes, totalSize, nil
}

// ChunkAndCompress splits raw into content-defined chunks and zstd-
// compresses each unique one. It returns the full ordered list of chunk
// hashes (as they occur in the file, duplicates included — this is the
// reconstruction order) plus the compressed bytes for each distinct hash.
//
// Exported so alternate stores (e.g. a SQLite-backed one) outside this
// package can produce chunks the same way Encode/EncodeBundle do, without
// duplicating the chunking/compression logic. It holds every unique
// chunk's compressed bytes in units at once, which is fine for the
// pattern-directory and Bundle use cases (both already need the whole
// file in memory anyway) — a caller that can't afford that should use
// ChunkAndCompressReader instead.
func ChunkAndCompress(raw []byte) (chunkHashes []Hash, units map[Hash][]byte, err error) {
	units = map[Hash][]byte{}
	_, chunkHashes, _, err = ChunkAndCompressReader(bytes.NewReader(raw), DefaultChunkSize, func(h Hash, compressed []byte) error {
		units[h] = compressed
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return chunkHashes, units, nil
}

// Encode splits the file at inputPath into content-defined chunks and
// writes the reconstruction pattern into patternDir:
//
//	patternDir/chunks/<hash>.unit   one compressed file per unique chunk
//	patternDir/manifest.json        the ordered recipe to reassemble them
//
// patternDir doubles as a shared chunk pool: encoding a second file into
// the same directory only adds the units that first file didn't already
// contribute — content the two files have in common is written once.
func Encode(inputPath, patternDir string) (*Manifest, error) {
	raw, err := os.ReadFile(inputPath)
	if err != nil {
		return nil, err
	}

	chunkHashes, units, err := ChunkAndCompress(raw)
	if err != nil {
		return nil, err
	}

	chunksDir := filepath.Join(patternDir, "chunks")
	if err := os.MkdirAll(chunksDir, 0o755); err != nil {
		return nil, err
	}
	for h, compressed := range units {
		unitPath := filepath.Join(chunksDir, h.String()+".unit")
		if _, err := os.Stat(unitPath); err == nil {
			continue // another (or a previous) encode already produced this chunk
		}
		if err := os.WriteFile(unitPath, compressed, 0o644); err != nil {
			return nil, err
		}
	}

	m := &Manifest{
		Name:        filepath.Base(inputPath),
		Size:        int64(len(raw)),
		FileHash:    HashBytes(raw),
		ChunkHashes: chunkHashes,
	}

	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(patternDir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return nil, err
	}

	return m, nil
}
