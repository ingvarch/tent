package nodeuptest_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

// entry is what a test reads back from an archive.
type entry struct {
	Name     string
	Typeflag byte
	Mode     int64
	Size     int64
	Content  string
}

// readTgz returns the entries of a gzip tar, and the error that stopped the reading, if any.
func readTgz(t *testing.T, data []byte) ([]entry, error) {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !zr.ModTime.IsZero() {
		t.Errorf("the gzip header has the time %s, want none", zr.ModTime)
	}
	var entries []entry
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return entries, err
		}
		content, err := io.ReadAll(tr)
		entries = append(entries, entry{h.Name, h.Typeflag, h.Mode, h.Size, string(content)})
		if err != nil {
			return entries, err
		}
	}
}

// files are the files of an archive: its top directory, two programs and a licence.
var files = []nodeuptest.TarFile{
	{Header: tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}},
	{Header: tar.Header{Name: "./bridge", Mode: 0o755}, Content: []byte("bridge plugin")},
	{Header: tar.Header{Name: "./loopback", Typeflag: tar.TypeReg, Mode: 0o755}, Content: []byte("loopback plugin")},
	{Header: tar.Header{Name: "./LICENSE", Mode: 0o644}, Content: []byte("Apache License\n")},
}

func TestTgz(t *testing.T) {
	data := nodeuptest.Tgz(t, files...)
	got, err := readTgz(t, data)
	if err != nil {
		t.Fatal(err)
	}
	want := []entry{
		{"./", tar.TypeDir, 0o755, 0, ""},
		{"./bridge", tar.TypeReg, 0o755, 13, "bridge plugin"},
		{"./loopback", tar.TypeReg, 0o755, 15, "loopback plugin"},
		{"./LICENSE", tar.TypeReg, 0o644, 15, "Apache License\n"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("entries (-want +got):\n%s", diff)
	}
}

// big is a file larger than a stored deflate block, 65535 bytes.
var big = nodeuptest.TarFile{
	Header: tar.Header{Name: "./big", Mode: 0o755}, Content: bytes.Repeat([]byte("0123456789"), 7000),
}

func TestTgzBytes(t *testing.T) {
	// The sums hold on every Go: specs hash the sha256 of such archives into golden files. They were worked out apart
	// from Tgz, from the tar and the formats of gzip and deflate.
	for _, c := range []struct {
		name  string
		files []nodeuptest.TarFile
		sum   string
	}{
		{"one block", files, "53773e165c985dd808130b22898932b7a086cdc91704850e7b39b7a402ab9f54"},
		{"two blocks", append(slices.Clone(files), big), "c36fff63198ac9c73eea78b8096a3feab1aaca907f07ba8d7a65e06e65a9829a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sum := sha256.Sum256(nodeuptest.Tgz(t, c.files...))
			if got := hex.EncodeToString(sum[:]); got != c.sum {
				t.Errorf("the archive's sha256 is %s, want %s", got, c.sum)
			}
		})
	}
}

func TestTgzOfTwoBlocks(t *testing.T) {
	got, err := readTgz(t, nodeuptest.Tgz(t, files[0], big))
	if err != nil {
		t.Fatal(err)
	}
	want := []entry{{"./", tar.TypeDir, 0o755, 0, ""}, {"./big", tar.TypeReg, 0o755, 70000, string(big.Content)}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("entries (-want +got):\n%s", diff)
	}
}

func TestTgzGivesTheSameBytes(t *testing.T) {
	data := nodeuptest.Tgz(t, files...)
	// Another clock gives the same bytes: synctest's starts in 2000.
	synctest.Test(t, func(t *testing.T) {
		if again := nodeuptest.Tgz(t, files...); !bytes.Equal(data, again) {
			t.Error("the same files at another time gave another archive")
		}
	})
}

func TestTgzEndsInsideAFileLargerThanItsContent(t *testing.T) {
	data := nodeuptest.Tgz(t, files[1], nodeuptest.TarFile{
		Header: tar.Header{Name: "./huge", Mode: 0o755, Size: 1 << 30}, Content: []byte("the start"),
	}, files[3])
	got, err := readTgz(t, data)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("reading the archive ended with %v, want %v", err, io.ErrUnexpectedEOF)
	}
	want := []entry{
		{"./bridge", tar.TypeReg, 0o755, 13, "bridge plugin"},
		{"./huge", tar.TypeReg, 0o755, 1 << 30, "the start"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("entries (-want +got):\n%s", diff)
	}
}
