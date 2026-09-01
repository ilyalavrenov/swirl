package chd

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilyalavrenov/swirl/internal/disc"
)

// corrupt writes a copy of a valid CHD with one edit applied.
func corrupt(t *testing.T, valid []byte, edit func([]byte)) string {
	t.Helper()

	broken := bytes.Clone(valid)
	edit(broken)

	path := filepath.Join(t.TempDir(), "broken.chd")
	require.NoError(t, os.WriteFile(path, broken, 0o600))

	return path
}

func validCHD(t *testing.T) (string, []byte) {
	t.Helper()

	path := writeCHD(t, []Track{
		makeTrack(1, disc.TrackTypeMode1, 12, 0, 0x5A),
		makeTrack(2, disc.TrackTypeAudio, 8, 0, 0xA5),
	})

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	return path, raw
}

func TestVerifyRoundTrip(t *testing.T) {
	t.Parallel()

	path := writeCHD(t, []Track{
		makeTrack(1, disc.TrackTypeMode1, 12, 0, 0x5A),
		makeTrack(2, disc.TrackTypeAudio, 8, 0, 0xA5),
	})

	report, err := Verify(t.Context(), path, nil)
	require.NoError(t, err)

	assert.Len(t, report.Tracks, 2)
	assert.Equal(t, hunkCount(20), report.Hunks)
	assert.Equal(t, int64(report.Hunks)*hunkBytes, report.LogicalBytes)
}

// Write hashes the hunks it assembles, Verify hashes the hunks it decompressed,
// so agreement rules out an asymmetry between the two codecs.
func TestVerifyDigestsMatchHeader(t *testing.T) {
	t.Parallel()

	path, valid := validCHD(t)

	report, err := Verify(t.Context(), path, nil)
	require.NoError(t, err)

	assert.Equal(t, valid[headerRawSHA1Offset:headerRawSHA1Offset+20], report.RawSHA1)
	assert.Equal(t, valid[headerCombinedSHA1Offset:headerCombinedSHA1Offset+20], report.CombinedSHA1)
}

func TestVerifyRejectsTampering(t *testing.T) {
	t.Parallel()

	_, valid := validCHD(t)
	firstHunk := firstHunkOffset(t, valid)

	for name, test := range map[string]struct {
		edit func([]byte)
		want string
	}{
		// Raw DEFLATE carries no checksum, so damage is caught either by the
		// decoder giving up or by the per-hunk CRC; both name the hunk.
		"corrupt hunk data": {
			edit: func(b []byte) { b[firstHunk+8] ^= 0xFF },
			want: "hunk 0",
		},
		"raw digest does not match the data": {
			edit: func(b []byte) { b[headerRawSHA1Offset] ^= 0xFF },
			want: "hunk data hashes to",
		},
		"combined digest does not match the metadata": {
			edit: func(b []byte) { b[headerCombinedSHA1Offset] ^= 0xFF },
			want: "metadata hashes to",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := Verify(t.Context(), corrupt(t, valid, test.edit), nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

// TestDigestsAreStable pins both header digests for a fixed disc. It cannot prove
// the values are what chdman would write, only that they never change by
// accident, which is what matters for a format other software has to agree with.
//
// The three tracks are chosen so that hashing their metadata sorts into an order
// none of them started in: with a fixture that happens to be pre-sorted, dropping
// the sort entirely would leave the digest unchanged and this test asleep.
func TestDigestsAreStable(t *testing.T) {
	t.Parallel()

	path := writeCHD(t, []Track{
		makeTrack(1, disc.TrackTypeMode1, 4, 0, 0x5A),
		makeTrack(2, disc.TrackTypeAudio, 8, 0, 0xA5),
		makeTrack(3, disc.TrackTypeAudio, 8, 0, 0x3C),
	})

	report, err := Verify(t.Context(), path, nil)
	require.NoError(t, err)

	assert.Equal(t, "891256b36712482670167e1c7f94e07b27b764e8", hex.EncodeToString(report.RawSHA1), "raw SHA1")
	assert.Equal(t, "b620fb388670ae8e767c328634de0b80bdd34632", hex.EncodeToString(report.CombinedSHA1), "combined SHA1")
}

// A disc whose size is not a whole number of hunks ends in a part-used one, and
// only the bytes it actually claims belong in the digest. Shrinking the logical
// size must therefore change what Verify computes: if it hashed whole hunks
// regardless, the stored digest would still match and this would pass.
func TestVerifyDigestStopsAtTheLogicalSize(t *testing.T) {
	t.Parallel()

	path, valid := validCHD(t)

	report, err := Verify(t.Context(), path, nil)
	require.NoError(t, err)

	hunkSize := int64(binary.BigEndian.Uint32(valid[0x38:]))
	require.Positive(t, report.LogicalBytes)

	short := corrupt(t, valid, func(b []byte) {
		binary.BigEndian.PutUint64(b[0x20:], uint64(report.LogicalBytes-hunkSize+1))
	})

	_, err = Verify(t.Context(), short, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hunk data hashes to")
}

func TestVerifyReportsProgress(t *testing.T) {
	t.Parallel()

	path := writeCHD(t, []Track{makeTrack(1, disc.TrackTypeMode1, 16, 0, 0x01)})

	sink := &bytes.Buffer{}

	report, err := Verify(t.Context(), path, sink)
	require.NoError(t, err)
	assert.Equal(t, report.Hunks*hunkBytes, sink.Len(), "every decompressed byte reaches the progress writer")
}

func TestStat(t *testing.T) {
	t.Parallel()

	path := writeCHD(t, []Track{
		makeTrack(1, disc.TrackTypeMode1, 12, 0, 0x5A),
		makeTrack(2, disc.TrackTypeAudio, 8, 0, 0xA5),
	})

	info, err := Stat(path)
	require.NoError(t, err)

	assert.Equal(t, uint32(chdVersion), info.Version)
	assert.Equal(t, "cdzl", info.Codec)
	assert.Equal(t, hunkBytes, info.HunkBytes)
	assert.Equal(t, hunkCount(20), info.Hunks)
	require.Len(t, info.Tracks, 2)
	assert.Equal(t, disc.TrackTypeAudio, info.Tracks[1].CUEType)
}

// TestWriteAtomic pins that the output only ever appears complete: it is built
// under another name and renamed, so a killed or failed run cannot leave a
// truncated file that still answers to the CHD magic.
func TestWriteAtomic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "disc.chd")

	require.NoError(t, Write(t.Context(), path, []Track{makeTrack(1, disc.TrackTypeMode1, 8, 0, 0x01)}, CodecDeflate, nil))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the staging file is renamed, not left beside the output")
	assert.Equal(t, "disc.chd", entries[0].Name())
}

func TestWriteFailureKeepsExistingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "disc.chd")
	require.NoError(t, os.WriteFile(path, []byte("previous"), 0o600))

	// Frames promises eight sectors, the reader holds two.
	err := Write(t.Context(), path, []Track{{
		Number: 1,
		Type:   disc.TrackTypeMode1,
		Frames: 8,
		Data:   bytes.NewReader(bytes.Repeat([]byte{0x01}, 2*sectorBytes)),
	}}, CodecDeflate, nil)
	require.Error(t, err)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "previous", string(got), "a failed write must not touch what was already there")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "and must not leave a staging file behind")
}
