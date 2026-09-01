package chd

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilyalavrenov/swirl/internal/disc"
)

// makeTrack varies the bytes within each 16-bit pair. A fixture of one repeated
// byte is blind to audio byte swapping, which would let that code change without
// moving any digest this suite pins.
func makeTrack(number int, trackType string, sectors, pregap int, fill byte) Track {
	data := make([]byte, sectors*sectorBytes)
	for i := range data {
		data[i] = fill ^ byte(i)
	}

	return Track{
		Number: number,
		Type:   trackType,
		Frames: sectors,
		Pregap: pregap,
		Data:   bytes.NewReader(data),
	}
}

func writeCHD(t *testing.T, tracks []Track) string {
	t.Helper()

	return writeCHDWith(t, tracks, CodecDeflate)
}

func writeCHDWith(t *testing.T, tracks []Track, codec Codec) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "disc.chd")
	require.NoError(t, Write(t.Context(), path, tracks, codec, io.Discard))

	return path
}

// metadataTexts walks the metadata chain straight out of the file bytes: a
// 16-byte entry header of tag, flags, uint24 length and uint64 next-offset, then
// the payload. Deliberately not readMetaChain, so the assertions pin the bytes
// libchdr will sscanf rather than agreeing with this package's own parser.
func metadataTexts(t *testing.T, path string) []string {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var (
		texts []string
		off   = int(binary.BigEndian.Uint64(raw[0x30:]))
	)

	for off != 0 {
		require.Less(t, off+metaEntryHeaderBytes, len(raw), "metadata offset inside the file")

		length := int(binary.BigEndian.Uint32(raw[off+4:]) & mask24)
		next := int(binary.BigEndian.Uint64(raw[off+8:]))

		payload := raw[off+metaEntryHeaderBytes : off+metaEntryHeaderBytes+length]
		texts = append(texts, strings.TrimRight(string(payload), "\x00"))
		off = next
	}

	return texts
}

func TestWriteSingleDataTrack(t *testing.T) {
	t.Parallel()

	got := metadataTexts(t, writeCHD(t, []Track{makeTrack(1, disc.TrackTypeMode1, 8, 0, 0xAB)}))

	assert.Equal(t, []string{
		"TRACK:1 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:8 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0",
	}, got, "8 frames is already a multiple of the track padding")
}

// MAME derives the padding from FRAMES, so a padded total shifts every following track.
func TestWriteMetadataFramesIsTheRealCount(t *testing.T) {
	t.Parallel()

	for name, frames := range map[string]int{
		"one short of a block":  1,
		"one past a block":      5,
		"already on a boundary": 8,
		"two short of a block":  9,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := metadataTexts(t, writeCHD(t, []Track{makeTrack(1, disc.TrackTypeMode1, frames, 0, 0xAA)}))
			require.Len(t, got, 1)
			assert.Contains(t, got[0], fmt.Sprintf("FRAMES:%d ", frames))
		})
	}
}

// TestWriteMetadataNoPregap is a regression test: Flycast reports "Unsupported
// subtype or pre/postgap" when PREGAP is non-zero, so the pregap is stored as
// ordinary track frames and the field stays 0 whatever the input says.
func TestWriteMetadataNoPregap(t *testing.T) {
	t.Parallel()

	const pregap = 150 // a full CD lead-in

	got := metadataTexts(t, writeCHD(t, []Track{makeTrack(1, disc.TrackTypeAudio, 20, pregap, 0xBB)}))
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "PREGAP:0", "the pregap is stored as track frames, not declared")
	assert.Contains(t, got[0], "FRAMES:20")
}

func TestWriteMultiTrack(t *testing.T) {
	t.Parallel()

	got := metadataTexts(t, writeCHD(t, []Track{
		makeTrack(1, disc.TrackTypeMode1, 8, 0, 0x01),
		makeTrack(2, disc.TrackTypeAudio, 8, 2, 0x02),
		makeTrack(3, disc.TrackTypeMode1, 8, 0, 0x03),
	}))

	assert.Equal(t, []string{
		"TRACK:1 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:8 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0",
		"TRACK:2 TYPE:AUDIO SUBTYPE:NONE FRAMES:8 PREGAP:0 PGTYPE:AUDIO PGSUB:NONE POSTGAP:0",
		"TRACK:3 TYPE:MODE1_RAW SUBTYPE:NONE FRAMES:8 PREGAP:0 PGTYPE:MODE1 PGSUB:NONE POSTGAP:0",
	}, got)
}

