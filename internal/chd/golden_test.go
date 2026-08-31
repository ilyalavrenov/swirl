package chd

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilyalavrenov/swirl/internal/disc"
)

// testdata/golden.chd was produced by `chdman createcd -c cdzl` from a synthetic
// two-track disc, and is the only fixture in this package this code did not write
// itself. It is shaped to reach every decoder path that swirl's own encoder never
// emits, so a regression in any of them fails here:
//
//   - 60 frames of 2448 bytes is 7.5 hunks, so the hunk count must round up
//   - chdman strips each MODE1 sector's sync header and P/Q parity (bitmap 0xff)
//   - the audio is two hunk-sized blocks repeated, ABAB, so the third audio hunk
//     is a self-reference and the fourth is a SELF_1 one past the previous target
//   - the audio track is stored byte-swapped
const goldenPath = "testdata/golden.chd"

// Digests as chdman itself reports them, so agreement is with MAME's definition
// rather than with this package's own arithmetic.
const (
	goldenRawSHA1      = "999338dd4517ea2b6b45a06521e1a72bb548e6cb"
	goldenCombinedSHA1 = "5468829351b7462e8bde84a4885d0260d08110d3"
)

func TestGoldenDigestsMatchChdman(t *testing.T) {
	t.Parallel()

	report, err := Verify(t.Context(), goldenPath, nil)
	require.NoError(t, err)

	assert.Equal(t, goldenRawSHA1, hexString(report.RawSHA1), "raw SHA1")
	assert.Equal(t, goldenCombinedSHA1, hexString(report.CombinedSHA1), "combined SHA1")
	assert.Equal(t, 8, report.Hunks, "7.5 hunks of data must round up to 8")
}

func hexString(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		sb.WriteString(string("0123456789abcdef"[c>>4]))
		sb.WriteString(string("0123456789abcdef"[c&0xF]))
	}

	return sb.String()
}

func TestGoldenUsesThePathsWriteNeverEmits(t *testing.T) {
	t.Parallel()

	c, err := openCHD(goldenPath)
	require.NoError(t, err)

	defer c.close()

	assert.Equal(t, 8, c.header.numHunks)
	assert.Greater(t, int64(c.header.numHunks), c.header.logicalBytes/int64(c.header.hunkBytes),
		"the fixture must not be hunk-aligned, or the rounding is untested")

	selfs := 0

	for _, r := range c.records {
		if r.selfHunk >= 0 {
			selfs++
		}
	}

	assert.Positive(t, selfs, "the fixture must contain a self-reference")

	first := make([]byte, c.records[0].length)
	_, err = c.f.ReadAt(first, c.records[0].offset)
	require.NoError(t, err)
	assert.NotZero(t, first[0], "the fixture's ECC bitmap must be set, or stripping is untested")
}

func TestGoldenExtractsTheOriginalTracks(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()

	tracks, err := Read(t.Context(), goldenPath, outputDir, nil)
	require.NoError(t, err)
	require.Len(t, tracks, 2)

	data, err := os.ReadFile(filepath.Join(outputDir, "track01.bin"))
	require.NoError(t, err)
	require.Len(t, data, 24*sectorBytes)

	// A restored MODE1 sector begins with the sync pattern chdman dropped, and its
	// parity must be non-zero for the CRC to have matched.
	assert.Equal(t, syncHeader(), data[:len(syncHeader())])
	assert.NotEqual(t, make([]byte, sectorBytes-eccPOffset), data[eccPOffset:sectorBytes])
	assert.Contains(t, string(data[16:16+2048]), "SWIRL TEST DISC")

	audio, err := os.ReadFile(filepath.Join(outputDir, "track02.raw"))
	require.NoError(t, err)
	require.Len(t, audio, 36*sectorBytes)

	blockA := make([]byte, 8*sectorBytes)
	for i := range blockA {
		blockA[i] = byte(i*7 + 3)
	}

	assert.Equal(t, blockA, audio[:len(blockA)], "audio comes back in the byte order the source had")
	assert.Equal(t, blockA, audio[16*sectorBytes:24*sectorBytes], "the self-referenced copy matches it")
}

