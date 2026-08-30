package chd

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha1" //nolint:gosec // SHA1 is required by the CHD v5 format
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ilyalavrenov/swirl/internal/disc"
)

const (
	headerMagicLen = 8

	huffmanNumSymbols = 16

	// Tree run lengths are stored as N-3.
	rleCountBias = 3

	// Without this bound a crafted header sizes an allocation the file never backs.
	mapMaxLengthBits = 32

	// The longest code the map's 4-bit length fields can express.
	maxHuffmanBits = 15

	// chdman points a self-reference at a hunk holding real data; a longer chain is a cycle.
	maxSelfChain = 32

	// Run counts are biased past what spelling the types out costs; a large one arrives as two symbols, high nibble first.
	rleSmallBias  = 2
	rleLargeBias  = 18
	rleLargeShift = 4

	// Trim a wider big-endian read to CHD's packed 24- and 48-bit fields.
	mask24 = 0xFFFFFF
	mask48 = 0xFFFFFFFFFFFF
)

// Raw DEFLATE has no checksum: damage can decode cleanly into the wrong bytes, which no decoder error would report.
var ErrCRCMismatch = errors.New("failed its stored CRC")

type TrackInfo struct {
	Number      int
	CUEType     string // "MODE1/2352", "AUDIO", etc.
	TotalFrames int    // CHD sector slots allocated to this track (includes PAD)
	RealFrames  int    // frames with actual data (TotalFrames - PAD)

	// GDROM means the track came from a CHGD entry, so the HDA boundary applies.
	GDROM bool
}

// size bounds what the rest of the header is allowed to claim.
type fileHeader struct {
	size         int64
	logicalBytes int64
	mapOffset    int64
	metaOffset   int64
	hunkBytes    int
	numHunks     int
	version      uint32
	codec        uint32
	rawSHA1      []byte // as stored; Verify compares its own recomputation
	combinedSHA1 []byte
}

// Only CDZLIB is accepted; an unknown codec decodes to plausible garbage. The rest is
// checked against the file size: no divide by zero, no allocation the file cannot fill.
func readHeader(f *os.File, path string) (fileHeader, error) {
	info, err := f.Stat()
	if err != nil {
		return fileHeader{}, fmt.Errorf("CHD: %w", err)
	}

	hdr := make([]byte, headerSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return fileHeader{}, fmt.Errorf("read header of %s: %w", path, err)
	}

	if string(hdr[0:headerMagicLen]) != headerMagic {
		return fileHeader{}, fmt.Errorf("%s: not a CHD file", path)
	}

	version := binary.BigEndian.Uint32(hdr[0x0C:])
	if version != chdVersion {
		return fileHeader{}, fmt.Errorf("%s: CHD version %d is not supported, want %d", path, version, chdVersion)
	}

	codec := binary.BigEndian.Uint32(hdr[0x10:])
	if codec != codecCDZlib {
		return fileHeader{}, fmt.Errorf("%s: compressor %q, want cdzl", path, tagString(codec))
	}

	//nolint:gosec // G115: the values are range-checked immediately below
	h := fileHeader{
		size:         info.Size(),
		logicalBytes: int64(binary.BigEndian.Uint64(hdr[0x20:])),
		mapOffset:    int64(binary.BigEndian.Uint64(hdr[0x28:])),
		metaOffset:   int64(binary.BigEndian.Uint64(hdr[0x30:])),
		hunkBytes:    int(binary.BigEndian.Uint32(hdr[0x38:])),
		version:      version,
		codec:        codec,
		rawSHA1:      bytes.Clone(hdr[headerRawSHA1Offset : headerRawSHA1Offset+sha1.Size]),
		combinedSHA1: bytes.Clone(hdr[headerCombinedSHA1Offset : headerCombinedSHA1Offset+sha1.Size]),
	}

	switch {
	case h.hunkBytes <= 0 || h.hunkBytes%slotBytes != 0:
		return fileHeader{}, fmt.Errorf("%s: hunk size %d is not a whole number of %d byte sectors",
			path, h.hunkBytes, slotBytes)
	case h.logicalBytes < 0:
		return fileHeader{}, fmt.Errorf("%s: logical size does not fit in an int64", path)
	case h.mapOffset < 0 || h.mapOffset >= h.size:
		return fileHeader{}, fmt.Errorf("%s: map offset %d is outside the file", path, h.mapOffset)
	case h.metaOffset < 0 || h.metaOffset >= h.size:
		return fileHeader{}, fmt.Errorf("%s: metadata offset %d is outside the file", path, h.metaOffset)
	}

	// The hunk count rounds up: a trailing part-used hunk still counts, and flooring
	// would shift every later read. Every hunk stores a byte, so file size caps it.
	h.numHunks = int((h.logicalBytes + int64(h.hunkBytes) - 1) / int64(h.hunkBytes))
	if int64(h.numHunks) > h.size {
		return fileHeader{}, fmt.Errorf("%s: header claims %d hunks in a %d byte file", path, h.numHunks, h.size)
	}

	return h, nil
}

