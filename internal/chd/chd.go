package chd

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha1" //nolint:gosec // SHA1 is required by the CHD v5 format
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/ilyalavrenov/swirl/internal/disc"
)

// A hunk holds framesPerHunk slots: 2352 bytes of sector data, then 96 of subcode.
const (
	headerMagic   = "MComprHD"
	headerSize    = 124
	chdVersion    = 5
	sectorBytes   = disc.SectorBytes
	subcodeBytes  = 96                         // always written as zeros
	slotBytes     = sectorBytes + subcodeBytes // 2448
	framesPerHunk = 8
	hunkBytes     = framesPerHunk * slotBytes // 19584
	trackPadding  = 4

	// Read accepts all four; Write emits only cdzl.
	codecCDZlib = uint32('c')<<24 | uint32('d')<<16 | uint32('z')<<8 | uint32('l')
	codecCDLzma = uint32('c')<<24 | uint32('d')<<16 | uint32('l')<<8 | uint32('z')
	codecCDFlac = uint32('c')<<24 | uint32('d')<<16 | uint32('f')<<8 | uint32('l')
	codecCDZstd = uint32('c')<<24 | uint32('d')<<16 | uint32('z')<<8 | uint32('s')

	// Types 0-3 index the header's compressor slots, so one file mixes codecs.
	// https://github.com/mamedev/mame/blob/33c42e9e0e89c879e0fc5b654cc70b947bf1473c/src/lib/util/chd.cpp#L68-L76
	mapCompType0 = 0
	mapCompType1 = 1
	mapCompType2 = 2
	mapCompType3 = 3
	mapCompNone  = 4

	// RLE and PARENT are read-side only; chdman emits them.
	mapCompSelf       = 5
	mapCompParent     = 6
	mapCompRLESmall   = 7
	mapCompRLELarge   = 8
	mapCompSelf0      = 9
	mapCompSelf1      = 10
	mapCompParentSelf = 11
	mapCompParent0    = 12
	mapCompParent1    = 13

	// Flycast branches on the tag: CHT2 adds an 11400-frame SESSION_GAP to the last
	// track's StartFAD; CHGD calls FillGDSession(), fixing track 3 at FAD 45150 with
	// no gap. A GD-ROM must use CHGD or its sector addressing is wrong.
	// https://github.com/flyinghead/flycast/blob/e2722869ffb6f404d2056a12653aa67e4210d61d/core/imgread/chd.cpp#L165-L177
	tagCHT2 = uint32('C')<<24 | uint32('H')<<16 | uint32('T')<<8 | uint32('2')
	tagCHGD = uint32('C')<<24 | uint32('H')<<16 | uint32('G')<<8 | uint32('D')

	// CHD_MDFLAGS_CHECKSUM includes the entry in the overall SHA1.
	// https://github.com/rtissera/libchdr/blob/5f82799f2c8cad1e9cd26d39a0f8d36369a5534b/include/libchdr/chd.h#L226
	metaFlags = 0x01

	metaEntryHeaderBytes = 16

	tagBytes = 4

	headerCodecOffset        = 0x10
	headerRawSHA1Offset      = 0x40
	headerCombinedSHA1Offset = 0x54

	numCompressors = 4

	// The entry size the map CRC is computed over.
	mapEntryBytes     = 12
	mapHeaderBytes    = 16
	mapCRCBits        = 16
	bitsPerByte       = 8
	bitWriterMSBIndex = bitsPerByte - 1

	huffmanRLEBitWidth = 4

	// The writer's fixed map tree: a compressed hunk in one bit, the four other symbols it
	// emits in three.
	mapCommonCodeBits = 1
	mapOtherCodeBits  = 3

	crcInitValue  = 0xFFFF
	crcHighBit    = 0x8000
	crcPolynomial = 0x1021

	outputFileMode = 0o644
)

// ceil(log2(hunkBytes+1)), enough for any compressed hunk length.
const lengthBits = 15

const (
	chdTypeMode1Raw = "MODE1_RAW"
	chdTypeMode2Raw = "MODE2_RAW"
)

