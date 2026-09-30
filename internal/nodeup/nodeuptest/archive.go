package nodeuptest

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
	"time"
)

// TarFile is an entry of an archive that Tgz makes: its header and, for a regular file, its content.
type TarFile struct {
	Header  tar.Header
	Content []byte
}

// archiveTime is the time of every entry that Tgz writes without one.
var archiveTime = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// Tgz returns a gzip tar of the files, in order, as the CNI plugins' releases are. It gives an entry without a size
// the size of its content, and one without a time archiveTime. gzip stores the data uncompressed and records no time,
// so the same files give the same bytes, whatever the clock and the Go version. A file whose size is larger than its
// content ends the archive inside it, as a download cut short does.
func Tgz(t testing.TB, files ...TarFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		h := f.Header
		if h.Size == 0 {
			h.Size = int64(len(f.Content))
		}
		if h.ModTime.IsZero() {
			h.ModTime = archiveTime
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("nodeuptest: tar %s: %v", h.Name, err)
		}
		if _, err := tw.Write(f.Content); err != nil {
			t.Fatalf("nodeuptest: tar %s: %v", h.Name, err)
		}
		if h.Size > int64(len(f.Content)) {
			return storedGzip(buf.Bytes())
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return storedGzip(buf.Bytes())
}

// maxStoredBlock is the most bytes that a stored deflate block holds.
const maxStoredBlock = 0xffff

// storedGzip returns data as a gzip stream written by hand, so that its bytes depend on data alone: a header without a
// name, a time or flags, for an unknown OS; stored deflate blocks of at most maxStoredBlock bytes, the last one final
// (RFC 1951 3.2.4), at least one of them; and data's CRC-32 and its size mod 2^32 (RFC 1952).
func storedGzip(data []byte) []byte {
	out := []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff}
	rest := data
	for {
		n := min(len(rest), maxStoredBlock)
		final := n == len(rest)
		var flags byte // BFINAL in bit 0; BTYPE 00, stored, in bits 1 and 2
		if final {
			flags = 1
		}
		out = append(out, flags)
		out = binary.LittleEndian.AppendUint16(out, uint16(n))
		out = binary.LittleEndian.AppendUint16(out, ^uint16(n))
		out = append(out, rest[:n]...)
		rest = rest[n:]
		if final {
			break
		}
	}
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(data))
	return binary.LittleEndian.AppendUint32(out, uint32(len(data)))
}