// Spans several read batches, with a fill that repeats every 256 sectors so most hunks
// deduplicate. A hunk arriving out of order shows up as the wrong sector index.
func TestWriteSectorDataRoundtrip(t *testing.T) {
	t.Parallel()

	const sectors = 2*hunksPerBatch*framesPerHunk + 5

	data := make([]byte, sectors*sectorBytes)
	for i := range data {
		data[i] = byte(i / sectorBytes)
	}

	path := writeCHD(t, []Track{{
		Number: 1,
		Type:   disc.TrackTypeMode1,
		Frames: sectors,
		Data:   bytes.NewReader(data),
	}})

	assert.Positive(t, selfReferences(t, path), "the repeated hunks must deduplicate")

	outputDir := t.TempDir()
	_, err := Read(t.Context(), path, outputDir, io.Discard)
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(outputDir, "track01.bin"))
	require.NoError(t, err)

	for sec := range sectors {
		assert.Equal(t, data[sec*sectorBytes:(sec+1)*sectorBytes],
			got[sec*sectorBytes:(sec+1)*sectorBytes], "sector %d", sec)
	}
}

// How many hunks a written CHD stores as a reference to an earlier one.
func selfReferences(t *testing.T, path string) int {
	t.Helper()

	c, err := openCHD(path)
	require.NoError(t, err)

	defer c.close()

	n := 0

	for _, r := range c.records {
		if r.selfHunk >= 0 {
			n++
		}
	}

	return n
}

// TestWriteHeader pins the CHD v5 header layout with literal offsets rather than
// this package's own parser.
func TestWriteHeader(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(writeCHD(t, []Track{makeTrack(1, disc.TrackTypeMode1, 8, 0, 0x00)}))
	require.NoError(t, err)

	assert.Equal(t, headerMagic, string(raw[0x00:0x08]))
	assert.Equal(t, []byte{0, 0, 0, chdVersion}, raw[0x0C:0x10], "version")
	assert.Equal(t, []byte("cdzl"), raw[0x10:0x14], "compressors[0]")
	assert.Equal(t, make([]byte, 12), raw[0x14:0x20], "compressors[1..3] must be unset")
}

// TestWriteStoredFramesOverride is a regression test for GD-ROM bridge padding:
// with StoredFrames set, FRAMES must report it rather than the padded real count,
// so the following track lands where a GD-ROM reader expects it.
func TestWriteStoredFramesOverride(t *testing.T) {
	t.Parallel()

	const bridgeStoredFrames = 20

	got := metadataTexts(t, writeCHD(t, []Track{
		makeTrack(1, disc.TrackTypeMode1, 4, 0, 0x01),
		{
			Number:       2,
			Type:         disc.TrackTypeAudio,
			Frames:       4,
			StoredFrames: bridgeStoredFrames,
			Data:         bytes.NewReader(bytes.Repeat([]byte{0x02}, 4*sectorBytes)),
		},
		makeTrack(3, disc.TrackTypeMode1, 4, 0, 0x03),
	}))

	require.Len(t, got, 3)
	assert.Contains(t, got[1], fmt.Sprintf("FRAMES:%d ", bridgeStoredFrames), "bridge honours StoredFrames")
	assert.Contains(t, got[0], "FRAMES:4 ", "track before the bridge is unaffected")
	assert.Contains(t, got[2], "FRAMES:4 ", "track after the bridge is unaffected")
}

func TestWriteMapIsHunkAligned(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(writeCHD(t, []Track{makeTrack(1, disc.TrackTypeMode1, 8, 0, 0x00)}))
	require.NoError(t, err)

	firstOffset := firstHunkOffset(t, raw)

	assert.Zero(t, firstOffset%hunkBytes, "hunk data must start on a hunk boundary")
	assert.Less(t, firstOffset, int64(len(raw)))
}

func TestCRC16CCITT(t *testing.T) {
	t.Parallel()

	// Standard check value for CRC-16/CCITT-FALSE, the variant the CHD map uses:
	// polynomial 0x1021, initial value 0xFFFF, no reflection.
	assert.Equal(t, uint16(0x29B1), crc16CCITT([]byte("123456789")))
}