type Track struct {
	Number int
	Type   string // CUE type: "MODE1/2352", "AUDIO", etc.
	Frames int    // total frames in source file (including any pregap)
	Pregap int    // pregap frames (INDEX 00 → INDEX 01 difference)

	// StoredFrames pads the GD-ROM bridge track so the tracks before track 3 sum to
	// exactly 45000 slots, which FillGDSession()'s fixed StartFAD of 45150 assumes.
	// https://github.com/flyinghead/flycast/blob/e2722869ffb6f404d2056a12653aa67e4210d61d/core/imgread/common.h#L130
	StoredFrames int

	Data io.Reader // raw sector data: Frames × sectorBytes
}

func effectiveFrames(t Track) int {
	if t.StoredFrames > 0 {
		return t.StoredFrames
	}

	return PadFrames(t.Frames)
}

// Sizes a progress indicator before Write runs.
func WriteBytes(tracks []Track) int64 {
	return int64(hunkCount(storedFrames(tracks))) * hunkBytes
}

func storedFrames(tracks []Track) int {
	total := 0
	for _, t := range tracks {
		total += effectiveFrames(t)
	}

	return total
}

func hunkCount(frames int) int {
	return (frames + framesPerHunk - 1) / framesPerHunk
}

type hunkRecord struct {
	compType uint8 // mapCompType0, mapCompNone or mapCompSelf
	length   uint32
	rawCRC   uint16 // CRC16-CCITT of the uncompressed hunk
	selfHunk int    // the identical earlier hunk, when compType is mapCompSelf
}

type layout struct {
	metaTexts [][]byte
	metaTag   uint32
	numHunks  int
}

func planLayout(tracks []Track) layout {
	tag := tagCHT2
	if isGDROM(tracks) {
		tag = tagCHGD
	}

	return layout{
		metaTexts: buildMetaTexts(tracks),
		metaTag:   tag,
		numHunks:  hunkCount(storedFrames(tracks)),
	}
}

// Write creates a CDZLIB-compressed CHD v5 file. The output appears only once
// complete, so cancelling ctx leaves any existing file untouched.
func Write(ctx context.Context, outputPath string, tracks []Track, progress io.Writer) error {
	if progress == nil {
		progress = io.Discard
	}

	// The map precedes the hunks but needs every compressed length, so the hunks go
	// to a temp file first.
	tmp, err := os.CreateTemp("", "swirl-chd-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	l := planLayout(tracks)

	records, rawSHA1, err := compressHunks(ctx, tmp, l, tracks, progress)
	if err != nil {
		return err
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek temp file: %w", err)
	}

	return assemble(outputPath, tmp, l, records, rawSHA1)
}

// Returns the map records plus the SHA1 of the uncompressed data.
func compressHunks(
	ctx context.Context, tmp io.Writer, l layout, tracks []Track, progress io.Writer,
) ([]hunkRecord, []byte, error) {
	records := make([]hunkRecord, l.numHunks)
	rawHash := sha1.New() //nolint:gosec // SHA1 is required by the CHD v5 format
	stream := &hunkStream{tracks: tracks}
	seen := make(map[[sha1.Size]byte]int, l.numHunks)

	workers := runtime.NumCPU()
	batch := make([][]byte, 0, workers)
	batchStart := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}

		if err := compressBatch(tmp, records, batchStart, batch, seen); err != nil {
			return err
		}

		batchStart += len(batch)
		batch = batch[:0]

		return nil
	}

	for range l.numHunks {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("compress hunk %d: %w", batchStart+len(batch), err)
		}

		buf, err := stream.next()
		if err != nil {
			return nil, nil, err
		}

		rawHash.Write(buf)
		progress.Write(buf) //nolint:errcheck // progress reporting never affects the result
		batch = append(batch, buf)

		if len(batch) == workers {
			if err := flush(); err != nil {
				return nil, nil, err
			}
		}
	}

	if err := flush(); err != nil {
		return nil, nil, err
	}

	return records, rawHash.Sum(nil), nil
}

// Lays tracks out as fixed-size hunks, zero-filling the slots past each track's data.
type hunkStream struct {
	tracks       []Track
	trackIdx     int
	frameInTrack int
}

