// Package qrcode renders a Bundle as a sequence of QR-code images and reads
// it back — another transport for the same reconstruction logic, on par
// with the pattern directory or the plain Bundle JSON file, for when
// nothing exists between two devices but a camera pointed at a screen (an
// air-gapped transfer, a printed sheet).
//
// Kept out of the root mosaic package for the same reason sqlitestore is:
// only code that actually renders/reads QR codes should pay for that
// dependency.
//
// A single QR code tops out around ~2KB of 8-bit data even at a
// forgiving error-correction level, well under mosaic's own max chunk
// size (4096 bytes) — so framing here is deliberately independent of the
// file's content-defined chunks. A Bundle's JSON (manifest + every chunk,
// already produced by mosaic.EncodeBundle) is instead re-split into small,
// fixed-size frames sized for reliable scanning, each carrying enough of
// its own header to be reassembled in any order, out of any subset that
// later completes — the same resumable, order-independent shape as the
// rest of mosaic, just at the framing layer instead of the chunk layer.
package qrcode

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"path/filepath"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
	"github.com/makiuchi-d/gozxing/qrcode/decoder"

	"mosaic"
)

// DefaultFrameSize is how many bytes of the Bundle's JSON go into each QR
// frame's payload. Kept well under a single QR code's real binary
// capacity so a frame stays reliably scannable by a phone camera,
// regardless of how large the file's own chunks are.
const DefaultFrameSize = 700

// imageSize is the rendered PNG's side length in pixels; the QR writer
// scales its native module grid up to fit it.
const imageSize = 512

const magic = "MQR1"

// header layout: magic(4) + setID(8) + total(2) + index(2) + payloadLen(2)
const headerSize = 4 + 8 + 2 + 2 + 2

var encodeHints = map[gozxing.EncodeHintType]interface{}{
	gozxing.EncodeHintType_CHARACTER_SET:    "ISO-8859-1",
	gozxing.EncodeHintType_ERROR_CORRECTION: decoder.ErrorCorrectionLevel_M,
	gozxing.EncodeHintType_MARGIN:           1,
}

var decodeHints = map[gozxing.DecodeHintType]interface{}{
	gozxing.DecodeHintType_PURE_BARCODE: true,
}

// EncodeToQR bundles inputPath exactly like mosaic.EncodeBundle, then
// splits that self-contained Bundle's JSON into DefaultFrameSize-byte
// frames and renders each as its own QR-code PNG in outDir (0000.png,
// 0001.png, ...) — meant to be shown one after another (or all printed on
// a sheet), not carried as files.
func EncodeToQR(inputPath, outDir string) (*mosaic.Bundle, int, error) {
	b, err := mosaic.EncodeBundle(inputPath)
	if err != nil {
		return nil, 0, err
	}

	blob, err := json.Marshal(b)
	if err != nil {
		return nil, 0, err
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, 0, err
	}

	setID := sha256.Sum256(blob)
	total := (len(blob) + DefaultFrameSize - 1) / DefaultFrameSize
	if total == 0 {
		total = 1
	}
	if total > 1<<16-1 {
		return nil, 0, fmt.Errorf("mosaic/qrcode: bundle needs %d frames, more than fit in a uint16 count", total)
	}
	digits := len(fmt.Sprintf("%d", total-1))

	writer := qrcode.NewQRCodeWriter()
	for i := 0; i < total; i++ {
		start := i * DefaultFrameSize
		end := min(start+DefaultFrameSize, len(blob))
		payload := blob[start:end]

		frame := make([]byte, headerSize+len(payload))
		copy(frame[0:4], magic)
		copy(frame[4:12], setID[:8])
		binary.BigEndian.PutUint16(frame[12:14], uint16(total))
		binary.BigEndian.PutUint16(frame[14:16], uint16(i))
		binary.BigEndian.PutUint16(frame[16:18], uint16(len(payload)))
		copy(frame[headerSize:], payload)

		matrix, err := writer.Encode(bytesToLatin1(frame), gozxing.BarcodeFormat_QR_CODE, imageSize, imageSize, encodeHints)
		if err != nil {
			return nil, 0, fmt.Errorf("mosaic/qrcode: encoding frame %d/%d: %w", i+1, total, err)
		}

		path := filepath.Join(outDir, fmt.Sprintf("%0*d.png", digits, i))
		if err := writePNG(path, matrix); err != nil {
			return nil, 0, err
		}
	}

	return b, total, nil
}