// Sizes a progress indicator before Read, from the header alone.
func LogicalBytes(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("CHD: %w", err)
	}
	defer f.Close()

	h, err := readHeader(f, path)
	if err != nil {
		return 0, err
	}

	return h.logicalBytes, nil
}

// Read writes each track's raw sector data into the already-existing outputDir and
// returns the metadata a CUE sheet needs. A nil progress is fine.
func Read(ctx context.Context, inputPath, outputDir string, progress io.Writer) ([]TrackInfo, error) {
	if progress == nil {
		progress = io.Discard
	}

	c, err := openCHD(inputPath)
	if err != nil {
		return nil, err
	}
	defer c.close()

	outFiles := make([]*os.File, len(c.tracks))

	// Covers the error returns only; the happy path closes explicitly to catch a flush error.
	defer func() {
		for _, of := range outFiles {
			if of != nil {
				of.Close()
			}
		}
	}()

	for i, t := range c.tracks {
		name := disc.TrackFileName(t.Number, t.CUEType)

		out, createErr := os.OpenFile(filepath.Join(outputDir, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, outputFileMode)
		if createErr != nil {
			return nil, fmt.Errorf("track %d: %w", t.Number, createErr)
		}

		outFiles[i] = out
	}

	writer := newTrackWriter(c.tracks, outFiles)
	if err := extractHunks(ctx, c.f, c.records, writer, c.header.hunkBytes, progress); err != nil {
		return nil, fmt.Errorf("%s: %w", inputPath, err)
	}

	for i, of := range outFiles {
		if closeErr := of.Close(); closeErr != nil {
			return nil, fmt.Errorf("close track %d: %w", c.tracks[i].Number, closeErr)
		}

		outFiles[i] = nil
	}

	return c.tracks, nil
}

// Routes each hunk's sectors to the right track file, dropping trailing PAD frames.
type trackWriter struct {
	tracks   []TrackInfo
	files    []*os.File
	idx      int
	realLeft int
	padLeft  int
}

func newTrackWriter(tracks []TrackInfo, files []*os.File) *trackWriter {
	return &trackWriter{
		tracks:   tracks,
		files:    files,
		realLeft: tracks[0].RealFrames,
		padLeft:  tracks[0].TotalFrames - tracks[0].RealFrames,
	}
}

// Reports false once every track is full.
func (w *trackWriter) writeSector(sector []byte) (bool, error) {
	for w.idx < len(w.tracks) && w.realLeft == 0 && w.padLeft == 0 {
		w.idx++
		if w.idx < len(w.tracks) {
			w.realLeft = w.tracks[w.idx].RealFrames
			w.padLeft = w.tracks[w.idx].TotalFrames - w.tracks[w.idx].RealFrames
		}
	}

	if w.idx >= len(w.tracks) {
		return false, nil
	}

	if w.realLeft == 0 {
		w.padLeft--

		return true, nil
	}

	if disc.IsAudio(w.tracks[w.idx].CUEType) {
		swapAudio(sector)
	}

	if _, err := w.files[w.idx].Write(sector); err != nil {
		return false, fmt.Errorf("write track %d: %w", w.tracks[w.idx].Number, err)
	}

	w.realLeft--

	return true, nil
}

// Hunks past the last track are still CRC-checked, so trailing data cannot hide corruption.
func extractHunks(
	ctx context.Context,
	f *os.File,
	records []mapReadRecord,
	w *trackWriter,
	storedHunkBytes int,
	progress io.Writer,
) error {
	framesInHunk := storedHunkBytes / slotBytes

	for hunkIdx := range records {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("read: %w", err)
		}

		raw, err := readHunk(f, records, hunkIdx, storedHunkBytes)
		if err != nil {
			return fmt.Errorf("hunk %d: %w", hunkIdx, err)
		}

		progress.Write(raw) //nolint:errcheck // progress reporting never affects the result

		for fi := range framesInHunk {
			more, wErr := w.writeSector(raw[fi*slotBytes : fi*slotBytes+sectorBytes])
			if wErr != nil {
				return wErr
			}

			if !more {
				break
			}
		}
	}

	return nil
}