func (s *hunkStream) next() ([]byte, error) {
	buf := make([]byte, hunkBytes)

	for slot := range framesPerHunk {
		for s.trackIdx < len(s.tracks) && s.frameInTrack >= effectiveFrames(s.tracks[s.trackIdx]) {
			s.frameInTrack -= effectiveFrames(s.tracks[s.trackIdx])
			s.trackIdx++
		}

		if s.trackIdx < len(s.tracks) && s.frameInTrack < s.tracks[s.trackIdx].Frames {
			off := slot * slotBytes
			if _, err := io.ReadFull(s.tracks[s.trackIdx].Data, buf[off:off+sectorBytes]); err != nil {
				return nil, fmt.Errorf("track %d frame %d: %w", s.tracks[s.trackIdx].Number, s.frameInTrack, err)
			}

			if disc.IsAudio(s.tracks[s.trackIdx].Type) {
				swapAudio(buf[off : off+sectorBytes])
			}
		}

		s.frameInTrack++
	}

	return buf, nil
}

// Compresses in parallel, writes in stream order, so layout never depends on timing.
func compressBatch(w io.Writer, records []hunkRecord, start int, batch [][]byte, seen map[[sha1.Size]byte]int) error {
	type result struct {
		stored   []byte
		digest   [sha1.Size]byte
		compType uint8
		rawCRC   uint16
		err      error
	}

	results := make([]result, len(batch))

	var wg sync.WaitGroup

	for i, raw := range batch {
		wg.Go(func() {
			compressed, err := compressCDZLIB(raw)
			if err != nil {
				results[i].err = err

				return
			}

			results[i] = result{
				stored:   compressed,
				digest:   sha1.Sum(raw), //nolint:gosec // G401: names a hunk's content, nothing security rests on it
				compType: mapCompType0,
				rawCRC:   crc16CCITT(raw),
			}
			if len(compressed) >= len(raw) {
				results[i].stored, results[i].compType = raw, mapCompNone
			}
		})
	}

	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			return fmt.Errorf("compress hunk %d: %w", start+i, r.err)
		}

		if src, dup := seen[r.digest]; dup {
			records[start+i] = hunkRecord{compType: mapCompSelf, selfHunk: src}

			continue
		}

		seen[r.digest] = start + i

		if _, err := w.Write(r.stored); err != nil {
			return fmt.Errorf("write hunk %d to temp: %w", start+i, err)
		}

		records[start+i] = hunkRecord{
			compType: r.compType,
			length:   uint32(len(r.stored)), //nolint:gosec // G115: compressed size always fits in uint32
			rawCRC:   r.rawCRC,
		}
	}

	return nil
}

// The two SHA1 fields are patched in last, because the combined digest covers the
// metadata too. Staged beside the destination and renamed in, so an interrupted write
// leaves nothing that still answers to the CHD magic.
func assemble(outputPath string, hunks io.Reader, l layout, records []hunkRecord, rawSHA1 []byte) error {
	metaStart := int64(headerSize)

	var metaLen int64
	for _, text := range l.metaTexts {
		metaLen += metaEntryHeaderBytes + int64(len(text))
	}

	mapOffset := metaStart + metaLen

	types, selfBits := promoteSelf(records)

	mapData, err := encodeMap(records, types, selfBits)
	if err != nil {
		return err
	}

	mapLen := int64(mapHeaderBytes + len(mapData))
	dataStart := alignUp(mapOffset+mapLen, hunkBytes)
	firstOffset := uint64(dataStart) //nolint:gosec // G115: sizes and file offsets are always non-negative

	f, err := os.CreateTemp(filepath.Dir(outputPath), "."+filepath.Base(outputPath)+".swirl-*")
	if err != nil {
		return fmt.Errorf("create %s: %w", outputPath, err)
	}

	staged := f.Name()

	// No-ops once the rename succeeds; the happy path closes explicitly to catch a flush error.
	defer os.Remove(staged)
	defer f.Close()

	sections := []struct {
		name string
		data []byte
	}{
		{"header", makeHeader(int64(l.numHunks)*hunkBytes, mapOffset, metaStart)},
		{"metadata", buildMetaBytes(l.metaTexts, metaStart, l.metaTag)},
		{"map header", buildMapHeader(mapData, firstOffset, records, selfBits)},
		{"map", mapData},
		{"padding", make([]byte, dataStart-(mapOffset+mapLen))},
	}

	for _, s := range sections {
		if _, err := f.Write(s.data); err != nil {
			return fmt.Errorf("write %s: %w", s.name, err)
		}
	}

	if _, err := io.Copy(f, hunks); err != nil {
		return fmt.Errorf("copy hunk data: %w", err)
	}

	if _, err := f.WriteAt(rawSHA1, headerRawSHA1Offset); err != nil {
		return fmt.Errorf("write raw SHA1: %w", err)
	}

	if _, err := f.WriteAt(combinedSHA1(rawSHA1, writeTuples(l)), headerCombinedSHA1Offset); err != nil {
		return fmt.Errorf("write combined SHA1: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", outputPath, err)
	}

	// CreateTemp opens at 0600; os.Create would have used the process umask.
	if err := os.Chmod(staged, outputFileMode); err != nil {
		return fmt.Errorf("set mode on %s: %w", outputPath, err)
	}

	if err := os.Rename(staged, outputPath); err != nil {
		return fmt.Errorf("move %s into place: %w", outputPath, err)
	}

	return nil
}

