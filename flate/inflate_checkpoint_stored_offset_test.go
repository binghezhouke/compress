package flate_test

import (
	"bufio"
	"bytes"
	stdflate "compress/flate"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/flate"
)

// Checkpoint offsets must count a byte only once even when Huffman lookahead
// left complete bytes in the bit buffer before a stored sync-flush block.
func TestInflateCheckpointAfterStoredFlush(t *testing.T) {
	payload := make([]byte, 1<<20)
	state := uint64(1234567)
	for i := range payload {
		if i%8192 >= 4096 {
			payload[i] = payload[i-4096]
		} else {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			payload[i] = byte(state)
		}
	}
	var encoded bytes.Buffer
	writer, err := stdflate.NewWriter(&encoded, stdflate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write(payload[:len(payload)/2]); err != nil {
		t.Fatal(err)
	}
	if err = writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write(payload[len(payload)/2:]); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	checkpointReplay(t, encoded.Bytes(), payload, false)
}

func TestInflateCheckpointNonAlignedHistory(t *testing.T) {
	// Two fixed-Huffman blocks: the first emits A; the second copies three
	// bytes at distance one. The second block begins at bit two of byte two.
	bits := new(checkpointBitWriter)
	bits.field(0, 1)
	bits.field(1, 2)
	bits.code(0x71, 8)
	bits.code(0, 7)
	bits.field(1, 1)
	bits.field(1, 2)
	bits.code(1, 7)
	bits.code(0, 5)
	bits.code(0, 7)
	checkpointReplay(t, bits.data, []byte("AAAA"), true)
}

func TestInflateCheckpointEmptyBlocksAndFullWindow(t *testing.T) {
	payload := bytes.Repeat([]byte("w"), 32768)
	var encoded bytes.Buffer
	writer, err := stdflate.NewWriter(&encoded, stdflate.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err = writer.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err = writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	checkpointReplay(t, encoded.Bytes(), append(payload, []byte("tail")...), false)
}

func checkpointReplay(t *testing.T, encoded, payload []byte, wantUnaligned bool) {
	t.Helper()
	native := stdflate.NewReader(bytes.NewReader(encoded))
	oracle, err := io.ReadAll(native)
	native.Close()
	if err != nil || !bytes.Equal(oracle, payload) {
		t.Fatalf("invalid independent fixture: %v", err)
	}
	inputs := map[string]func([]byte) io.Reader{
		"bytes":   func(p []byte) io.Reader { return bytes.NewReader(p) },
		"buffer":  func(p []byte) io.Reader { return bytes.NewBuffer(p) },
		"generic": func(p []byte) io.Reader { return struct{ io.Reader }{bytes.NewReader(p)} },
		"strings": func(p []byte) io.Reader { return strings.NewReader(string(p)) },
		"bufio":   func(p []byte) io.Reader { return bufio.NewReader(bytes.NewReader(p)) },
	}
	for kind, input := range inputs {
		t.Run(kind, func(t *testing.T) {
			var points []flate.InflateCheckpoint
			reader := flate.NewReaderOpts(input(encoded), flate.WithPartialBlock(), flate.WithEobCallback(func(cp flate.InflateCheckpoint) {
				if cp.Final || cp.UncompressedOffset >= int64(len(payload)) {
					return
				}
				cp.Window = append([]byte(nil), cp.Window...)
				points = append(points, cp)
			}))
			got, err := io.ReadAll(reader)
			reader.Close()
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("initial decode: %v", err)
			}
			if len(points) == 0 {
				t.Fatal("no nonfinal checkpoint")
			}
			unaligned := false
			for _, cp := range points {
				unaligned = unaligned || cp.BitOffset != 0
				reader := flate.NewReaderOpts(input(encoded[cp.CompressedOffset:]), flate.WithResumeFrom(cp))
				got, err := io.ReadAll(reader)
				reader.Close()
				if err != nil || !bytes.Equal(got, payload[cp.UncompressedOffset:]) {
					t.Fatalf("resume output=%d input=%d bit=%d: %v", cp.UncompressedOffset, cp.CompressedOffset, cp.BitOffset, err)
				}
			}
			if wantUnaligned && !unaligned {
				t.Fatal("nonaligned checkpoint not observed")
			}
		})
	}
}

type checkpointBitWriter struct {
	data []byte
	bits uint
}

func (w *checkpointBitWriter) bit(v byte) {
	if w.bits%8 == 0 {
		w.data = append(w.data, 0)
	}
	w.data[len(w.data)-1] |= (v & 1) << (w.bits % 8)
	w.bits++
}
func (w *checkpointBitWriter) field(v uint, n uint) {
	for i := uint(0); i < n; i++ {
		w.bit(byte(v >> i))
	}
}
func (w *checkpointBitWriter) code(v uint, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		w.bit(byte(v >> i))
	}
}