type rawMeta struct {
	tag     uint32
	flags   byte
	payload []byte
}

func readMetaChain(f *os.File, h fileHeader) ([]rawMeta, error) {
	var metas []rawMeta

	// An entry can point back at itself; cap the walk at what the file can hold.
	maxEntries := h.size / metaEntryHeaderBytes
	offset := h.metaOffset

	for offset != 0 {
		if int64(len(metas)) >= maxEntries {
			return nil, errors.New("metadata chain is cyclic or longer than the file")
		}

		hdr := make([]byte, metaEntryHeaderBytes)
		if _, err := f.ReadAt(hdr, offset); err != nil {
			return nil, fmt.Errorf("read meta header at %d: %w", offset, err)
		}

		tag := binary.BigEndian.Uint32(hdr[0:])
		// Flags at byte 4, then a 24-bit big-endian payload length.
		flags := hdr[4]
		payLen := binary.BigEndian.Uint32(hdr[4:]) & mask24
		next := int64(binary.BigEndian.Uint64(hdr[8:])) //nolint:gosec // G115: sizes and file offsets are always non-negative

		payload := make([]byte, payLen)
		if _, err := f.ReadAt(payload, offset+int64(metaEntryHeaderBytes)); err != nil {
			return nil, fmt.Errorf("read meta payload at %d: %w", offset, err)
		}

		metas = append(metas, rawMeta{tag: tag, flags: flags, payload: payload})
		offset = next
	}

	return metas, nil
}

func parseTrackMetas(metas []rawMeta) ([]TrackInfo, error) {
	var tracks []TrackInfo

	for _, m := range metas {
		if m.tag != tagCHT2 && m.tag != tagCHGD {
			continue
		}

		// Null-terminated ASCII: "TRACK:1 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:452 PAD:0 ...".
		text := strings.TrimRight(string(m.payload), "\x00")
		kv := parseKV(text)

		num, err := strconv.Atoi(kv["TRACK"])
		if err != nil {
			return nil, fmt.Errorf("bad TRACK in %q: %w", text, err)
		}

		frames, err := strconv.Atoi(kv["FRAMES"])
		if err != nil {
			return nil, fmt.Errorf("bad FRAMES in %q: %w", text, err)
		}

		pad := 0

		if s, ok := kv["PAD"]; ok {
			pad, err = strconv.Atoi(s)
			if err != nil {
				return nil, fmt.Errorf("bad PAD in %q: %w", text, err)
			}
		}

		tracks = append(tracks, TrackInfo{
			Number:      num,
			CUEType:     chdTypeToCUE(kv["TYPE"]),
			TotalFrames: frames,
			RealFrames:  frames - pad,
			GDROM:       m.tag == tagCHGD,
		})
	}

	return tracks, nil
}

func parseKV(s string) map[string]string {
	m := make(map[string]string)

	for field := range strings.FieldsSeq(s) {
		if key, value, ok := strings.Cut(field, ":"); ok {
			m[key] = value
		}
	}

	return m
}

func chdTypeToCUE(t string) string {
	switch t {
	case chdTypeMode2Raw:
		return disc.TrackTypeMode2
	case disc.TrackTypeAudio:
		return disc.TrackTypeAudio
	default:
		return disc.TrackTypeMode1
	}
}

func tagString(tag uint32) string {
	var b [tagBytes]byte

	binary.BigEndian.PutUint32(b[:], tag)

	return string(b[:])
}

// The last six bytes of b[0:8]: CHD's file-offset width.
func uint48BE(b []byte) int64 {
	return int64(binary.BigEndian.Uint64(b) & mask48)
}

type mapReadRecord struct {
	compType uint8
	length   uint32
	offset   int64
	crc      uint16 // CRC16-CCITT of the decompressed hunk

	// The hunk this one duplicates, or -1. chdman deduplicates identical hunks.
	selfHunk int
}