// SHA1(rawSHA1 || sorted(tuples...)); sorting frees the digest from the order the
// metadata happens to be stored in. Only `chdman verify` checks it, never libchdr.
// https://github.com/mamedev/mame/blob/33c42e9e0e89c879e0fc5b654cc70b947bf1473c/src/lib/util/chd.cpp#L1839
func combinedSHA1(rawSHA1 []byte, tuples [][]byte) []byte {
	slices.SortFunc(tuples, bytes.Compare)

	combined := sha1.New() //nolint:gosec // SHA1 is required by the CHD v5 format
	combined.Write(rawSHA1)

	for _, t := range tuples {
		combined.Write(t)
	}

	return combined.Sum(nil)
}

// One entry's contribution to the combined digest: 4-byte tag, then SHA1(payload).
func metaTuple(tag uint32, payload []byte) []byte {
	b := make([]byte, tagBytes, tagBytes+sha1.Size)
	binary.BigEndian.PutUint32(b, tag)

	h := sha1.Sum(payload) //nolint:gosec // SHA1 is required by the CHD v5 format

	return append(b, h[:]...)
}

// Every entry a writer emits carries the same tag and CHD_MDFLAGS_CHECKSUM.
func writeTuples(l layout) [][]byte {
	tuples := make([][]byte, 0, len(l.metaTexts))
	for _, text := range l.metaTexts {
		tuples = append(tuples, metaTuple(l.metaTag, text))
	}

	return tuples
}

// CDZLIB wire format: an ECC bitmap of ceil(frames/8) bytes (always 0, we never
// strip), the 2-byte big-endian length of the deflated sector block, then
// DEFLATE(sectors) and DEFLATE(subcode). The subcode block must be present even for
// Dreamcast discs that carry none: libchdr builds WANT_SUBCODE=1 and fails without it.
// https://github.com/rtissera/libchdr/blob/5f82799f2c8cad1e9cd26d39a0f8d36369a5534b/include/libchdr/chdconfig.h#L6
// https://github.com/rtissera/libchdr/blob/5f82799f2c8cad1e9cd26d39a0f8d36369a5534b/src/libchdr_chd.c#L818
func compressCDZLIB(hunkData []byte) ([]byte, error) {
	frames := len(hunkData) / slotBytes

	// De-interleave: sector and subcode regions compress as separate blocks.
	sectorData := make([]byte, frames*sectorBytes)
	subcodeData := make([]byte, frames*subcodeBytes)

	for i := range frames {
		copy(sectorData[i*sectorBytes:], hunkData[i*slotBytes:i*slotBytes+sectorBytes])
		copy(subcodeData[i*subcodeBytes:], hunkData[i*slotBytes+sectorBytes:i*slotBytes+slotBytes])
	}

	baseCmp, err := zlibCompress(sectorData)
	if err != nil {
		return nil, err
	}

	subcodeCmp, err := zlibCompress(subcodeData)
	if err != nil {
		return nil, err
	}

	eccBytes := (frames + bitsPerByte - 1) / bitsPerByte // = 1 for framesPerHunk=8
	out := make([]byte, eccBytes+2+len(baseCmp)+len(subcodeCmp))
	out[eccBytes] = byte(len(baseCmp) >> bitsPerByte) //nolint:gosec // G115: upper byte of 16-bit length
	out[eccBytes+1] = byte(len(baseCmp))              //nolint:gosec // G115: lower byte of 16-bit length
	copy(out[eccBytes+2:], baseCmp)
	copy(out[eccBytes+2+len(baseCmp):], subcodeCmp)

	return out, nil
}

