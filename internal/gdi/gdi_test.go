package gdi_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilyalavrenov/swirl/internal/disc"
	"github.com/ilyalavrenov/swirl/internal/gdi"
)

func writeGDI(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "disc.gdi")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func TestParseGDROM(t *testing.T) {
	t.Parallel()

	sheet, err := gdi.Parse(writeGDI(t, `3
1 0 4 2352 track01.bin 0
2 3000 0 2352 track02.raw 0
3 45000 4 2352 track03.bin 0
`))
	require.NoError(t, err)

	assert.True(t, sheet.IsGDROM, "a track at the high-density boundary means a GD-ROM")
	assert.Equal(t, []gdi.Track{
		{Number: 1, LBA: 0, TrackType: gdi.TrackTypeData, Filename: "track01.bin"},
		{Number: 2, LBA: 3000, TrackType: gdi.TrackTypeAudio, Filename: "track02.raw"},
		{Number: 3, LBA: 45000, TrackType: gdi.TrackTypeData, Filename: "track03.bin"},
	}, sheet.Tracks)
}

func TestParsePlainCD(t *testing.T) {
	t.Parallel()

	sheet, err := gdi.Parse(writeGDI(t, `1
1 0 4 2352 track01.bin 0
`))
	require.NoError(t, err)

	assert.False(t, sheet.IsGDROM)
	assert.Len(t, sheet.Tracks, 1)
}

// TestParseSkipsUnparseableLines pins the tolerance real .gdi files in the wild
// need: a stray line is skipped, not fatal.
func TestParseSkipsUnparseableLines(t *testing.T) {
	t.Parallel()

	sheet, err := gdi.Parse(writeGDI(t, `2

1 0 4 2352 track01.bin 0
this line is nonsense
x 0 4 2352 bad-number.bin 0
2 600 0 2352 track02.raw 0
`))
	require.NoError(t, err)

	assert.Len(t, sheet.Tracks, 2)
	assert.Equal(t, "track02.raw", sheet.Tracks[1].Filename)
}

func TestParseRejectsBadInput(t *testing.T) {
	t.Parallel()

	for name, content := range map[string]string{
		"not a gdi file":   "this is not a gdi file\nat all\n",
		"truncated fields": "1\n1 0 4 2352\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := gdi.Parse(writeGDI(t, content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "no tracks found")
		})
	}
}

func TestParseMissingFile(t *testing.T) {
	t.Parallel()

	_, err := gdi.Parse(filepath.Join(t.TempDir(), "nope.gdi"))
	require.Error(t, err)
}

func TestTrackCUEType(t *testing.T) {
	t.Parallel()

	assert.Equal(t, disc.TrackTypeAudio, gdi.Track{TrackType: gdi.TrackTypeAudio}.CUEType())
	assert.Equal(t, disc.TrackTypeMode1, gdi.Track{TrackType: gdi.TrackTypeData}.CUEType())
}

// GDI writers quote filenames containing spaces. Splitting on whitespace breaks
// those into several fields, and the extra ones push the line past the minimum
// count so the guard never notices.
func TestParseQuotedFilenames(t *testing.T) {
	t.Parallel()

	path := writeGDI(t, `2
1 0 4 2352 "my track 01.bin" 0
2 756 0 2352 track02.raw 0
`)

	sheet, err := gdi.Parse(path)
	require.NoError(t, err)
	require.Len(t, sheet.Tracks, 2)
	assert.Equal(t, "my track 01.bin", sheet.Tracks[0].Filename)
	assert.Equal(t, 0, sheet.Tracks[0].LBA)
	assert.Equal(t, "track02.raw", sheet.Tracks[1].Filename)
	assert.Equal(t, 756, sheet.Tracks[1].LBA)
}
