package mosaic

import (
	"crypto/sha256"
	"encoding/binary"
	"io"
	"math/rand"

	"github.com/restic/chunker"
)

// chunkPolynomial is derived once from a fixed seed instead of randomly at
// runtime: identical content must always split into identical chunk
// boundaries across separate encode runs (today, next week, on a
// different machine), otherwise two copies of the same bytes would hash
// to different chunks and deduplication would never kick in.
var chunkPolynomial = mustPolynomial()

func mustPolynomial() chunker.Pol {
	seed := sha256.Sum256([]byte("mosaic-chunk-polynomial-v1"))
	// A seeded PRNG gives DerivePolynomial an effectively endless, varied
	// (but fully reproducible) stream of candidate bytes to search through.
	source := rand.New(rand.NewSource(int64(binary.BigEndian.Uint64(seed[:8]))))
	pol, err := chunker.DerivePolynomial(source)
	if err != nil {
		panic("mosaic: could not derive chunk polynomial: " + err.Error())
	}
	return pol
}

// Chunk boundaries are tuned small on purpose: this project targets
// transport media with tight per-unit budgets (a QR code, a printed
// bitmap tile), not the multi-megabyte chunks chunker defaults to for
// backup tools.
const (
	minChunkSize = 512  // bytes
	maxChunkSize = 4096 // bytes
	avgBits      = 11   // ~2KiB average chunk size
)

// splitChunks reads r and calls fn once per content-defined chunk, in
// order. Boundaries are determined by the content itself (a rolling
// checksum), not by byte offset — so inserting or removing bytes near the
// start of a file only reshuffles the chunks around that edit, leaving
// everything else, and its hash, unchanged.
func splitChunks(r io.Reader, fn func(data []byte) error) error {
	c := chunker.New(r, chunkPolynomial,
		chunker.WithBoundaries(minChunkSize, maxChunkSize),
		chunker.WithAverageBits(avgBits),
	)
	buf := make([]byte, maxChunkSize)
	for {
		chunk, err := c.Next(buf)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Next reuses buf on each call, so copy out before it's overwritten.
		data := make([]byte, len(chunk.Data))
		copy(data, chunk.Data)
		if err := fn(data); err != nil {
			return err
		}
	}
}
