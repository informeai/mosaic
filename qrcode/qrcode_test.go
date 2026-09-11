package qrcode

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"mosaic"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.bin")
	// Big enough to need several QR frames (DefaultFrameSize is 700).
	data := randomBytes(t, 5_000)
	if err := os.WriteFile(input, data, 0o644); err != nil {
		t.Fatal(err)
	}

	qrDir := filepath.Join(dir, "qr")
	b, total, err := EncodeToQR(input, qrDir)
	if err != nil {
		t.Fatal(err)
	}
	if total < 2 {
		t.Fatalf("expected the 5000-byte bundle to need several frames, got %d", total)
	}
	if b.FileHash != mosaic.HashBytes(data) {
		t.Fatal("bundle file hash does not match the input")
	}

	entries, err := os.ReadDir(qrDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != total {
		t.Fatalf("expected %d PNG frames, found %d", total, len(entries))
	}

	store, err := mosaic.OpenStore(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}

	have, gotTotal, got, err := DecodeFromQR(qrDir, store)
	if err != nil {
		t.Fatal(err)
	}
	if have != total || gotTotal != total {
		t.Fatalf("expected %d/%d frames, got %d/%d", total, total, have, gotTotal)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reconstructed bytes do not match the original file")
	}
}

// TestPartialScan proves the point of framing this way: decoding from a
// subset of the frames reports progress instead of failing outright, and
// feeding it the rest later (in any order) completes the reconstruction —
// simulating an animated QR loop scanned across more than one pass.
func TestPartialScan(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.bin")
	data := randomBytes(t, 5_000)
	if err := os.WriteFile(input, data, 0o644); err != nil {
		t.Fatal(err)
	}

	fullDir := filepath.Join(dir, "qr-full")
	_, total, err := EncodeToQR(input, fullDir)
	if err != nil {
		t.Fatal(err)
	}
	if total < 3 {
		t.Skip("not enough frames to exercise a partial scan")
	}

	entries, err := os.ReadDir(fullDir)
	if err != nil {
		t.Fatal(err)
	}

	partialDir := filepath.Join(dir, "qr-partial")
	if err := os.MkdirAll(partialDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Copy only every other frame across.
	for i, e := range entries {
		if i%2 != 0 {
			continue
		}
		src, err := os.ReadFile(filepath.Join(fullDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(partialDir, e.Name()), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store, err := mosaic.OpenStore(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}

	have, gotTotal, data2, err := DecodeFromQR(partialDir, store)
	if err == nil {
		t.Fatal("expected an error reporting incomplete frames")
	}
	if data2 != nil {
		t.Fatal("expected no reconstructed data from a partial scan")
	}
	if have == 0 || have >= gotTotal {
		t.Fatalf("expected partial progress, got %d/%d", have, gotTotal)
	}

	// The missing frames show up (a second pass over the loop).
	for i, e := range entries {
		if i%2 == 0 {
			continue
		}
		src, err := os.ReadFile(filepath.Join(fullDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(partialDir, e.Name()), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	have, gotTotal, data2, err = DecodeFromQR(partialDir, store)
	if err != nil {
		t.Fatal(err)
	}
	if have != gotTotal {
		t.Fatalf("expected all frames captured, got %d/%d", have, gotTotal)
	}
	if !bytes.Equal(data2, data) {
		t.Fatal("reconstructed bytes do not match the original file after completing the scan")
	}
}
