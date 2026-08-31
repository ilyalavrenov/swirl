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
	// byte; see TestReadDropsTrackPadding for what happens otherwise.
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
			corrupt: func(b []byte) { copy(b[0x10:0x14], "avhu") },
			want:    `compressor 0 is "avhu"`,
		},
		"unsupported codec in a later slot": {
			corrupt: func(b []byte) { copy(b[0x14:0x18], "avhu") },
			want:    `compressor 1 is "avhu"`,
		},
		"no compressor at all": {
			corrupt: func(b []byte) { copy(b[0x10:0x14], "\x00\x00\x00\x00") },
			want:    `compressor 0 is "\x00\x00\x00\x00"`,
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

// Two tracks because trailing zeros on the unaligned track are only half of it: the
// padding also pushes every later track off its start sector.
func TestReadDropsTrackPadding(t *testing.T) {
	t.Parallel()

	const (
		firstFrames  = 5 // not 4-aligned, so three padding sectors follow
		secondFrames = 6
	)

	path := writeCHD(t, []Track{
		makeTrack(1, disc.TrackTypeMode1, firstFrames, 0, 0x33),
		makeTrack(2, disc.TrackTypeMode1, secondFrames, 0, 0x77),
	})

	outputDir := t.TempDir()

	tracks, err := Read(t.Context(), path, outputDir, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, firstFrames, tracks[0].RealFrames)
	assert.Equal(t, PadFrames(firstFrames), tracks[0].TotalFrames, "the slots are still allocated")

	for name, test := range map[string]struct {
		file   string
		frames int
		fill   byte
	}{
		"unaligned first track": {"track01.bin", firstFrames, 0x33},
		"the track after it":    {"track02.bin", secondFrames, 0x77},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			want := make([]byte, test.frames*sectorBytes)
			for i := range want {
				want[i] = test.fill ^ byte(i)
			}

			got, err := os.ReadFile(filepath.Join(outputDir, test.file))
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
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
