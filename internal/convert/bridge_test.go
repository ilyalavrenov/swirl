package convert

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilyalavrenov/swirl/internal/chd"
	"github.com/ilyalavrenov/swirl/internal/cue"
	"github.com/ilyalavrenov/swirl/internal/disc"
	"github.com/ilyalavrenov/swirl/internal/gdi"
)

// The bridge padding is arithmetic over track lengths, so it is checked against
// synthetic track lists: proving it end to end would mean encoding the 45000
// sectors before the high-density area, roughly 110 MB of compression to assert
// a subtraction.

// layoutDir writes one file per named track, each frames sectors long.
func layoutDir(t *testing.T, names []string, frames []int) string {
	t.Helper()

	dir := t.TempDir()
	for i, name := range names {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), make([]byte, frames[i]*disc.SectorBytes), 0o600))
	}

	return dir
}

func TestCueTrackLayoutBridge(t *testing.T) {
	t.Parallel()

	dir := layoutDir(t, []string{"a.bin", "b.bin", "c.bin"}, []int{450, 4, 100})

	// Rems[i] follows Files[i], so the marker after file 2 makes track 2 the bridge.
	sheet := &cue.Sheet{
		Files: []cue.File{
			{Name: "a.bin", Tracks: []cue.Track{{Number: 1, Type: disc.TrackTypeMode1}}},
			{Name: "b.bin", Tracks: []cue.Track{{Number: 2, Type: disc.TrackTypeAudio}}},
			{Name: "c.bin", Tracks: []cue.Track{{Number: 3, Type: disc.TrackTypeMode1}}},
		},
		Rems: []string{"", "HIGH-DENSITY AREA", ""},
	}

	l, err := cueTrackLayout(dir, sheet)
	require.NoError(t, err)
	assert.True(t, l.gdrom)
	assert.Equal(t, 1, l.bridge)
	assert.Equal(t, disc.HDAStartLBA, l.tracks[2].StartLBA, "track 3 opens the high-density area")
}

func TestCueTrackLayoutBridgeSkipsTracklessFile(t *testing.T) {
	t.Parallel()

	// A FILE with no TRACK contributes no track, so the bridge index must follow
	// the track list rather than the file list.
	dir := layoutDir(t, []string{"a.bin", "b.bin", "c.bin"}, []int{450, 4, 100})

	sheet := &cue.Sheet{
		Files: []cue.File{
			{Name: "a.bin", Tracks: []cue.Track{{Number: 1, Type: disc.TrackTypeMode1}}},
			{Name: "empty.bin"},
			{Name: "b.bin", Tracks: []cue.Track{{Number: 2, Type: disc.TrackTypeAudio}}},
			{Name: "c.bin", Tracks: []cue.Track{{Number: 3, Type: disc.TrackTypeMode1}}},
		},
		Rems: []string{"", "", "HIGH-DENSITY AREA", ""},
	}

	l, err := cueTrackLayout(dir, sheet)
	require.NoError(t, err)
	require.Len(t, l.tracks, 3)
	assert.Equal(t, 1, l.bridge, "the marker follows b.bin, which holds track 2")
	assert.Equal(t, 2, l.tracks[l.bridge].Number)
}

func TestGDITrackLayoutBridge(t *testing.T) {
	t.Parallel()

	dir := layoutDir(t, []string{"track01.bin", "track02.raw", "track03.bin"}, []int{450, 4, 100})

	// Track 3 opens the high-density area, so track 2 is the bridge.
	sheet := &gdi.Sheet{
		IsGDROM: true,
		Tracks: []gdi.Track{
			{Number: 1, LBA: 0, Filename: "track01.bin"},
			{Number: 2, LBA: 600, TrackType: gdi.TrackTypeAudio, Filename: "track02.raw"},
			{Number: 3, LBA: disc.HDAStartLBA, Filename: "track03.bin"},
		},
	}

	l, err := gdiTrackLayout(dir, sheet)
	require.NoError(t, err)
	assert.Equal(t, 1, l.bridge)
}

func TestGDITrackLayoutBridgeStopsAtFirstHDATrack(t *testing.T) {
	t.Parallel()

	dir := layoutDir(t, []string{"t1.bin", "t2.raw", "t3.bin", "t4.bin"}, []int{450, 4, 100, 100})

	// Tracks 3 and 4 both sit in the high-density area; the bridge is still track 2.
	sheet := &gdi.Sheet{
		IsGDROM: true,
		Tracks: []gdi.Track{
			{Number: 1, LBA: 0, Filename: "t1.bin"},
			{Number: 2, LBA: 600, TrackType: gdi.TrackTypeAudio, Filename: "t2.raw"},
			{Number: 3, LBA: disc.HDAStartLBA, Filename: "t3.bin"},
			{Number: 4, LBA: disc.HDAStartLBA + 100, Filename: "t4.bin"},
		},
	}

	l, err := gdiTrackLayout(dir, sheet)
	require.NoError(t, err)
	assert.Equal(t, 1, l.bridge)
}