// chdman is the authority on whether swirl's output is readable by other software.
// It is not a build dependency, so this skips when the binary is absent.
func TestChdmanReadsWhatWriteProduces(t *testing.T) {
	t.Parallel()

	chdman, err := exec.LookPath("chdman")
	if err != nil {
		t.Skip("chdman not installed")
	}

	audio := make([]byte, 8*sectorBytes)
	for i := range audio {
		audio[i] = byte(i*13 + 5)
	}

	path := writeCHD(t, []Track{
		makeTrack(1, "MODE1/2352", 8, 0, 0x5A),
		{Number: 2, Type: "AUDIO", Frames: 8, Data: bytes.NewReader(bytes.Clone(audio))},
	})

	out, err := exec.CommandContext(t.Context(), chdman, "verify", "-i", path).CombinedOutput()
	require.NoError(t, err, "chdman verify: %s", out)
	assert.Contains(t, string(out), "Raw SHA1 verification successful")
	assert.Contains(t, string(out), "Overall SHA1 verification successful")

	dir := t.TempDir()
	out, err = exec.CommandContext(t.Context(), chdman,
		"extractcd", "-i", path, "-o", filepath.Join(dir, "o.cue"), "-f").CombinedOutput()
	require.NoError(t, err, "chdman extractcd: %s", out)

	// chdman concatenates a plain CD's tracks into one .bin, so the audio track
	// begins where the 8-frame data track ends.
	got, err := os.ReadFile(filepath.Join(dir, "o.bin"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(got), 16*sectorBytes)
	assert.Equal(t, audio, got[8*sectorBytes:16*sectorBytes],
		"chdman must see the audio in the order the source had")
}

// testdata/golden-gdrom.chd is the same idea at GD-ROM scale: a 105 MB disc that
// compresses to 9 KB because nearly all of it is silence. The scale is the point.
// Six hunks cannot produce a long enough run of one compression type for chdman to
// reach for RLE_LARGE, nor a Huffman tree with more than two code lengths, and the
// code-length spread is exactly what the canonical-order bug turned on.
const goldenGDROMPath = "testdata/golden-gdrom.chd"

const (
	goldenGDROMRawSHA1      = "7e03f92ac3069419294e189cdea4875800cf773e"
	goldenGDROMCombinedSHA1 = "43b586526f1229205fcce751e90cdb31c802d9a2"
)

func TestGoldenGDROMDigestsMatchChdman(t *testing.T) {
	t.Parallel()

	report, err := Verify(t.Context(), goldenGDROMPath, nil)
	require.NoError(t, err)

	assert.Equal(t, goldenGDROMRawSHA1, hexString(report.RawSHA1), "raw SHA1")
	assert.Equal(t, goldenGDROMCombinedSHA1, hexString(report.CombinedSHA1), "combined SHA1")
	assert.Equal(t, 5627, report.Hunks)
}

func TestGoldenGDROMReachesTheWiderPaths(t *testing.T) {
	t.Parallel()

	c, err := openCHD(goldenGDROMPath)
	require.NoError(t, err)

	defer c.close()

	require.Len(t, c.tracks, 3)

	for _, tr := range c.tracks {
		assert.True(t, tr.GDROM, "track %d must carry the CHGD tag", tr.Number)
	}

	assert.Equal(t, disc.HDAStartLBA, c.tracks[0].TotalFrames+c.tracks[1].TotalFrames,
		"the tracks before the high-density area must sum to its start")

	selfs := 0

	for _, r := range c.records {
		if r.selfHunk >= 0 {
			selfs++
		}
	}

	assert.Greater(t, selfs, 5000, "most of a silent disc should deduplicate")

	// A tree with several code lengths is what separates MAME's canonical order
	// from the textbook one; two lengths is the minimum that can tell them apart.
	lengths := map[int]bool{}

	for _, l := range goldenTreeLengths(t, goldenGDROMPath) {
		if l > 0 {
			lengths[l] = true
		}
	}

	assert.Greater(t, len(lengths), 2, "code lengths present: %v", lengths)
}

func goldenTreeLengths(t *testing.T, path string) []int {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	mapOffset := binary.BigEndian.Uint64(raw[0x28:])
	compLen := binary.BigEndian.Uint32(raw[mapOffset:])

	br := &bitReader{buf: raw[mapOffset+mapHeaderBytes : mapOffset+mapHeaderBytes+uint64(compLen)]}

	lengths, err := readHuffmanTree(br)
	require.NoError(t, err)

	return lengths
}

func TestGoldenGDROMExtractsEveryTrack(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()

	tracks, err := Read(t.Context(), goldenGDROMPath, outputDir, nil)
	require.NoError(t, err)
	require.Len(t, tracks, 3)

	data, err := os.ReadFile(filepath.Join(outputDir, "track01.bin"))
	require.NoError(t, err)
	assert.Equal(t, syncHeader(), data[:len(syncHeader())], "sync restored on the first sector")
	assert.Contains(t, string(data[16:16+2048]), "SWIRL GD TEST")

	audio, err := os.ReadFile(filepath.Join(outputDir, "track02.raw"))
	require.NoError(t, err)

	want := make([]byte, 16*sectorBytes)
	for i := range want {
		want[i] = byte(i*7 + 3)
	}

	assert.Equal(t, want, audio[:len(want)], "audio byte order survives the round trip")

	hda, err := os.ReadFile(filepath.Join(outputDir, "track03.bin"))
	require.NoError(t, err)
	require.Len(t, hda, 12*sectorBytes)
	assert.Equal(t, syncHeader(), hda[:len(syncHeader())], "parity restored past the high-density boundary")
}