// flate.NewWriter allocates a fresh window and hash table, twice per hunk, about a
// megabyte of garbage. Pooled rather than shared because compressBatch is concurrent.
//
//nolint:gochecknoglobals // pool of reusable encoders, no observable state
var flateWriters sync.Pool

func zlibCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer

	w, pooled := flateWriters.Get().(*flate.Writer)
	if pooled {
		w.Reset(&buf)
	} else {
		var err error
		if w, err = flate.NewWriter(&buf, flate.DefaultCompression); err != nil {
			return nil, fmt.Errorf("flate init: %w", err)
		}
	}

	defer flateWriters.Put(w)

	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("flate write: %w", err)
	}

	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("flate close: %w", err)
	}

	return buf.Bytes(), nil
}

// The tree the map's type stream is coded against, as 16 four-bit code lengths.
func mapCodeLengths() []int {
	lengths := make([]int, huffmanNumSymbols)
	lengths[mapCompType0] = mapCommonCodeBits

	for _, sym := range []int{mapCompNone, mapCompSelf, mapCompSelf0, mapCompSelf1} {
		lengths[sym] = mapOtherCodeBits
	}

	return lengths
}

// Promotes each self-reference to the symbol MAME's compress_v5_map would pick and
// returns the width the remaining spelled-out targets need; diverging fails chdman verify.
// https://github.com/mamedev/mame/blob/33c42e9e0e89c879e0fc5b654cc70b947bf1473c/src/lib/util/chd.cpp#L2239-L2250
func promoteSelf(records []hunkRecord) ([]uint8, byte) {
	types := make([]uint8, len(records))
	lastSelf, maxSelf := 0, 0

	for i, r := range records {
		types[i] = r.compType
		if r.compType != mapCompSelf {
			continue
		}

		switch r.selfHunk {
		case lastSelf:
			types[i] = mapCompSelf0
		case lastSelf + 1:
			types[i] = mapCompSelf1
		default:
			maxSelf = max(maxSelf, r.selfHunk)
		}

		lastSelf = r.selfHunk
	}

	selfBits := byte(0)
	for v := maxSelf; v > 0; v >>= 1 {
		selfBits++
	}

	return types, selfBits
}

// The Huffman+RLE map data, without its 16-byte header.
func encodeMap(records []hunkRecord, types []uint8, selfBits byte) ([]byte, error) {
	lengths := mapCodeLengths()

	codes, err := buildCanonicalCodes(lengths)
	if err != nil {
		return nil, err
	}

	bySym := make(map[int]huffmanCodeEntry, len(codes))
	for _, c := range codes {
		bySym[c.sym] = c
	}

	bw := &bitWriter{}

	// Tree RLE, escape=1: a literal bit-length of 1 is "1 1", anything else is itself.
	for _, l := range lengths {
		if l == 1 {
			bw.write(1, huffmanRLEBitWidth)
		}

		bw.write(uint32(l), huffmanRLEBitWidth) //nolint:gosec // G115: a code length is under 16
	}

	for _, t := range types {
		c := bySym[int(t)]
		bw.write(c.code, c.length)
	}

	for i, t := range types {
		switch t {
		case mapCompType0:
			bw.write(records[i].length, lengthBits)
			bw.write(uint32(records[i].rawCRC), mapCRCBits)
		case mapCompNone:
			bw.write(uint32(records[i].rawCRC), mapCRCBits)
		case mapCompSelf:
			bw.write(uint32(records[i].selfHunk), int(selfBits)) //nolint:gosec // G115: a hunk index is non-negative
		}
	}

	return bw.buf, nil
}

