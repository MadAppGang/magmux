package source

import (
	"bytes"
	"io"
	"io/fs"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

// genFile is a file of size bytes, byte i being byte(i%251), generated as it
// is read: nothing of it exists in memory. It deliberately does NOT implement
// io.Seeker or io.ReaderAt. info is what its Stat reports, which a test may
// make lie.
type genFile struct {
	size, pos int64
	info      fileInfo
}

func (f *genFile) Read(p []byte) (int, error) {
	if f.pos >= f.size {
		return 0, io.EOF
	}
	n := int64(len(p))
	if rest := f.size - f.pos; n > rest {
		n = rest
	}
	for i := int64(0); i < n; i++ {
		p[i] = byte((f.pos + i) % 251)
	}
	f.pos += n
	return int(n), nil
}
func (f *genFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *genFile) Close() error               { return nil }

type fileInfo struct {
	name string
	size int64
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return 0o644 }
func (i fileInfo) ModTime() time.Time { return time.Time{} }
func (i fileInfo) IsDir() bool        { return false }
func (i fileInfo) Sys() any           { return nil }

// genFS serves one generated file. statSize is what BOTH fs.Stat and the
// handle's Stat report: equal to size for an honest file, smaller for a file
// swapped between the check and the read.
type genFS struct {
	name           string
	size, statSize int64
}

func (g genFS) Open(name string) (fs.File, error) {
	if name != g.name {
		return nil, fs.ErrNotExist
	}
	return &genFile{size: g.size, info: fileInfo{name: name, size: g.statSize}}, nil
}

func (g genFS) Stat(name string) (fs.FileInfo, error) {
	if name != g.name {
		return nil, fs.ErrNotExist
	}
	return fileInfo{name: name, size: g.statSize}, nil
}

func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// D10: a tail is a BOUNDED read whatever FS a host supplies. A file that
// cannot seek is read through and its head discarded; it is never held whole
// (review r1 #10b).
func TestATailOfAnUnseekableFileHoldsOnlyTheTail(t *testing.T) {
	const size, n = 64 << 20, 4 << 10
	fsys := genFS{name: "t.jsonl", size: size, statSize: size}
	var (
		buf  []byte
		got  int64
		terr error
	)
	alloc := allocatedBy(func() { buf, got, terr = Tail(fsys, "t.jsonl", n) })
	if terr != nil || got != size {
		t.Fatalf("Tail: size %d err %v", got, terr)
	}
	want := make([]byte, n)
	for i := range want {
		want[i] = byte((int64(size-n) + int64(i)) % 251)
	}
	if !bytes.Equal(buf, want) {
		t.Fatal("the bytes returned are not the file's last n")
	}
	if alloc > 8<<20 {
		t.Fatalf("a %d-byte tail of a %d-byte file allocated %d bytes: the file was held whole", n, size, alloc)
	}
}

// A record is bounded by the HANDLE it is read through, not by a check of its
// name made before the read: a file swapped in between, or one whose size is
// misreported, is still never read past claude.MaxRecordBytes (review r1 #13b).
func TestARecordIsBoundedByTheHandleItIsReadThrough(t *testing.T) {
	fsys := genFS{name: "1.json", size: 64 * claude.MaxRecordBytes, statSize: 10}
	var err error
	alloc := allocatedBy(func() { _, err = readBounded(fsys, "1.json") })
	if err == nil || !strings.Contains(err.Error(), "record bound") {
		t.Fatalf("a record past the bound was read: err %v", err)
	}
	if alloc > 16*claude.MaxRecordBytes {
		t.Fatalf("reading a record allocated %d bytes for a %d-byte bound: the file was read whole", alloc, claude.MaxRecordBytes)
	}
}
