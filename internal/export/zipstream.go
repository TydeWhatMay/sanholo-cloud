package export

import (
	"bufio"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
)

// A streaming reader for the ZIP the SanHolo instance writes.
//
// archive/zip reads from an io.ReaderAt (it needs the central directory at
// the end), so using it would mean holding the instance's whole export, a
// catalog of possibly several hundred megabytes, in memory or on disk before
// the first byte could go to the customer. The instance writes its archive
// with archive/zip to a plain stream, so every entry is a local header, a
// deflate stream and a data descriptor, in order. This reader walks that
// layout, hands each entry's decompressed bytes to the caller as they arrive
// and checks the CRC and sizes against the descriptor. It accepts what Go's
// writer produces (deflate with a data descriptor, or any entry whose sizes
// are in its local header) and refuses everything else.

const (
	sigLocal      = 0x04034b50
	sigDescriptor = 0x08074b50
	sigCentral    = 0x02014b50
	sigEnd        = 0x06054b50
	sigEnd64      = 0x06064b50

	flagEncrypted  = 0x1
	flagDescriptor = 0x8

	methodStore   = 0
	methodDeflate = 8
)

var errZip = errors.New("export: the instance's archive is not readable")

// countReader counts the bytes taken from br. It has ReadByte so that
// compress/flate takes exactly the bytes of the deflate stream and no more.
type countReader struct {
	br *bufio.Reader
	n  int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.br.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countReader) ReadByte() (byte, error) {
	b, err := c.br.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// entryReader yields one entry's decompressed bytes and tracks their CRC and
// length.
type entryReader struct {
	r    io.Reader
	crc  hash.Hash32
	size int64
}

func (e *entryReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if n > 0 {
		_, _ = e.crc.Write(p[:n])
		e.size += int64(n)
	}
	return n, err
}

// readZipStream calls fn for each file entry with its decompressed bytes. fn
// may read less than all of them; the rest is read and discarded. It returns
// nil when the central directory is reached, which proves the archive was
// complete. A directory entry is skipped.
func readZipStream(src io.Reader, fn func(name string, body io.Reader) error) error {
	cr := &countReader{br: bufio.NewReaderSize(src, 64<<10)}
	for {
		sig, err := readU32(cr)
		if err != nil {
			return fmt.Errorf("%w: %w", errZip, noEOF(err))
		}
		switch sig {
		case sigCentral, sigEnd, sigEnd64:
			return nil
		case sigLocal:
		default:
			return fmt.Errorf("%w: unexpected record", errZip)
		}
		var h [26]byte
		if _, err := io.ReadFull(cr, h[:]); err != nil {
			return fmt.Errorf("%w: %w", errZip, noEOF(err))
		}
		flags := binary.LittleEndian.Uint16(h[2:])
		method := binary.LittleEndian.Uint16(h[4:])
		crc := binary.LittleEndian.Uint32(h[10:])
		csize := binary.LittleEndian.Uint32(h[14:])
		usize := binary.LittleEndian.Uint32(h[18:])
		nameLen := int(binary.LittleEndian.Uint16(h[22:]))
		extraLen := int(binary.LittleEndian.Uint16(h[24:]))
		name := make([]byte, nameLen)
		if _, err := io.ReadFull(cr, name); err != nil {
			return fmt.Errorf("%w: %w", errZip, noEOF(err))
		}
		if _, err := io.CopyN(io.Discard, cr, int64(extraLen)); err != nil {
			return fmt.Errorf("%w: %w", errZip, noEOF(err))
		}
		if flags&flagEncrypted != 0 {
			return fmt.Errorf("%w: encrypted entry", errZip)
		}
		descriptor := flags&flagDescriptor != 0
		start := cr.n
		var body io.Reader
		switch {
		case method == methodDeflate:
			body = flate.NewReader(cr)
		case method == methodStore && !descriptor:
			body = io.LimitReader(cr, int64(csize))
		default:
			return fmt.Errorf("%w: unsupported entry", errZip)
		}
		er := &entryReader{r: body, crc: crc32.NewIEEE()}
		if n := len(name); n > 0 && name[n-1] == '/' {
			// A directory has no data; the reader above is not read.
			if _, err := io.Copy(io.Discard, er); err != nil {
				return fmt.Errorf("%w: %w", errZip, noEOF(err))
			}
			continue
		}
		if err := fn(string(name), er); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, er); err != nil {
			return fmt.Errorf("%w: %w", errZip, noEOF(err))
		}
		compressed := cr.n - start
		if descriptor {
			wantCRC, err := readDescriptor(cr, compressed, er.size)
			if err != nil {
				return err
			}
			crc = wantCRC
		} else if int64(usize) != er.size {
			return fmt.Errorf("%w: size mismatch", errZip)
		}
		if er.crc.Sum32() != crc {
			return fmt.Errorf("%w: checksum mismatch", errZip)
		}
	}
}

// readDescriptor reads the data descriptor after an entry and checks its
// sizes against what was read. Sizes are 8 bytes each when either exceeds
// 32 bits (Go's writer switches to the ZIP64 form then), otherwise 4.
func readDescriptor(cr *countReader, compressed, uncompressed int64) (crc uint32, err error) {
	first, err := readU32(cr)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errZip, noEOF(err))
	}
	if first == sigDescriptor { // the signature is optional
		if first, err = readU32(cr); err != nil {
			return 0, fmt.Errorf("%w: %w", errZip, noEOF(err))
		}
	}
	crc = first
	if compressed >= 0xFFFFFFFF || uncompressed >= 0xFFFFFFFF {
		var b [16]byte
		if _, err := io.ReadFull(cr, b[:]); err != nil {
			return 0, fmt.Errorf("%w: %w", errZip, noEOF(err))
		}
		c, u := binary.LittleEndian.Uint64(b[:8]), binary.LittleEndian.Uint64(b[8:])
		if c > math.MaxInt64 || u > math.MaxInt64 || int64(c) != compressed || int64(u) != uncompressed {
			return 0, fmt.Errorf("%w: size mismatch", errZip)
		}
		return crc, nil
	}
	var b [8]byte
	if _, err := io.ReadFull(cr, b[:]); err != nil {
		return 0, fmt.Errorf("%w: %w", errZip, noEOF(err))
	}
	if int64(binary.LittleEndian.Uint32(b[:4])) != compressed || int64(binary.LittleEndian.Uint32(b[4:])) != uncompressed {
		return 0, fmt.Errorf("%w: size mismatch", errZip)
	}
	return crc, nil
}

func readU32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

// noEOF turns a bare EOF into ErrUnexpectedEOF: in the middle of an archive
// the end of the stream is a cut-off, not a clean end.
func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