func readCompressedMap(f *os.File, h fileHeader) ([]mapReadRecord, error) {
	mh := make([]byte, mapHeaderBytes)
	if _, err := f.ReadAt(mh, h.mapOffset); err != nil {
		return nil, fmt.Errorf("read map header: %w", err)
	}

	compDataLen := int64(binary.BigEndian.Uint32(mh[0:]))
	if h.mapOffset+mapHeaderBytes+compDataLen > h.size {
		return nil, fmt.Errorf("map data of %d bytes overruns the file", compDataLen)
	}

	// 48-bit big-endian absolute offset of hunk 0, at bytes 4-9.
	firstOffset := uint48BE(mh[2:])

	lengthBits, selfBits := int(mh[12]), int(mh[13])
	if lengthBits > mapMaxLengthBits || selfBits > mapMaxLengthBits {
		return nil, fmt.Errorf("map declares %d length bits and %d self bits, both must fit a uint32", lengthBits, selfBits)
	}

	mapData := make([]byte, compDataLen)
	if _, err := f.ReadAt(mapData, h.mapOffset+int64(mapHeaderBytes)); err != nil {
		return nil, fmt.Errorf("read map data: %w", err)
	}

	br := &bitReader{buf: mapData}

	symLen, err := readHuffmanTree(br)
	if err != nil {
		return nil, err
	}

	codes, err := buildCanonicalCodes(symLen)
	if err != nil {
		return nil, err
	}

	records := make([]mapReadRecord, h.numHunks)

	if err := readMapTypes(br, codes, records); err != nil {
		return nil, err
	}

	if err := readMapEntries(br, records, h, firstOffset, lengthBits, selfBits); err != nil {
		return nil, err
	}

	return records, nil
}

// The type stream RLE-encodes repeats: RLE_SMALL carries a one-symbol count, RLE_LARGE two.
// https://github.com/rtissera/libchdr/blob/5f82799f2c8cad1e9cd26d39a0f8d36369a5534b/src/libchdr_chd.c#L1622
func readMapTypes(br *bitReader, codes []huffmanCodeEntry, records []mapReadRecord) error {
	last, repeat := uint8(0), 0

	for idx := range records {
		if repeat > 0 {
			repeat--
			records[idx].compType = last

			continue
		}

		sym, err := decodeHuffmanSym(br, codes)
		if err != nil {
			return fmt.Errorf("map sym %d: %w", idx, err)
		}

		if sym == mapCompRLESmall || sym == mapCompRLELarge {
			if repeat, err = readRunLength(br, codes, sym); err != nil {
				return fmt.Errorf("map run at hunk %d: %w", idx, err)
			}
		} else {
			last = uint8(sym) //nolint:gosec // G115: sym is always a small compression type code
		}

		records[idx].compType = last
	}

	return nil
}

func readRunLength(br *bitReader, codes []huffmanCodeEntry, sym int) (int, error) {
	high, err := decodeHuffmanSym(br, codes)
	if err != nil {
		return 0, err
	}

	if sym == mapCompRLESmall {
		return rleSmallBias + high, nil
	}

	low, err := decodeHuffmanSym(br, codes)
	if err != nil {
		return 0, err
	}

	return rleLargeBias + (high << rleLargeShift) + low, nil
}

// A second pass for the payload: length and CRC for compressed and stored hunks, a hunk number for a self-reference.
func readMapEntries(
	br *bitReader, records []mapReadRecord, h fileHeader, firstOffset int64, lengthBits, selfBits int,
) error {
	offset := firstOffset
	lastSelf := 0

	for idx := range records {
		rec := &records[idx]
		rec.selfHunk = -1

		switch rec.compType {
		case mapCompType0, mapCompNone:
			length := uint32(h.hunkBytes) //nolint:gosec // G115: hunkBytes fits in uint32

			if rec.compType == mapCompType0 {
				l, ok := br.readBits(lengthBits)
				if !ok {
					return fmt.Errorf("truncated map: length for hunk %d", idx)
				}

				length = l
			}

			crc, ok := br.readBits(mapCRCBits)
			if !ok {
				return fmt.Errorf("truncated map: CRC for hunk %d", idx)
			}

			rec.length, rec.offset = length, offset
			rec.crc = uint16(crc) //nolint:gosec // G115: the field is exactly mapCRCBits wide
			offset += int64(length)

		case mapCompSelf:
			v, ok := br.readBits(selfBits)
			if !ok {
				return fmt.Errorf("truncated map: self-reference for hunk %d", idx)
			}

			lastSelf = int(v)
			rec.selfHunk = lastSelf

		case mapCompSelf1:
			lastSelf++

			fallthrough
		case mapCompSelf0:
			rec.selfHunk = lastSelf

		case mapCompParent, mapCompParentSelf, mapCompParent0, mapCompParent1:
			return fmt.Errorf("hunk %d comes from a parent CHD, which is not supported", idx)

		default:
			return fmt.Errorf("hunk %d has unknown compression type %d", idx, rec.compType)
		}

		if rec.selfHunk >= len(records) {
			return fmt.Errorf("hunk %d refers to hunk %d, past the end of the map", idx, rec.selfHunk)
		}
	}

	return nil
}

