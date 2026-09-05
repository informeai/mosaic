package mosaic

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDBRoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "mosaic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	data := randomBytes(t, 50_000)

	summary, err := EncodeToDB(db, "in.bin", data)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ID == "" {
		t.Fatal("expected a non-empty manifest id")
	}

	// Encoding identical content again must return the same id without
	// erroring or duplicating anything.
	again, err := EncodeToDB(db, "in.bin", data)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != summary.ID {
		t.Fatalf("re-encoding identical content changed the id: %s -> %s", summary.ID, again.ID)
	}

	list, err := ListManifests(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 manifest after encoding the same content twice, got %d", len(list))
	}

	got, gotBytes, err := DecodeFromDB(db, summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "in.bin" || got.Size != int64(len(data)) {
		t.Fatalf("unexpected manifest summary: %+v", got)
	}
	if !bytes.Equal(gotBytes, data) {
		t.Fatal("reconstructed bytes do not match the original file")
	}

	if _, _, err := DecodeFromDB(db, "0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Fatal("expected an error for an unknown manifest id")
	}
}

func TestBundleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.bin")
	data := randomBytes(t, 50_000)
	if err := os.WriteFile(input, data, 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := EncodeBundle(input)
	if err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := DecodeBundle(b, store)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reconstructed bytes do not match the original file")
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.bin")
	data := randomBytes(t, 50_000)
	if err := os.WriteFile(input, data, 0o644); err != nil {
		t.Fatal(err)
	}

	patternDir := filepath.Join(dir, "pattern")
	if _, err := Encode(input, patternDir); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}

	_, got, err := Decode(patternDir, store)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reconstructed bytes do not match the original file")
	}
}

// TestResume proves the core promise: a decode fed only part of the
// pattern reports partial progress instead of failing outright, and
// picks up exactly where it left off once the rest becomes available —
// no need to re-transfer chunks it already has.
func TestResume(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.bin")
	data := randomBytes(t, 50_000)
	if err := os.WriteFile(input, data, 0o644); err != nil {
		t.Fatal(err)
	}

	patternDir := filepath.Join(dir, "pattern")
	m, err := Encode(input, patternDir)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an interrupted scan: only half the unit files made it.
	chunksDir := filepath.Join(patternDir, "chunks")
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	for i, e := range entries {
		if i%2 == 0 {
			if err := os.Remove(filepath.Join(chunksDir, e.Name())); err != nil {
				t.Fatal(err)
			}
			removed++
		}
	}
	if removed == 0 || removed == len(entries) {
		t.Skip("not enough unique chunks to exercise a partial capture")
	}

	store, err := OpenStore(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := Decode(patternDir, store); err == nil {
		t.Fatal("expected decode to report missing chunks, got nil error")
	}
	have, total := Progress(m, store)
	if have == 0 || have >= total {
		t.Fatalf("expected partial progress, got %d/%d", have, total)
	}

	// The missing units reappear (a rescan, a second photo of the sheet).
	if _, err := Encode(input, patternDir); err != nil {
		t.Fatal(err)
	}
	_, got, err := Decode(patternDir, store)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reconstructed bytes do not match the original file after resuming")
	}
}

// TestDedupAcrossFiles proves the other core promise: two files that
// share content only pay the transport cost for what differs between
// them, because they're encoded into the same chunk pool.
func TestDedupAcrossFiles(t *testing.T) {
	dir := t.TempDir()

	shared := randomBytes(t, 40_000)
	fileA := append(append([]byte{}, shared...), randomBytes(t, 2_000)...)
	fileB := append(append([]byte{}, shared...), randomBytes(t, 2_000)...)

	pathA := filepath.Join(dir, "a.bin")
	pathB := filepath.Join(dir, "b.bin")
	if err := os.WriteFile(pathA, fileA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, fileB, 0o644); err != nil {
		t.Fatal(err)
	}

	patternDir := filepath.Join(dir, "pattern")
	if _, err := Encode(pathA, patternDir); err != nil {
		t.Fatal(err)
	}
	afterA, err := os.ReadDir(filepath.Join(patternDir, "chunks"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Encode(pathB, patternDir); err != nil {
		t.Fatal(err)
	}
	afterB, err := os.ReadDir(filepath.Join(patternDir, "chunks"))
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("units after A: %d, units after B: %d (B adds only what A didn't already have)",
		len(afterA), len(afterB))
	if len(afterB) >= len(afterA)*2 {
		t.Fatalf("expected shared content to be deduplicated, but B roughly doubled the unit count (%d -> %d)",
			len(afterA), len(afterB))
	}
}