func TestGDITrackLayoutWithoutBridge(t *testing.T) {
	t.Parallel()

	// The high-density area opening on track 1 leaves nothing to pad.
	dir := layoutDir(t, []string{"track01.bin"}, []int{100})
	sheet := &gdi.Sheet{
		IsGDROM: true,
		Tracks:  []gdi.Track{{Number: 1, LBA: disc.HDAStartLBA, Filename: "track01.bin"}},
	}

	l, err := gdiTrackLayout(dir, sheet)
	require.NoError(t, err)
	assert.Equal(t, -1, l.bridge)
}

func TestPadBridge(t *testing.T) {
	t.Parallel()

	// 450 real frames pad to 452, so the bridge has to cover the remainder.
	const t1Frames = 450

	tracks := []chd.Track{
		{Number: 1, Frames: t1Frames},
		{Number: 2, Frames: 4},
		{Number: 3, Frames: 100},
	}

	padBridge(tracks, 1)

	want := disc.HDAStartLBA - chd.PadFrames(t1Frames)
	assert.Equal(t, want, tracks[1].StoredFrames, "the bridge fills the gap up to the high-density boundary")
	assert.Zero(t, tracks[0].StoredFrames, "tracks either side are untouched")
	assert.Zero(t, tracks[2].StoredFrames)
}

func TestPadBridgeSkipsLongEnoughTrack(t *testing.T) {
	t.Parallel()

	tracks := []chd.Track{{Number: 1, Frames: 40000}, {Number: 2, Frames: 40000}}

	padBridge(tracks, 1)

	assert.Zero(t, tracks[1].StoredFrames, "no padding is added when the track already reaches the boundary")
}

func TestPadBridgeWithoutBridge(t *testing.T) {
	t.Parallel()

	tracks := []chd.Track{{Number: 1, Frames: 100}}

	padBridge(tracks, -1)

	assert.Zero(t, tracks[0].StoredFrames)
}

func TestCueSheetForGDROM(t *testing.T) {
	t.Parallel()

	// Tracks 1 and 2 fill exactly the sectors before the high-density area, so
	// track 3 opens it.
	got := cueSheetFor([]chd.TrackInfo{
		{Number: 1, CUEType: disc.TrackTypeMode1, TotalFrames: 12000, GDROM: true},
		{Number: 2, CUEType: disc.TrackTypeAudio, TotalFrames: disc.HDAStartLBA - 12000, GDROM: true},
		{Number: 3, CUEType: disc.TrackTypeMode1, TotalFrames: 500, GDROM: true},
	})

	require.Contains(t, got, hdaRem)
	assert.Equal(t, `FILE "track03.bin" BINARY`, got[len(got)-3], "the marker sits immediately before track 3")
	assert.Equal(t, hdaRem, got[len(got)-4])
}

func TestCueSheetForPlainCD(t *testing.T) {
	t.Parallel()

	// No CHGD tag, and the tracks never reach the boundary.
	got := cueSheetFor([]chd.TrackInfo{
		{Number: 1, CUEType: disc.TrackTypeMode1, TotalFrames: 8},
		{Number: 2, CUEType: disc.TrackTypeAudio, TotalFrames: 8},
		{Number: 3, CUEType: disc.TrackTypeAudio, TotalFrames: 8},
	})

	assert.NotContains(t, got, hdaRem, "an ordinary CD has no high-density area")
	assert.Equal(t, []string{
		`FILE "track01.bin" BINARY`, "  TRACK 01 MODE1/2352", "    INDEX 01 00:00:00",
		`FILE "track02.raw" BINARY`, "  TRACK 02 AUDIO", "    INDEX 01 00:00:00",
		`FILE "track03.raw" BINARY`, "  TRACK 03 AUDIO", "    INDEX 01 00:00:00",
	}, got)
}

// TestCueSheetForLongCDAtBoundary is the converse: an ordinary CD whose tracks
// land on the high-density sector still gets no marker, because the CHGD tag is
// what says the disc has a high-density area at all.
func TestCueSheetForLongCDAtBoundary(t *testing.T) {
	t.Parallel()

	got := cueSheetFor([]chd.TrackInfo{
		{Number: 1, CUEType: disc.TrackTypeMode1, TotalFrames: 20000},
		{Number: 2, CUEType: disc.TrackTypeAudio, TotalFrames: disc.HDAStartLBA - 20000},
		{Number: 3, CUEType: disc.TrackTypeAudio, TotalFrames: 500},
	})

	assert.NotContains(t, got, hdaRem)
}

// TestCueSheetForGDROMTagWithoutBoundary is the case a track-number heuristic got
// wrong: a GD-ROM whose tracks miss the boundary gets no marker rather than one
// before track 3.
func TestCueSheetForGDROMTagWithoutBoundary(t *testing.T) {
	t.Parallel()

	got := cueSheetFor([]chd.TrackInfo{
		{Number: 1, CUEType: disc.TrackTypeMode1, TotalFrames: 8, GDROM: true},
		{Number: 2, CUEType: disc.TrackTypeAudio, TotalFrames: 8, GDROM: true},
		{Number: 3, CUEType: disc.TrackTypeMode1, TotalFrames: 8, GDROM: true},
	})

	assert.NotContains(t, got, hdaRem)
}