// 16 code lengths as 4-bit values, escape 1: "1 1" is a literal 1, "1 v N" is N+3 of v.
func readHuffmanTree(br *bitReader) ([]int, error) {
	symLen := make([]int, huffmanNumSymbols)

	for i := 0; i < huffmanNumSymbols; {
		v, ok := br.readBits(huffmanRLEBitWidth)
		if !ok {
			return nil, errors.New("truncated map: tree")
		}

		if v != 1 {
			symLen[i] = int(v)
			i++

			continue
		}

		v2, ok := br.readBits(huffmanRLEBitWidth)
		if !ok {
			return nil, errors.New("truncated map: tree escape")
		}

		if v2 == 1 {
			symLen[i] = 1
			i++

			continue
		}

		cnt, ok := br.readBits(huffmanRLEBitWidth)
		if !ok {
			return nil, errors.New("truncated map: tree run length")
		}

		for j := 0; j < int(cnt)+rleCountBias && i < huffmanNumSymbols; j++ {
			symLen[i] = int(v2)
			i++
		}
	}

	return symLen, nil
}

type huffmanCodeEntry struct {
	code   uint32
	length int
	sym    int
}

// MAME derives each length's first code from the longest length down, not the
// textbook shortest-first. The two agree only when every symbol shares one code
// length, the only tree Write emits; chdman's trees decode to different symbols.
// https://github.com/rtissera/libchdr/blob/5f82799f2c8cad1e9cd26d39a0f8d36369a5534b/src/libchdr_huffman.c#L496
func buildCanonicalCodes(symLen []int) ([]huffmanCodeEntry, error) {
	var histo [maxHuffmanBits + 1]uint32

	for _, l := range symLen {
		if l > 0 {
			histo[l]++
		}
	}

	var start [maxHuffmanBits + 1]uint32

	curStart := uint32(0)

	for l := maxHuffmanBits; l > 0; l-- {
		next := (curStart + histo[l]) >> 1
		if l != 1 && next*2 != curStart+histo[l] {
			return nil, errors.New("map code lengths do not form a Huffman tree")
		}

		start[l] = curStart
		curStart = next
	}

	var codes []huffmanCodeEntry

	for sym, l := range symLen {
		if l == 0 {
			continue
		}

		codes = append(codes, huffmanCodeEntry{code: start[l], length: l, sym: sym})
		start[l]++
	}

	return codes, nil
}

func decodeHuffmanSym(br *bitReader, codes []huffmanCodeEntry) (int, error) {
	var accum uint32

	for bits := 1; bits <= huffmanNumSymbols; bits++ {
		bit, ok := br.readBits(1)
		if !ok {
			return 0, errors.New("truncated bitstream in Huffman decode")
		}

		accum = (accum << 1) | bit

		for _, c := range codes {
			if c.length == bits && c.code == accum {
				return c.sym, nil
			}
		}
	}

	return 0, errors.New("no matching Huffman code")
}

// Resolves a self-reference to the hunk holding the data, then checks the map's CRC,
// so a hunk that decompresses cleanly into the wrong bytes cannot reach a caller.
func readHunk(f *os.File, records []mapReadRecord, idx, storedHunkBytes int) ([]byte, error) {
	for range maxSelfChain {
		rec := records[idx]
		if rec.selfHunk < 0 {
			return readStoredHunk(f, rec, storedHunkBytes)
		}

		idx = rec.selfHunk
	}

	return nil, errors.New("self-references form a cycle")
}

