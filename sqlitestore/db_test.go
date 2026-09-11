package sqlitestore

import (
	"bytes"
	"crypto/rand"
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