// DecodeFromQR scans every *.png in qrDir, decodes each as a QR code, and
// groups the resulting frames by the transfer they belong to (a setID
// derived from the sender's full payload) — exactly the scenario of
// scanning an animated QR loop out of order, across many passes, with
// some frames missed. Frames that aren't readable, aren't one of ours, or
// belong to a different transfer than the most complete one found are
// simply ignored, not treated as fatal.
//
// have/total describe the most complete transfer present; data is nil
// (with a descriptive error) until every one of its frames has been
// captured, at which point the reassembled Bundle is reconstructed via
// mosaic.DecodeBundle — verifying every chunk and the whole file against
// their hashes exactly as any other Bundle would.
func DecodeFromQR(qrDir string, store *mosaic.Store) (have, total int, data []byte, err error) {
	entries, err := os.ReadDir(qrDir)
	if err != nil {
		return 0, 0, nil, err
	}

	type frameSet struct {
		total  uint16
		frames map[uint16][]byte
	}
	sets := map[[8]byte]*frameSet{}

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".png" {
			continue
		}
		frame, ok := readFrame(filepath.Join(qrDir, e.Name()))
		if !ok {
			continue // unreadable, blurry, or not one of ours — try again next pass
		}

		var setID [8]byte
		copy(setID[:], frame[4:12])
		frameTotal := binary.BigEndian.Uint16(frame[12:14])
		idx := binary.BigEndian.Uint16(frame[14:16])
		payloadLen := binary.BigEndian.Uint16(frame[16:18])
		if headerSize+int(payloadLen) > len(frame) {
			continue // truncated/corrupted header — ignore
		}
		payload := frame[headerSize : headerSize+int(payloadLen)]

		s, ok := sets[setID]
		if !ok {
			s = &frameSet{total: frameTotal, frames: map[uint16][]byte{}}
			sets[setID] = s
		}
		s.frames[idx] = payload
	}

	// If qrDir happens to hold frames from more than one scan session, the
	// most complete transfer is the one a receiver actually cares about.
	var best *frameSet
	for _, s := range sets {
		if best == nil || len(s.frames) > len(best.frames) {
			best = s
		}
	}
	if best == nil {
		return 0, 0, nil, fmt.Errorf("mosaic/qrcode: no frames captured yet in %s", qrDir)
	}
	have, total = len(best.frames), int(best.total)
	if have < total {
		return have, total, nil, fmt.Errorf("mosaic/qrcode: %d/%d frames captured", have, total)
	}

	blob := make([]byte, 0, total*DefaultFrameSize)
	for i := 0; i < total; i++ {
		p, ok := best.frames[uint16(i)]
		if !ok {
			return have, total, nil, fmt.Errorf("mosaic/qrcode: frame %d missing despite a complete count", i)
		}
		blob = append(blob, p...)
	}

	var b mosaic.Bundle
	if err := json.Unmarshal(blob, &b); err != nil {
		return have, total, nil, fmt.Errorf("mosaic/qrcode: reassembled frames are not a valid bundle: %w", err)
	}

	data, err = mosaic.DecodeBundle(&b, store)
	if err != nil {
		return have, total, nil, err
	}
	return have, total, data, nil
}

// readFrame decodes path as a QR code and returns its raw frame bytes.
// ok is false for anything that isn't a readable QR code carrying one of
// our frames (wrong image, blurry scan, unrelated QR content).
func readFrame(path string) (frame []byte, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		return nil, false
	}

	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return nil, false
	}
	result, err := qrcode.NewQRCodeReader().Decode(bmp, decodeHints)
	if err != nil {
		return nil, false
	}

	frame = latin1ToBytes(result.GetText())
	if len(frame) < headerSize || string(frame[0:4]) != magic {
		return nil, false
	}
	return frame, true
}

func writePNG(path string, img *gozxing.BitMatrix) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	err = png.Encode(f, img)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// bytesToLatin1/latin1ToBytes round-trip arbitrary bytes through a Go
// string by mapping each byte to the Unicode code point of the same
// value (which is exactly what ISO-8859-1 is) — casting a []byte straight
// to a string instead would have the writer misinterpret it as UTF-8 and
// choke on the invalid sequences that turn up in compressed/binary data.
func bytesToLatin1(b []byte) string {
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}

func latin1ToBytes(s string) []byte {
	runes := []rune(s)
	b := make([]byte, len(runes))
	for i, r := range runes {
		b[i] = byte(r)
	}
	return b
}