func readStoredHunk(f *os.File, rec mapReadRecord, storedHunkBytes int) ([]byte, error) {
	raw := make([]byte, storedHunkBytes)

	if rec.compType == mapCompNone {
		if _, err := f.ReadAt(raw, rec.offset); err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
	} else {
		compressed := make([]byte, rec.length)
		if _, err := f.ReadAt(compressed, rec.offset); err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}

		var err error
		if raw, err = decompressCDZLIB(compressed, storedHunkBytes); err != nil {
			return nil, err
		}
	}

	if got := crc16CCITT(raw); got != rec.crc {
		return nil, fmt.Errorf("%w: got %#04x, want %#04x", ErrCRCMismatch, got, rec.crc)
	}

	return raw, nil
}

func decompressCDZLIB(data []byte, storedHunkBytes int) ([]byte, error) {
	frames := storedHunkBytes / slotBytes
	eccBytes := (frames + bitsPerByte - 1) / bitsPerByte // 1 for framesPerHunk=8

	const sectorCmpLenBytes = 2

	if len(data) < eccBytes+sectorCmpLenBytes {
		return nil, fmt.Errorf("CDZLIB hunk too short (%d bytes)", len(data))
	}

	baseCmpLen := int(binary.BigEndian.Uint16(data[eccBytes:]))
	sectorCmpStart := eccBytes + sectorCmpLenBytes

	if sectorCmpStart+baseCmpLen > len(data) {
		return nil, errors.New("CDZLIB sector block overflows hunk")
	}

	sectorData, err := zlibDecompress(data[sectorCmpStart:sectorCmpStart+baseCmpLen], frames*sectorBytes)
	if err != nil {
		return nil, fmt.Errorf("decompress sector data: %w", err)
	}

	// Unused here, but covered by the stored hunk CRC, so it still has to be decoded.
	subcodeData, err := zlibDecompress(data[sectorCmpStart+baseCmpLen:], frames*subcodeBytes)
	if err != nil {
		return nil, fmt.Errorf("decompress subcode data: %w", err)
	}

	out := make([]byte, storedHunkBytes)
	stripped := data[:eccBytes]
	tables := newECCTables()

	for i := range frames {
		copy(out[i*slotBytes:], sectorData[i*sectorBytes:(i+1)*sectorBytes])
		copy(out[i*slotBytes+sectorBytes:], subcodeData[i*subcodeBytes:(i+1)*subcodeBytes])

		// A set bit means the encoder dropped this sector's sync header and parity: chdman always does, Write never.
		if stripped[i/bitsPerByte]&(1<<(i%bitsPerByte)) != 0 {
			tables.restoreSector(out[i*slotBytes : i*slotBytes+sectorBytes])
		}
	}

	return out, nil
}

// Same reasoning as flateWriters: two decoder allocations per hunk otherwise.
//
//nolint:gochecknoglobals // pool of reusable decoders, no observable state
var flateReaders sync.Pool

// What flate.NewReader actually returns; declared so the pooled value keeps both halves.
type resettableReader interface {
	io.ReadCloser
	flate.Resetter
}

func zlibDecompress(data []byte, expectedSize int) ([]byte, error) {
	r, pooled := flateReaders.Get().(resettableReader)
	if pooled {
		if err := r.Reset(bytes.NewReader(data), nil); err != nil {
			return nil, fmt.Errorf("flate reset: %w", err)
		}
	} else {
		reader, ok := flate.NewReader(bytes.NewReader(data)).(resettableReader)
		if !ok {
			return nil, errors.New("flate reader cannot be reset")
		}

		r = reader
	}

	defer flateReaders.Put(r)
	defer r.Close()

	out := make([]byte, expectedSize)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("flate decompress: %w", err)
	}

	return out, nil
}

// MSB-first, matching bitWriter in chd.go.
type bitReader struct {
	buf    []byte
	bitPos int
}

func (br *bitReader) readBits(n int) (uint32, bool) {
	var v uint32

	for range n {
		byteIdx := br.bitPos / bitsPerByte
		if byteIdx >= len(br.buf) {
			return 0, false
		}

		bitIdx := bitWriterMSBIndex - (br.bitPos % bitsPerByte)
		bit := uint32((br.buf[byteIdx] >> bitIdx) & 1)
		v = (v << 1) | bit
		br.bitPos++
	}

	return v, true
}
