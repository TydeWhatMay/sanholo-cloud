package export

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// nonSeekable hides the Seek and WriteAt of a buffer, as the instance's HTTP
// response does, so archive/zip writes data descriptors.
type plainWriter struct{ w io.Writer }

func (p plainWriter) Write(b []byte) (int, error) { return p.w.Write(b) }

func buildZip(t *testing.T, entries map[string][]byte, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(plainWriter{&buf})
	for _, name := range order {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func collect(t *testing.T, data []byte) (map[string][]byte, []string, error) {
	t.Helper()
	got := map[string][]byte{}
	var order []string
	err := readZipStream(bytes.NewReader(data), func(name string, body io.Reader) error {
		b, err := io.ReadAll(body)
		got[name] = b
		order = append(order, name)
		return err
	})
	return got, order, err
}

func TestReadZipStreamReadsWhatGoWrites(t *testing.T) {
	big := make([]byte, 3<<20)
	if _, err := rand.Read(big[:1<<20]); err != nil { // part random, part zeros: both shapes of deflate data
		t.Fatal(err)
	}
	entries := map[string][]byte{
		"catalog.sqlite": big,
		"goals.json":     []byte("[]"),
		"empty.json":     {},
		"README.txt":     []byte("hello"),
	}
	order := []string{"catalog.sqlite", "goals.json", "empty.json", "README.txt"}
	got, gotOrder, err := collect(t, buildZip(t, entries, order))
	if err != nil {
		t.Fatal(err)
	}
	if len(gotOrder) != len(order) {
		t.Fatalf("entries %v, want %v", gotOrder, order)
	}
	for i, name := range order {
		if gotOrder[i] != name || !bytes.Equal(got[name], entries[name]) {
			t.Errorf("entry %q differs (order %v)", name, gotOrder)
		}
	}
}

func TestReadZipStreamAcceptsAnEntryTheCallerLeavesUnread(t *testing.T) {
	data := buildZip(t, map[string][]byte{"a": bytes.Repeat([]byte("x"), 100_000), "b": []byte("second")}, []string{"a", "b"})
	var names []string
	var second []byte
	err := readZipStream(bytes.NewReader(data), func(name string, body io.Reader) error {
		names = append(names, name)
		if name == "b" {
			second, _ = io.ReadAll(body)
		}
		return nil // "a" is not read at all
	})
	if err != nil || len(names) != 2 || string(second) != "second" {
		t.Fatalf("err=%v names=%v second=%q", err, names, second)
	}
}

func TestReadZipStreamSkipsDirectories(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(plainWriter{&buf})
	if _, err := zw.CreateHeader(&zip.FileHeader{Name: "dir/", Method: zip.Store}); err != nil {
		t.Fatal(err)
	}
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "dir/f", Method: zip.Deflate})
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	got, order, err := collect(t, buf.Bytes())
	if err != nil || len(order) != 1 || string(got["dir/f"]) != "x" {
		t.Fatalf("err=%v order=%v", err, order)
	}
}

func TestReadZipStreamRefusesABrokenArchive(t *testing.T) {
	data := buildZip(t, map[string][]byte{"a": bytes.Repeat([]byte("0123456789"), 5000), "b": []byte("tail")}, []string{"a", "b"})
	cases := []struct {
		name string
		data []byte
	}{
		{"cut inside an entry", data[:len(data)/4]},
		{"cut before the central directory", data[:bytes.Index(data, []byte{0x50, 0x4b, 0x01, 0x02})]},
		{"empty", nil},
		{"not an archive", []byte("this is not a zip file at all")},
		{"flipped byte in the compressed data", func() []byte {
			d := append([]byte(nil), data...)
			d[60] ^= 0xFF
			return d
		}()},
		{"wrong checksum", func() []byte {
			d := append([]byte(nil), data...)
			i := bytes.Index(d, []byte{0x50, 0x4b, 0x07, 0x08})
			if i < 0 {
				t.Fatal("no data descriptor to damage: the writer changed")
			}
			d[i+4] ^= 0xFF
			return d
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := collect(t, c.data)
			if err == nil {
				t.Fatal("a broken archive was read to the end")
			}
		})
	}
}

func TestReadZipStreamStopsWhenTheCallerFails(t *testing.T) {
	data := buildZip(t, map[string][]byte{"a": []byte("1"), "b": []byte("2")}, []string{"a", "b"})
	boom := errors.New("boom")
	calls := 0
	err := readZipStream(bytes.NewReader(data), func(string, io.Reader) error { calls++; return boom })
	if !errors.Is(err, boom) || calls != 1 {
		t.Errorf("err=%v calls=%d", err, calls)
	}
}

func TestDataDescriptorSizesAreEightBytesWhenEitherPassesFourGiB(t *testing.T) {
	desc := func(sig bool, crc uint32, c, u uint64, wide bool) *countReader {
		var b bytes.Buffer
		w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
		if sig {
			w(uint32(sigDescriptor))
		}
		w(crc)
		if wide {
			w(c)
			w(u)
		} else {
			w(uint32(c))
			w(uint32(u))
		}
		return &countReader{br: bufio.NewReader(&b)}
	}
	big := uint64(5) << 30
	for _, sig := range []bool{true, false} {
		if crc, err := readDescriptor(desc(sig, 0xdeadbeef, big, 100, true), int64(big), 100); err != nil || crc != 0xdeadbeef {
			t.Errorf("zip64 descriptor (sig %v): %v %x", sig, err, crc)
		}
		if _, err := readDescriptor(desc(sig, 1, 10, 20, false), 10, 20); err != nil {
			t.Errorf("plain descriptor (sig %v): %v", sig, err)
		}
		if _, err := readDescriptor(desc(sig, 1, big, 100, true), int64(big)+1, 100); err == nil {
			t.Errorf("a wrong zip64 size was accepted (sig %v)", sig)
		}
		if _, err := readDescriptor(desc(sig, 1, 10, 20, false), 10, 21); err == nil {
			t.Errorf("a wrong size was accepted (sig %v)", sig)
		}
	}
}