func buildMapHeader(mapData []byte, firstOffset uint64, records []hunkRecord, selfBits byte) []byte {
	entryData := make([]byte, len(records)*mapEntryBytes)
	curOffset := firstOffset

	for i, r := range records {
		off := i * mapEntryBytes
		entryData[off] = r.compType

		// The CRC below is taken over references normalized back to plain SELF: the target
		// hunk where the offset goes, no length, no CRC.
		if r.compType == mapCompSelf {
			putUint48BE(entryData[off+4:], uint64(r.selfHunk)) //nolint:gosec // G115: a hunk index is non-negative

			continue
		}

		putUint24BE(entryData[off+1:], r.length)
		putUint48BE(entryData[off+4:], curOffset)
		binary.BigEndian.PutUint16(entryData[off+10:], r.rawCRC)
		curOffset += uint64(r.length)
	}

	h := make([]byte, mapHeaderBytes)
	binary.BigEndian.PutUint32(h[0:], uint32(len(mapData))) //nolint:gosec // G115: map size fits in uint32
	putUint48BE(h[4:], firstOffset)
	binary.BigEndian.PutUint16(h[10:], crc16CCITT(entryData))
	h[12] = lengthBits // bits used to encode compressed lengths
	h[13] = selfBits
	// h[14] parentBits = 0, h[15] reserved = 0

	return h
}

func putUint24BE(b []byte, v uint32) {
	var tmp [4]byte

	binary.BigEndian.PutUint32(tmp[:], v)
	copy(b, tmp[1:])
}

func putUint48BE(b []byte, v uint64) {
	var tmp [8]byte

	binary.BigEndian.PutUint64(tmp[:], v)
	copy(b, tmp[2:])
}

// CRC-16/CCITT, polynomial 0x1021, init 0xFFFF. A CRC runs over every hunk on both
// paths, and the table form measures 7.7x the bit-by-bit loop it replaces.
//
//nolint:gochecknoglobals // immutable lookup table, built once
var crcTable = func() [256]uint16 {
	var t [256]uint16

	for i := range t {
		c := uint16(i) << bitsPerByte

		for range bitsPerByte {
			if c&crcHighBit != 0 {
				c = c<<1 ^ crcPolynomial
			} else {
				c <<= 1
			}
		}

		t[i] = c
	}

	return t
}()

func crc16CCITT(data []byte) uint16 {
	crc := uint16(crcInitValue)
	for _, b := range data {
		crc = crc<<bitsPerByte ^ crcTable[byte(crc>>bitsPerByte)^b]
	}

	return crc
}

// MSB-first, the convention bitReader in read.go expects.
type bitWriter struct {
	buf    []byte
	bitPos int
}

func (bw *bitWriter) write(value uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		bit := (value >> i) & 1
		byteIdx := bw.bitPos / bitsPerByte
		bitIdx := bitWriterMSBIndex - (bw.bitPos % bitsPerByte)

		for byteIdx >= len(bw.buf) {
			bw.buf = append(bw.buf, 0)
		}

		bw.buf[byteIdx] |= byte(bit) << bitIdx
		bw.bitPos++
	}
}

// CHD stores track frame counts at trackPadding alignment; Flycast advances by it.
// https://github.com/flyinghead/flycast/blob/e2722869ffb6f404d2056a12653aa67e4210d61d/core/imgread/chd.cpp#L198-L200
func PadFrames(n int) int {
	return (n + trackPadding - 1) / trackPadding * trackPadding
}

func alignUp(n, align int64) int64 {
	return (n + align - 1) / align * align
}

func chdTrackType(cueType string) string {
	switch strings.ToUpper(cueType) {
	case disc.TrackTypeMode2:
		return chdTypeMode2Raw
	case disc.TrackTypeAudio:
		return disc.TrackTypeAudio
	default:
		return chdTypeMode1Raw
	}
}

// Flycast parses PGTYPE but never acts on it; it matters only because the payload is
// hashed verbatim into the combined SHA1, so matching chdman keeps its verify passing.
func pregapType(cueType string) string {
	if disc.IsAudio(cueType) {
		return disc.TrackTypeAudio
	}

	return "MODE1"
}

// GD-ROM is the only case that sets StoredFrames for bridge-track padding.
func isGDROM(tracks []Track) bool {
	for _, t := range tracks {
		if t.StoredFrames > 0 {
			return true
		}
	}

	return false
}

