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
	"iter"
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

	// Hunks decompressed at once: a larger batch amortises the sync but holds more buffers.
	hunksPerBatch = 256

	// Far past any disc, so a larger size is a damaged field. The map is allocated from it.
	maxLogicalBytes = 64 << 30
)

// Raw DEFLATE has no checksum: damage can decode cleanly into the wrong bytes, which no decoder error would report.
var ErrCRCMismatch = errors.New("failed its stored CRC")

type TrackInfo struct {
	Number      int
	CUEType     string // "MODE1/2352", "AUDIO", etc.
	TotalFrames int    // CHD sector slots allocated to this track (includes PAD)
	RealFrames  int    // frames with actual data (TotalFrames - PAD)
	Pregap      int    // frames before this track's data that redump keeps at the head of its file
	GDROM       bool   // from a CHGD entry, so the high-density boundary applies
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
	codecs       [numCompressors]uint32

	rawSHA1      []byte // as stored; Verify compares its own recomputation
	combinedSHA1 []byte
}

// An unsupported codec is rejected rather than decoded to plausible garbage, and every
// size is bounded before anything is allocated from it.

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

	var codecs [numCompressors]uint32

	for i := range codecs {
		codecs[i] = binary.BigEndian.Uint32(hdr[headerCodecOffset+i*4:])
		// Slot 0 must be set; a later zero slot just means chdman was given fewer codecs.
		if codecs[i] == 0 && i > 0 {
			continue
		}

		if !supportedCodec(codecs[i]) {
			return fileHeader{}, fmt.Errorf("%s: compressor %d is %q, which swirl cannot decode",
				path, i, tagString(codecs[i]))
		}
	}

	//nolint:gosec // G115: the values are range-checked immediately below
	h := fileHeader{
		size:         info.Size(),
		logicalBytes: int64(binary.BigEndian.Uint64(hdr[0x20:])),
		mapOffset:    int64(binary.BigEndian.Uint64(hdr[0x28:])),
		metaOffset:   int64(binary.BigEndian.Uint64(hdr[0x30:])),
		hunkBytes:    int(binary.BigEndian.Uint32(hdr[0x38:])),
		version:      version,
		codecs:       codecs,

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

	if h.logicalBytes > maxLogicalBytes {
		return fileHeader{}, fmt.Errorf("%s: header claims %d bytes, larger than any disc", path, h.logicalBytes)
	}

	// Rounds up: a trailing part-used hunk still counts. File size bounds nothing here,
	// because a self-referenced hunk stores no bytes of its own.
	h.numHunks = int((h.logicalBytes + int64(h.hunkBytes) - 1) / int64(h.hunkBytes))

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
	if err := extractHunks(ctx, c, writer, progress); err != nil {
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

// Routes sectors by LBA. A range starts before its track when a pregap heads the file;
// frames between ranges are padding no file wants.
type trackWriter struct {
	tracks []TrackInfo
	files  []*os.File
	spans  []span
	lba    int
	idx    int
}

type span struct{ start, end int }

func newTrackWriter(tracks []TrackInfo, files []*os.File) *trackWriter {
	spans := make([]span, len(tracks))

	startLBA := 0
	for i, t := range tracks {
		spans[i] = span{startLBA - t.Pregap, startLBA + t.RealFrames}
		startLBA += t.TotalFrames
	}

	// chdman folds a pregap into the track before it either as PAD or as that track's own
	// frames. In the second case the ranges overlap, and the frames belong to the later
	// track: the earlier one ends where its successor's file begins.
	for i := 1; i < len(spans); i++ {
		spans[i-1].end = min(spans[i-1].end, spans[i].start)
	}

	return &trackWriter{tracks: tracks, files: files, spans: spans}
}

// Sectors past the last track are dropped rather than refused: they are padding the
// hunk's CRC has already covered.
func (w *trackWriter) writeHunk(raw []byte) error {
	for off := 0; off+slotBytes <= len(raw); off += slotBytes {
		more, err := w.writeSector(raw[off : off+sectorBytes])
		if err != nil {
			return err
		}

		if !more {
			return nil
		}
	}

	return nil
}

// Reports false once every track is full.
func (w *trackWriter) writeSector(sector []byte) (bool, error) {
	lba := w.lba
	w.lba++

	for w.idx < len(w.spans) && lba >= w.spans[w.idx].end {
		w.idx++
	}

	if w.idx >= len(w.spans) {
		return false, nil
	}

	if lba < w.spans[w.idx].start {
		return true, nil
	}

	if disc.IsAudio(w.tracks[w.idx].CUEType) {
		swapAudio(sector)
	}

	if _, err := w.files[w.idx].Write(sector); err != nil {
		return false, fmt.Errorf("write track %d: %w", w.tracks[w.idx].Number, err)
	}

	return true, nil
}

// Hunks past the last track are still CRC-checked, so trailing data cannot hide corruption.
func extractHunks(ctx context.Context, c *chdFile, w *trackWriter, progress io.Writer) error {
	for raw, err := range hunkSeq(ctx, c) {
		if err != nil {
			return err
		}

		progress.Write(raw) //nolint:errcheck // progress reporting never affects the result

		if err := w.writeHunk(raw); err != nil {
			return err
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

		// Null-terminated ASCII: "TRACK:1 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:450 PREGAP:0 ...".
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

		// Only CHGD carries PAD, which makes FRAMES the allocated total; without it FRAMES is
		// the real count and the padding has to be derived.
		total, dataFrames := PadFrames(frames), frames

		if s, ok := kv["PAD"]; ok {
			pad, err := strconv.Atoi(s)
			if err != nil {
				return nil, fmt.Errorf("bad PAD in %q: %w", text, err)
			}

			total, dataFrames = frames, frames-pad
		}

		tracks = append(tracks, TrackInfo{
			Number:      num,
			CUEType:     chdTypeToCUE(kv["TYPE"]),
			TotalFrames: total,
			RealFrames:  dataFrames,
			GDROM:       m.tag == tagCHGD,
		})
	}

	assignPregaps(tracks)

	return tracks, nil
}

// Two seconds, per the Red Book. chdman folds these frames into the preceding track,
// as PAD or as track data, so the split redump records has to be derived.
const standardPregap = 150

// A track opening the high-density area is a session boundary, not a pregap.
func assignPregaps(tracks []TrackInfo) {
	startLBA, afterData, prevPad := 0, false, 0

	// Only where the CHD records the gap. chdman can instead fold a pregap into the
	// preceding track's own frames, leaving nothing to distinguish it from a disc that
	// never had one, and inventing frames there would corrupt both.
	for i, t := range tracks {
		if t.GDROM && afterData && prevPad >= standardPregap && startLBA != disc.HDAStartLBA &&
			disc.IsAudio(t.CUEType) {
			tracks[i].Pregap = standardPregap
		}

		afterData, prevPad = !disc.IsAudio(t.CUEType), t.TotalFrames-t.RealFrames
		startLBA += t.TotalFrames
	}
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
		case mapCompType0, mapCompType1, mapCompType2, mapCompType3, mapCompNone:
			next, err := readStoredEntry(br, rec, h, idx, lengthBits, offset)
			if err != nil {
				return err
			}

			offset = next

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

// MAME derives each length's first code from the longest length down, not the textbook
// shortest-first. They disagree on mixed-length trees, which Write now emits, so both
// sides build their codes here.
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

// Follows a self-reference to the hunk that holds the data.
func sourceHunk(records []mapReadRecord, idx int) (int, error) {
	for range maxSelfChain {
		if records[idx].selfHunk < 0 {
			return idx, nil
		}

		idx = records[idx].selfHunk
	}

	return 0, errors.New("self-references form a cycle")
}

// Yields every hunk in stream order, decompressing a batch at a time.
func hunkSeq(ctx context.Context, c *chdFile) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		for start := 0; start < len(c.records); start += hunksPerBatch {
			if err := ctx.Err(); err != nil {
				yield(nil, err)

				return
			}

			batch, err := decompressBatch(c.f, c.header, c.records, start, min(hunksPerBatch, len(c.records)-start))
			if err != nil {
				yield(nil, err)

				return
			}

			for _, raw := range batch {
				if !yield(raw, nil) {
					return
				}
			}
		}
	}
}

// Decodes hunks [start, start+n) in parallel, once per distinct stored hunk.
func decompressBatch(f *os.File, h fileHeader, records []mapReadRecord, start, n int) ([][]byte, error) {
	sources := make([]int, n)

	for i := range sources {
		src, err := sourceHunk(records, start+i)
		if err != nil {
			return nil, fmt.Errorf("hunk %d: %w", start+i, err)
		}

		sources[i] = src
	}

	out := make([][]byte, n)
	errs := make([]error, n)
	first := make(map[int]int, n)

	var wg sync.WaitGroup

	for i, src := range sources {
		if _, dup := first[src]; dup {
			continue
		}

		first[src] = i

		wg.Go(func() { out[i], errs[i] = readStoredHunk(f, h, records[src]) })
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("hunk %d: %w", sources[i], err)
		}
	}

	// Callers byte-swap audio in place, so a repeat cannot be handed the same buffer.
	for i, src := range sources {
		if first[src] != i {
			out[i] = bytes.Clone(out[first[src]])
		}
	}

	return out, nil
}

func readStoredHunk(f *os.File, h fileHeader, rec mapReadRecord) ([]byte, error) {
	raw := make([]byte, h.hunkBytes)

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
		if raw, err = decompressHunk(h.codecs[rec.compType], compressed, h.hunkBytes); err != nil {
			return nil, err
		}
	}

	if got := crc16CCITT(raw); got != rec.crc {
		return nil, fmt.Errorf("%w: got %#04x, want %#04x", ErrCRCMismatch, got, rec.crc)
	}

	return raw, nil
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

// Length, unless the hunk was stored whole, then CRC. Returns where the next hunk starts.
func readStoredEntry(
	br *bitReader, rec *mapReadRecord, h fileHeader, idx, lengthBits int, offset int64,
) (int64, error) {
	length := uint32(h.hunkBytes) //nolint:gosec // G115: hunkBytes fits in uint32

	if rec.compType != mapCompNone {
		if h.codecs[rec.compType] == 0 {
			return 0, fmt.Errorf("hunk %d names compressor %d, which the header leaves empty", idx, rec.compType)
		}

		l, ok := br.readBits(lengthBits)
		if !ok {
			return 0, fmt.Errorf("truncated map: length for hunk %d", idx)
		}

		length = l
	}

	crc, ok := br.readBits(mapCRCBits)
	if !ok {
		return 0, fmt.Errorf("truncated map: CRC for hunk %d", idx)
	}

	rec.length, rec.offset = length, offset
	rec.crc = uint16(crc) //nolint:gosec // G115: the field is exactly mapCRCBits wide

	return offset + int64(length), nil
}
