package git

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"sync"

	"gitforge/internal/store"
)

var emptyZlibStream = []byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01}

var zlibReaderPool = sync.Pool{
	New: func() any {
		r, err := zlib.NewReader(bytes.NewReader(emptyZlibStream))
		if err != nil {
			return nil
		}
		return r
	},
}

var flateReaderPool = sync.Pool{
	New: func() any {
		return flate.NewReader(&sliceReader{})
	},
}

var zlibWriterPool = sync.Pool{
	New: func() any {
		w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
		return w
	},
}

var zlibBestCompPool = sync.Pool{
	New: func() any {
		w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestCompression)
		return w
	},
}

// sliceReader implements io.Reader and io.ByteReader over a []byte without heap allocation,
// so compress/zlib never allocates an internal bufio.Reader and stops at the exact stream boundary.
type sliceReader struct {
	b   []byte
	pos int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.b) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.pos:])
	s.pos += n
	return n, nil
}

func (s *sliceReader) ReadByte() (byte, error) {
	if s.pos >= len(s.b) {
		return 0, io.EOF
	}
	c := s.b[s.pos]
	s.pos++
	return c, nil
}

func readPackSize(buf []byte, off int) (objType uint8, size int, next int, err error) {
	if off >= len(buf) {
		return 0, 0, 0, errors.New("pack entry truncated")
	}
	b := buf[off]
	off++
	objType = (b >> 4) & 0x07
	sz := uint64(b & 0x0f)
	shift := uint(4)
	for b&0x80 != 0 {
		if off >= len(buf) {
			return 0, 0, 0, errors.New("pack size varint truncated")
		}
		b = buf[off]
		off++
		sz |= uint64(b&0x7f) << shift
		shift += 7
		if shift > 60 {
			return 0, 0, 0, errors.New("pack size varint overflow")
		}
	}
	return objType, int(sz), off, nil
}

func readOfsDeltaOffset(buf []byte, off int) (back int, next int, err error) {
	if off >= len(buf) {
		return 0, 0, errors.New("ofs-delta offset truncated")
	}
	b := buf[off]
	off++
	val := int64(b & 0x7f)
	for b&0x80 != 0 {
		if off >= len(buf) {
			return 0, 0, errors.New("ofs-delta offset truncated")
		}
		b = buf[off]
		off++
		val = ((val + 1) << 7) | int64(b&0x7f)
	}
	return int(val), off, nil
}