// firstHunkOffset reads hunk 0's absolute file offset from bytes [4..9] of the
// compressed map header, a 48-bit big-endian value.
func firstHunkOffset(t *testing.T, raw []byte) int64 {
	t.Helper()

	mapOffset := int64(binary.BigEndian.Uint64(raw[0x28:]))
	require.Less(t, mapOffset+mapHeaderBytes, int64(len(raw)), "map header must be inside the file")

	return uint48BE(raw[mapOffset+2:])
}

func TestNilProgress(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "disc.chd")
	require.NoError(t, Write(t.Context(), path, []Track{makeTrack(1, disc.TrackTypeMode1, 8, 0, 0x01)}, CodecDeflate, nil))

	tracks, err := Read(t.Context(), path, t.TempDir(), nil)
	require.NoError(t, err)
	assert.Len(t, tracks, 1)
}

func TestWriteBytes(t *testing.T) {
	t.Parallel()

	// 8 frames fills exactly one hunk; 9 spills into a second.
	assert.Equal(t, int64(hunkBytes), WriteBytes([]Track{makeTrack(1, disc.TrackTypeMode1, 8, 0, 0)}))
	assert.Equal(t, int64(2*hunkBytes), WriteBytes([]Track{makeTrack(1, disc.TrackTypeMode1, 9, 0, 0)}))
}

// PCM smooth enough that FLAC beats deflate on it.
func sinePCM(t *testing.T) []byte {
	t.Helper()

	buf := make([]byte, hunkBytes)
	for i := range hunkBytes / audioFrameBytes {
		at := float64(i) / 44100
		v := int16(9000*math.Sin(2*math.Pi*220*at) +
			5000*math.Sin(2*math.Pi*333*at) +
			2500*math.Sin(2*math.Pi*55*at))
		binary.BigEndian.PutUint16(buf[i*audioFrameBytes:], uint16(v))
		binary.BigEndian.PutUint16(buf[i*audioFrameBytes+audioSampleBytes:], uint16(v/2))
	}

	return buf
}

func TestCompressHunkTriesFLACOnlyForAudio(t *testing.T) {
	t.Parallel()

	raw := sinePCM(t)

	_, audioType, err := compressHunk(hunk{data: raw, audio: true}, CodecFLAC)
	require.NoError(t, err)
	require.Equal(t, uint8(mapCompType1), audioType, "FLAC must win this hunk when it is audio")

	_, dataType, err := compressHunk(hunk{data: raw, audio: false}, CodecFLAC)
	require.NoError(t, err)
	assert.Equal(t, uint8(mapCompType0), dataType, "the same bytes on a data track stay deflate")
}

func TestCompressBatch(t *testing.T) {
	t.Parallel()

	raw := make([]byte, hunkBytes)
	_, err := rand.Read(raw)
	require.NoError(t, err)

	// A Dreamcast disc never reaches this branch: its zero subcode leaves deflate enough
	// headroom to win on every real hunk.
	t.Run("stores an incompressible hunk", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		records := make([]hunkRecord, 1)
		require.NoError(t, compressBatch(&buf, records, 0, []hunk{{data: raw}}, map[[sha1.Size]byte]int{}, CodecDeflate))

		assert.Equal(t, uint8(mapCompNone), records[0].compType)
		assert.Equal(t, raw, buf.Bytes(), "an incompressible hunk is stored verbatim")
		assert.Equal(t, uint32(hunkBytes), records[0].length)
	})

	t.Run("references a repeat instead of storing it twice", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		records := make([]hunkRecord, 2)
		require.NoError(t, compressBatch(&buf, records, 0, []hunk{{data: raw}, {data: raw}}, map[[sha1.Size]byte]int{}, CodecDeflate))

		assert.Equal(t, uint8(mapCompSelf), records[1].compType)
		assert.Equal(t, 0, records[1].selfHunk)
		assert.Equal(t, raw, buf.Bytes(), "the repeat adds nothing to the data area")
	})
}

// TestUint48BE pins the full width of a stored file offset: a truncated
// read agrees with a correct one for anything under 16 MB, which every fixture
// here is.
func TestUint48BE(t *testing.T) {
	t.Parallel()

	const offset = 0xABCDEF012345

	buf := make([]byte, 8)
	putUint48BE(buf[2:], offset)

	assert.Equal(t, int64(offset), uint48BE(buf))
}