// CHGD puts the allocated count in FRAMES and the padding in PAD; Flycast derives
// EndFAD = StartFAD + FRAMES - 1 - PAD. CHT2 has no PAD, so its FRAMES is the real count,
// as chdman writes it, or readers take the padding as track data.
// https://github.com/rtissera/libchdr/blob/5f82799f2c8cad1e9cd26d39a0f8d36369a5534b/include/libchdr/chd.h#L249
// https://github.com/flyinghead/flycast/blob/e2722869ffb6f404d2056a12653aa67e4210d61d/core/imgread/chd.cpp#L188-L190
func buildMetaTexts(tracks []Track) [][]byte {
	gdrom := isGDROM(tracks)
	out := make([][]byte, len(tracks))

	for i, t := range tracks {
		ef := effectiveFrames(t)

		var s string
		if gdrom {
			s = fmt.Sprintf(
				"TRACK:%d TYPE:%s SUBTYPE:NONE FRAMES:%d PAD:%d PREGAP:0 PGTYPE:%s PGSUB:NONE POSTGAP:0",
				t.Number, chdTrackType(t.Type), ef, ef-t.Frames, pregapType(t.Type),
			)
		} else {
			s = fmt.Sprintf(
				"TRACK:%d TYPE:%s SUBTYPE:NONE FRAMES:%d PREGAP:0 PGTYPE:%s PGSUB:NONE POSTGAP:0",
				t.Number, chdTrackType(t.Type), t.Frames, pregapType(t.Type),
			)
		}

		out[i] = append([]byte(s), 0) // null-terminated, as chdman stores it
	}

	return out
}

// Metadata is a linked list after the file header: a 16-byte entry header (tag,
// flags, uint24 payload length, uint64 offset of the next entry or 0), then payload.
// https://github.com/mamedev/mame/blob/33c42e9e0e89c879e0fc5b654cc70b947bf1473c/src/lib/util/chd.cpp#L1724-L1728
func buildMetaBytes(metaTexts [][]byte, metaStart int64, tag uint32) []byte {
	offsets := make([]int64, len(metaTexts))

	off := metaStart
	for i, text := range metaTexts {
		offsets[i] = off
		off += metaEntryHeaderBytes + int64(len(text))
	}

	var buf []byte

	for i, text := range metaTexts {
		var nextOff uint64
		if i < len(metaTexts)-1 {
			nextOff = uint64(offsets[i+1]) //nolint:gosec // G115: sizes and file offsets are always non-negative
		}

		chunk := make([]byte, metaEntryHeaderBytes+len(text))
		binary.BigEndian.PutUint32(chunk[0:], tag)
		chunk[4] = metaFlags
		putUint24BE(chunk[5:], uint32(len(text))) //nolint:gosec // G115: metadata length fits in 24 bits
		binary.BigEndian.PutUint64(chunk[8:], nextOff)
		copy(chunk[metaEntryHeaderBytes:], text)
		buf = append(buf, chunk...)
	}

	return buf
}

// The 124-byte CHD v5 header. The SHA1 fields at 0x40 and 0x54 stay zero until the
// hunks are written; parentsha1 at 0x68 stays zero, we never write parent chains.
// https://github.com/mamedev/mame/blob/33c42e9e0e89c879e0fc5b654cc70b947bf1473c/src/lib/util/chd.cpp#L2606-L2624
func makeHeader(totalLogical, mapOffset, metaOffset int64) []byte {
	h := make([]byte, headerSize)
	copy(h[0x00:], headerMagic)
	binary.BigEndian.PutUint32(h[0x08:], headerSize)
	binary.BigEndian.PutUint32(h[0x0C:], chdVersion)
	binary.BigEndian.PutUint32(h[headerCodecOffset:], codecCDZlib) // compressors[0]

	// compressors[1..3] = 0 (only one codec needed)
	//nolint:gosec // G115: sizes and file offsets are always non-negative
	binary.BigEndian.PutUint64(h[0x20:], uint64(totalLogical))
	//nolint:gosec // G115: sizes and file offsets are always non-negative
	binary.BigEndian.PutUint64(h[0x28:], uint64(mapOffset))
	//nolint:gosec // G115: sizes and file offsets are always non-negative
	binary.BigEndian.PutUint64(h[0x30:], uint64(metaOffset))
	binary.BigEndian.PutUint32(h[0x38:], hunkBytes)
	binary.BigEndian.PutUint32(h[0x3C:], slotBytes)

	return h
}