func inflatePackEntryInto(r *store.Repo, data []byte, expectedSize int) (out []byte, consumed int, err error) {
	if len(data) >= 6 {
		cmf := data[0]
		flg := data[1]
		if cmf&0x0f == 8 && cmf>>4 <= 7 && ((uint16(cmf)<<8)|uint16(flg))%31 == 0 && flg&0x20 == 0 {
			sr := sliceReader{b: data[2:]}
			var fr io.ReadCloser
			if pooled := flateReaderPool.Get(); pooled != nil {
				fr = pooled.(io.ReadCloser)
				if err = fr.(flate.Resetter).Reset(&sr, nil); err != nil {
					_ = fr.Close()
					fr = flate.NewReader(&sr)
				}
			} else {
				fr = flate.NewReader(&sr)
			}
			if expectedSize > 0 {
				if r != nil {
					out = r.AllocBytes(expectedSize)
				} else {
					out = make([]byte, expectedSize)
				}
				if _, err = io.ReadFull(fr, out); err != nil {
					_ = fr.Close()
					flateReaderPool.Put(fr)
					return nil, 0, err
				}
			}
			var extra [1]byte
			n, rerr := fr.Read(extra[:])
			if n > 0 || (rerr != nil && rerr != io.EOF) {
				_ = fr.Close()
				flateReaderPool.Put(fr)
				if rerr == nil {
					return nil, 0, errors.New("inflated pack object larger than declared size")
				}
				return nil, 0, rerr
			}
			_ = fr.Close()
			flateReaderPool.Put(fr)
			if 2+sr.pos+4 > len(data) {
				return nil, 0, errors.New("zlib trailer truncated")
			}
			wantAdler := binary.BigEndian.Uint32(data[2+sr.pos : 2+sr.pos+4])
			if adler32.Checksum(out) != wantAdler {
				return nil, 0, zlib.ErrChecksum
			}
			return out, 2 + sr.pos + 4, nil
		}
	}

	sr := sliceReader{b: data}
	var zr io.ReadCloser
	if pooled := zlibReaderPool.Get(); pooled != nil {
		zr = pooled.(io.ReadCloser)
		if err = zr.(zlib.Resetter).Reset(&sr, nil); err != nil {
			_ = zr.Close()
			zr, err = zlib.NewReader(&sr)
			if err != nil {
				return nil, 0, err
			}
		}
	} else {
		zr, err = zlib.NewReader(&sr)
		if err != nil {
			return nil, 0, err
		}
	}

	if expectedSize > 0 {
		if r != nil {
			out = r.AllocBytes(expectedSize)
		} else {
			out = make([]byte, expectedSize)
		}
		if _, err = io.ReadFull(zr, out); err != nil {
			_ = zr.Close()
			zlibReaderPool.Put(zr)
			return nil, 0, err
		}
	}
	// Trigger EOF + Adler-32 trailer verification.
	var extra [1]byte
	n, rerr := zr.Read(extra[:])
	if n > 0 || (rerr != nil && rerr != io.EOF) {
		_ = zr.Close()
		zlibReaderPool.Put(zr)
		if rerr == nil {
			return nil, 0, errors.New("inflated pack object larger than declared size")
		}
		return nil, 0, rerr
	}
	if cerr := zr.Close(); cerr != nil {
		zlibReaderPool.Put(zr)
		return nil, 0, cerr
	}
	zlibReaderPool.Put(zr)
	return out, sr.pos, nil
}

func readDeltaSize(delta []byte, off int) (int, int, error) {
	var val uint64
	var shift uint
	for {
		if off >= len(delta) {
			return 0, 0, errors.New("delta size truncated")
		}
		b := delta[off]
		off++
		val |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
		if shift > 60 {
			return 0, 0, errors.New("delta size overflow")
		}
	}
	return int(val), off, nil
}

// ApplyDelta applies a Git binary delta stream onto base bytes.
func ApplyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	srcSize, next, err := readDeltaSize(delta, off)
	if err != nil {
		return nil, err
	}
	off = next
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta base size mismatch: want %d got %d", srcSize, len(base))
	}
	dstSize, next, err := readDeltaSize(delta, off)
	if err != nil {
		return nil, err
	}
	off = next

	out := make([]byte, dstSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			var copyOff, copySize int
			for bit := uint(0); bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy offset truncated")
					}
					copyOff |= int(delta[off]) << (bit * 8)
					off++
				}
			}
			for bit := uint(0); bit < 3; bit++ {
				if op&(1<<(bit+4)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy size truncated")
					}
					copySize |= int(delta[off]) << (bit * 8)
					off++
				}
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOff+copySize > len(base) || written+copySize > dstSize {
				return nil, errors.New("delta copy out of bounds")
			}
			copy(out[written:written+copySize], base[copyOff:copyOff+copySize])
			written += copySize
		} else if op != 0 {
			lit := int(op & 0x7f)
			if off+lit > len(delta) || written+lit > dstSize {
				return nil, errors.New("delta insert out of bounds")
			}
			copy(out[written:written+lit], delta[off:off+lit])
			off += lit
			written += lit
		} else {
			return nil, errors.New("invalid delta opcode 0")
		}
	}
	if written != dstSize {
		return nil, fmt.Errorf("delta output truncated: %d/%d", written, dstSize)
	}
	return out, nil
}

