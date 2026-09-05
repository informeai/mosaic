package mosaic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// chunkAndCompress splits raw into content-defined chunks and zstd-
// compresses each unique one. It returns the full ordered list of chunk
// hashes (as they occur in the file, duplicates included — this is the
// reconstruction order) plus the compressed bytes for each distinct hash.
func chunkAndCompress(raw []byte) (chunkHashes []Hash, units map[Hash][]byte, err error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, nil, err
	}
	defer enc.Close()

	units = map[Hash][]byte{}
	err = splitChunks(bytes.NewReader(raw), func(data []byte) error {
		h := hashBytes(data)
		chunkHashes = append(chunkHashes, h)
		if _, ok := units[h]; ok {
			return nil // repeated inside this file — already queued
		}
		units[h] = enc.EncodeAll(data, nil)
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("mosaic: chunking failed: %w", err)
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

	chunkHashes, units, err := chunkAndCompress(raw)
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
		FileHash:    hashBytes(raw),
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
