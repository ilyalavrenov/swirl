package chd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilyalavrenov/swirl/internal/disc"
)

func TestReadExtractsTrackData(t *testing.T) {
	t.Parallel()

	// Both counts are multiples of the track padding, so these round-trip byte for
	// byte; see TestReadPadsUnalignedTracks for what happens otherwise.
	const t1Frames, t2Frames = 12, 8

	// Varied within each 16-bit pair so the audio track's byte swapping shows up.
	t1 := make([]byte, t1Frames*sectorBytes)
	t2 := make([]byte, t2Frames*sectorBytes)

	for i := range t1 {
		t1[i] = 0x11 ^ byte(i)
	}

	for i := range t2 {
		t2[i] = 0x22 ^ byte(i)
	}

	path := writeCHD(t, []Track{
		{Number: 1, Type: disc.TrackTypeMode1, Frames: t1Frames, Data: bytes.NewReader(t1)},
		{Number: 2, Type: disc.TrackTypeAudio, Frames: t2Frames, Data: bytes.NewReader(t2)},
	})

	outputDir := t.TempDir()

	tracks, err := Read(t.Context(), path, outputDir, io.Discard)
	require.NoError(t, err)
	require.Len(t, tracks, 2)

	assert.Equal(t, disc.TrackTypeMode1, tracks[0].CUEType)
	assert.Equal(t, disc.TrackTypeAudio, tracks[1].CUEType)
	assert.False(t, tracks[0].GDROM, "a plain CD carries CHT2 metadata")

	got1, err := os.ReadFile(filepath.Join(outputDir, "track01.bin"))
	require.NoError(t, err)
	assert.Equal(t, t1, got1)

	got2, err := os.ReadFile(filepath.Join(outputDir, "track02.raw"))
	require.NoError(t, err)
	assert.Equal(t, t2, got2)
}

// TestReadGDROMFlag checks the CHGD tag: bridge padding is what makes Write
// choose it.
func TestReadGDROMFlag(t *testing.T) {
	t.Parallel()

	path := writeCHD(t, []Track{
		{
			Number: 1, Type: disc.TrackTypeMode1, Frames: 4,
			Data: bytes.NewReader(bytes.Repeat([]byte{0x01}, 4*sectorBytes)),
		},
		{
			Number: 2, Type: disc.TrackTypeAudio, Frames: 4, StoredFrames: 16,
			Data: bytes.NewReader(bytes.Repeat([]byte{0x02}, 4*sectorBytes)),
		},
	})

	outputDir := t.TempDir()

	tracks, err := Read(t.Context(), path, outputDir, io.Discard)
	require.NoError(t, err)
	require.Len(t, tracks, 2)

	assert.True(t, tracks[0].GDROM, "bridge padding means a GD-ROM, tagged CHGD")
	assert.Equal(t, 16, tracks[1].TotalFrames)
	assert.Equal(t, 4, tracks[1].RealFrames, "the padding is not real data")

	got, err := os.ReadFile(filepath.Join(outputDir, "track02.raw"))
	require.NoError(t, err)
	assert.Equal(t, bytes.Repeat([]byte{0x02}, 4*sectorBytes), got, "the 12 pad frames are not written out")
}

func TestReadRejectsBadInput(t *testing.T) {
	t.Parallel()

	valid, err := os.ReadFile(writeCHD(t, []Track{makeTrack(1, disc.TrackTypeMode1, 8, 0, 0x5A)}))
	require.NoError(t, err)

	for name, test := range map[string]struct {
		corrupt func([]byte)
		want    string
	}{
		"not a CHD": {
			corrupt: func(b []byte) { copy(b[0:8], "NOTACHD!") },
			want:    "not a CHD file",
		},
		"wrong version": {
			corrupt: func(b []byte) { b[0x0F] = 4 },
			want:    "is not supported",
		},
		"unsupported codec": {
			corrupt: func(b []byte) { copy(b[0x10:0x14], "cdlz") },
			want:    "want cdzl",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			broken := bytes.Clone(valid)
			test.corrupt(broken)

			path := filepath.Join(t.TempDir(), "broken.chd")
			require.NoError(t, os.WriteFile(path, broken, 0o600))

			_, err := Read(t.Context(), path, t.TempDir(), io.Discard)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

// TestReadHunkCRC flips every byte of the hunk stream rather
// than pinning one offset. Raw DEFLATE carries no checksum, so a flip may decode
// cleanly into the wrong bytes; that case is what the per-hunk CRC exists for,
// and it has to happen at least once here or the CRC check goes untested.
func TestReadHunkCRC(t *testing.T) {
	t.Parallel()

	valid, err := os.ReadFile(writeCHD(t, []Track{makeTrack(1, disc.TrackTypeMode1, 24, 0, 0x5A)}))
	require.NoError(t, err)

	start := firstHunkOffset(t, valid)
	caughtByCRC, caughtByFlate := 0, 0

	for offset := start; offset < int64(len(valid)); offset++ {
		broken := bytes.Clone(valid)
		broken[offset] ^= 0x01

		path := filepath.Join(t.TempDir(), "corrupt.chd")
		require.NoError(t, os.WriteFile(path, broken, 0o600))

		_, err := Read(t.Context(), path, t.TempDir(), io.Discard)
		switch {
		case err == nil:
		case errors.Is(err, ErrCRCMismatch):
			caughtByCRC++
		default:
			caughtByFlate++
		}
	}

	assert.Positive(t, caughtByCRC, "corruption that decodes cleanly must be caught by the hunk CRC")
	assert.Positive(t, caughtByFlate, "corruption that breaks the stream must be caught by the decoder")
}

// TestReadPadsUnalignedTracks documents a known round-trip limitation: tracks are
// stored padded to a 4-frame boundary and a plain CD's CHT2 metadata has no PAD
// field, so a track whose length is not a multiple of 4 comes back with up to
// three trailing zero sectors. GD-ROM images use CHGD, which does carry PAD.
func TestReadPadsUnalignedTracks(t *testing.T) {
	t.Parallel()

	const frames = 5

	path := writeCHD(t, []Track{makeTrack(1, disc.TrackTypeMode1, frames, 0, 0x33)})

	outputDir := t.TempDir()

	tracks, err := Read(t.Context(), path, outputDir, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, PadFrames(frames), tracks[0].RealFrames, "padding is indistinguishable from data here")

	want := make([]byte, frames*sectorBytes)
	for i := range want {
		want[i] = 0x33 ^ byte(i)
	}

	got, err := os.ReadFile(filepath.Join(outputDir, "track01.bin"))
	require.NoError(t, err)
	assert.Len(t, got, PadFrames(frames)*sectorBytes)
	assert.Equal(t, want, got[:frames*sectorBytes], "real sectors survive intact")
}

func TestLogicalBytes(t *testing.T) {
	t.Parallel()

	tracks := []Track{makeTrack(1, disc.TrackTypeMode1, 20, 0, 0x01)}
	path := writeCHD(t, tracks)

	got, err := LogicalBytes(path)
	require.NoError(t, err)
	assert.Equal(t, WriteBytes(tracks), got, "the header must agree with what Write planned")
}

func TestLogicalBytesRejectsNonCHD(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "notachd.chd")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte{0x00}, headerSize), 0o600))

	_, err := LogicalBytes(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a CHD file")
}