// ParsePackAndStoreLocked validates a v2/v3 packfile, resolves all deltas, and stores
// every object into r while r.GitMu is held.
func ParsePackAndStoreLocked(r *store.Repo, pack []byte) error {
	if len(pack) < 32 {
		return errors.New("pack too short")
	}
	if string(pack[:4]) != "PACK" {
		return errors.New("pack missing PACK magic")
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 && ver != 3 {
		return fmt.Errorf("unsupported pack version %d", ver)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20

	// Verify trailing SHA-1 checksum.
	sum := sha1.Sum(pack[:trailerStart])
	if !bytes.Equal(sum[:], pack[trailerStart:]) {
		return errors.New("pack checksum mismatch")
	}

	if count == 0 {
		if trailerStart != 12 {
			return errors.New("unexpected trailing pack bytes")
		}
		return nil
	}

	type rawEntry struct {
		offset       int
		typeCode     uint8
		data         []byte
		baseOffset   int
		baseOID      [20]byte
		resolvedType store.ObjType
		resolvedData []byte
		oid          [20]byte
		done         bool
		resolving    bool
	}

	var stackEntries [16]rawEntry
	var entries []rawEntry
	if count <= len(stackEntries) {
		entries = stackEntries[:count]
	} else {
		entries = make([]rawEntry, count)
	}
	var byOffset map[int]*rawEntry
	off := 12
	hasDeltas := false

	for i := 0; i < count; i++ {
		entryOff := off
		tc, sz, next, err := readPackSize(pack[:trailerStart], off)
		if err != nil {
			return err
		}
		off = next
		e := &entries[i]
		e.offset = entryOff
		e.typeCode = tc

		switch tc {
		case 1, 2, 3, 4:
		case 6: // OFS_DELTA
			hasDeltas = true
			if byOffset == nil {
				byOffset = make(map[int]*rawEntry, count)
				for j := 0; j < i; j++ {
					byOffset[entries[j].offset] = &entries[j]
				}
			}
			back, next2, err := readOfsDeltaOffset(pack[:trailerStart], off)
			if err != nil {
				return err
			}
			off = next2
			if back <= 0 || back > entryOff {
				return errors.New("invalid ofs-delta base offset")
			}
			e.baseOffset = entryOff - back
		case 7: // REF_DELTA
			hasDeltas = true
			if off+20 > trailerStart {
				return errors.New("ref-delta base oid truncated")
			}
			copy(e.baseOID[:], pack[off:off+20])
			off += 20
		default:
			return fmt.Errorf("unknown pack object type %d", tc)
		}

		inflated, consumed, err := inflatePackEntryInto(r, pack[off:trailerStart], sz)
		if err != nil {
			return fmt.Errorf("pack entry %d inflate: %w", i, err)
		}
		e.data = inflated
		off += consumed
		if byOffset != nil {
			byOffset[entryOff] = e
		}
	}

	if off != trailerStart {
		return fmt.Errorf("pack trailing bytes: %d", trailerStart-off)
	}

	var resolvedByOID map[[20]byte]*rawEntry
	if hasDeltas {
		resolvedByOID = make(map[[20]byte]*rawEntry, count)
	}
	var resolve func(e *rawEntry) error
	resolve = func(e *rawEntry) error {
		if e.done {
			return nil
		}
		if e.resolving {
			return errors.New("pack delta cycle")
		}
		e.resolving = true
		switch e.typeCode {
		case 1, 2, 3, 4:
			e.resolvedType = store.ObjType(e.typeCode)
			e.resolvedData = e.data
		case 6:
			base := byOffset[e.baseOffset]
			if base == nil {
				return errors.New("missing ofs-delta base")
			}
			if err := resolve(base); err != nil {
				return err
			}
			merged, err := ApplyDelta(base.resolvedData, e.data)
			if err != nil {
				return err
			}
			e.resolvedType = base.resolvedType
			e.resolvedData = merged
		case 7:
			var baseType store.ObjType
			var baseData []byte
			if base := resolvedByOID[e.baseOID]; base != nil {
				if err := resolve(base); err != nil {
					return err
				}
				baseType = base.resolvedType
				baseData = base.resolvedData
			} else if ext := r.Objects[e.baseOID]; ext != nil {
				baseType = ext.Type
				baseData = ext.Data
			} else {
				return fmt.Errorf("missing ref-delta base %s", FormatOID(e.baseOID))
			}
			merged, err := ApplyDelta(baseData, e.data)
			if err != nil {
				return err
			}
			e.resolvedType = baseType
			e.resolvedData = merged
		}
		e.oid = HashObject(e.resolvedType, e.resolvedData)
		e.resolving = false
		e.done = true
		if resolvedByOID != nil {
			resolvedByOID[e.oid] = e
		}
		return nil
	}

	var stackCommits [4][20]byte
	newCommits := stackCommits[:0]
	for i := range entries {
		e := &entries[i]
		if hasDeltas {
			if err := resolve(e); err != nil {
				return err
			}
		} else {
			e.resolvedType = store.ObjType(e.typeCode)
			e.resolvedData = e.data
			e.oid = HashObject(e.resolvedType, e.resolvedData)
		}
		if _, exists := r.Objects[e.oid]; !exists {
			r.Objects[e.oid] = r.AllocGitObject(e.resolvedType, e.resolvedData)
		}
		if e.resolvedType == store.ObjCommit {
			if _, exists := r.Commits[e.oid]; !exists {
				info := r.AllocCommitInfo()
				if err := ParseCommitFastInto(e.resolvedData, info); err != nil {
					return err
				}
				r.Commits[e.oid] = info
				newCommits = append(newCommits, e.oid)
			}
		}
	}

	// Compute commit heights accurately regardless of commit order inside the pack.
	if len(newCommits) == 1 {
		ci := r.Commits[newCommits[0]]
		if ci != nil && ci.Height == 0 {
			var maxP uint32
			for _, p := range ci.Parents {
				if pi := r.Commits[p]; pi != nil && pi.Height > maxP {
					maxP = pi.Height
				}
			}
			ci.Height = maxP + 1
		}
	} else if len(newCommits) > 1 {
		var calcHeight func(oid [20]byte) uint32
		calcHeight = func(oid [20]byte) uint32 {
			ci := r.Commits[oid]
			if ci == nil {
				return 0
			}
			if ci.Height > 0 {
				return ci.Height
			}
			var maxP uint32
			for _, p := range ci.Parents {
				if ph := calcHeight(p); ph > maxP {
					maxP = ph
				}
			}
			ci.Height = maxP + 1
			return ci.Height
		}
		for _, oid := range newCommits {
			calcHeight(oid)
		}
	}
	return nil
}

func appendPackObjHeader(dst []byte, objType store.ObjType, size int) []byte {
	rem := size
	first := byte((uint8(objType) << 4) | uint8(rem&0x0f))
	rem >>= 4
	for rem > 0 {
		dst = append(dst, first|0x80)
		first = byte(rem & 0x7f)
		rem >>= 7
	}
	return append(dst, first)
}

// WritePack serializes objects into a valid v2 Git packfile using fast zlib compression.
func WritePack(objects []*store.GitObject) ([]byte, error) {
	return WritePackLevel(objects, zlib.BestSpeed)
}

// WritePackLevel serializes objects into a valid v2 Git packfile at the requested zlib level.
func WritePackLevel(objects []*store.GitObject, level int) ([]byte, error) {
	est := 32 + len(objects)*64
	out := make([]byte, 12, est)
	copy(out[:4], "PACK")
	binary.BigEndian.PutUint32(out[4:8], 2)
	binary.BigEndian.PutUint32(out[8:12], uint32(len(objects)))

	pool := &zlibWriterPool
	if level == zlib.BestCompression {
		pool = &zlibBestCompPool
	}
	zw := pool.Get().(*zlib.Writer)
	var zbuf bytes.Buffer
	zbuf.Grow(4096)

	for _, obj := range objects {
		out = appendPackObjHeader(out, obj.Type, len(obj.Data))
		zbuf.Reset()
		zw.Reset(&zbuf)
		if _, err := zw.Write(obj.Data); err != nil {
			pool.Put(zw)
			return nil, err
		}
		if err := zw.Close(); err != nil {
			pool.Put(zw)
			return nil, err
		}
		out = append(out, zbuf.Bytes()...)
	}
	pool.Put(zw)

	sum := sha1.Sum(out)
	out = append(out, sum[:]...)
	return out, nil
}
