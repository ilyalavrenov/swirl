package convert_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilyalavrenov/swirl/internal/chd"
	"github.com/ilyalavrenov/swirl/internal/convert"
	"github.com/ilyalavrenov/swirl/internal/disc"
)

type image struct {
	t   *testing.T
	dir string
}

func newImage(t *testing.T) *image {
	t.Helper()

	return &image{t: t, dir: t.TempDir()}
}

func (i *image) write(name string, data []byte) string {
	i.t.Helper()

	path := filepath.Join(i.dir, name)
	require.NoError(i.t, os.WriteFile(path, data, 0o600))

	return path
}

func (i *image) track(name string, sectors int, fill byte) *image {
	i.write(name, bytes.Repeat([]byte{fill}, sectors*disc.SectorBytes))

	return i
}

// namedTrack writes a track whose IP.BIN header sits after pregap sectors of the
// previous track's run-out.
func (i *image) namedTrack(name string, sectors, pregap int, product string) *image {
	const nameOffset = 0x90

	data := make([]byte, sectors*disc.SectorBytes)
	copy(data[pregap*disc.SectorBytes+nameOffset:], product)
	i.write(name, data)

	return i
}

func (i *image) sheet(name, content string) string {
	return i.write(name, []byte(content))
}

func lines(t *testing.T, path string) []string {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func TestCueToGDITwoTracks(t *testing.T) {
	t.Parallel()

	img := newImage(t).
		track("track01.bin", 300, 0x00).
		track("track02.raw", 100, 0xFF)

	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "track02.raw" AUDIO
  TRACK 02 AUDIO
    INDEX 01 00:00:00
`)

	outputDir := filepath.Join(t.TempDir(), "out")

	result, err := convert.Run(t.Context(), convert.FormatCUE, convert.FormatGDI, cuePath, outputDir, convert.Options{})
	require.NoError(t, err)

	assert.Equal(t, 2, result.Tracks)
	assert.Equal(t, int64(400*disc.SectorBytes), result.Bytes)

	assert.Equal(t, []string{
		"2",
		"1 0 4 2352 track01.bin 0 ",
		"2 300 0 2352 track02.raw 0 ",
	}, lines(t, filepath.Join(outputDir, "disc.gdi")))

	for _, name := range []string{"track01.bin", "track02.raw"} {
		assert.FileExists(t, filepath.Join(outputDir, name))
	}
}

// TestCueToGDIHighDensityArea checks that tracks after a REM HIGH-DENSITY AREA
// start at the fixed boundary rather than after the previous track.
func TestCueToGDIHighDensityArea(t *testing.T) {
	t.Parallel()

	img := newImage(t).
		track("track01.bin", 3000, 0x00).
		track("track02.raw", 1000, 0xAA).
		track("track03.bin", 500, 0xBB)

	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "track02.raw" AUDIO
  TRACK 02 AUDIO
    INDEX 01 00:00:00
REM HIGH-DENSITY AREA
FILE "track03.bin" BINARY
  TRACK 03 MODE1/2352
    INDEX 01 00:00:00
`)

	outputDir := filepath.Join(t.TempDir(), "out")

	_, err := convert.Run(t.Context(), convert.FormatCUE, convert.FormatGDI, cuePath, outputDir, convert.Options{})
	require.NoError(t, err)

	got := lines(t, filepath.Join(outputDir, "disc.gdi"))
	assert.Equal(t, "3 45000 4 2352 track03.bin 0 ", got[3])
}

// A CHD stores a track's whole file, pregap included, so its frame count has to
// come out larger than the GDI one for the same track.
func TestCueToCHDKeepsPregapFrames(t *testing.T) {
	t.Parallel()

	const (
		realFrames = 50
		pregap     = 150
	)

	img := newImage(t).
		track("track01.bin", 300, 0x00).
		track("track02.raw", realFrames+pregap, 0xFF)

	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "track02.raw" AUDIO
  TRACK 02 AUDIO
    INDEX 00 00:00:00
    INDEX 01 00:02:00
`)

	chdPath := filepath.Join(t.TempDir(), "disc.chd")
	_, err := convert.Run(t.Context(), convert.FormatCUE, convert.FormatCHD, cuePath, chdPath, convert.Options{})
	require.NoError(t, err)

	info, err := chd.Stat(chdPath)
	require.NoError(t, err)
	require.Len(t, info.Tracks, 2)
	assert.Equal(t, realFrames+pregap, info.Tracks[1].RealFrames, "the pregap is stored, not stripped")
}

// TestCueToGDIStripsPregap checks that the lead-in before INDEX 01 is dropped from
// the track file but still counted in the next track's start sector.
func TestCueToGDIStripsPregap(t *testing.T) {
	t.Parallel()

	img := newImage(t).
		track("track01.bin", 300, 0x00).
		track("track02.raw", 200, 0xFF).
		track("track03.bin", 100, 0xAA)

	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "track02.raw" AUDIO
  TRACK 02 AUDIO
    INDEX 00 00:00:00
    INDEX 01 00:02:00
FILE "track03.bin" BINARY
  TRACK 03 MODE1/2352
    INDEX 01 00:00:00
`)

	outputDir := filepath.Join(t.TempDir(), "out")

	_, err := convert.Run(t.Context(), convert.FormatCUE, convert.FormatGDI, cuePath, outputDir, convert.Options{})
	require.NoError(t, err)

	got := lines(t, filepath.Join(outputDir, "disc.gdi"))
	assert.Equal(t, "2 450 0 2352 track02.raw 0 ", got[2], "150 pregap frames past track 1's 300")
	assert.Equal(t, "3 500 4 2352 track03.bin 0 ", got[3], "track 2 contributes its 50 real frames, not all 200")

	info, err := os.Stat(filepath.Join(outputDir, "track02.raw"))
	require.NoError(t, err)
	assert.Equal(t, int64(50*disc.SectorBytes), info.Size(), "the 150 pregap sectors are not copied")
}

func TestGDIToCUE(t *testing.T) {
	t.Parallel()

	img := newImage(t).
		track("track01.bin", 100, 0x01).
		track("track02.raw", 50, 0x02).
		track("track03.bin", 200, 0x03)

	gdiPath := img.sheet("disc.gdi", `3
1 0 4 2352 track01.bin 0
2 600 0 2352 track02.raw 0
3 45000 4 2352 track03.bin 0
`)

	outputDir := filepath.Join(t.TempDir(), "out")

	result, err := convert.Run(t.Context(), convert.FormatGDI, convert.FormatCUE, gdiPath, outputDir, convert.Options{})
	require.NoError(t, err)
	assert.Equal(t, 3, result.Tracks)

	assert.Equal(t, []string{
		`FILE "track01.bin" BINARY`,
		"  TRACK 01 MODE1/2352",
		"    INDEX 01 00:00:00",
		`FILE "track02.raw" AUDIO`,
		"  TRACK 02 AUDIO",
		"    INDEX 01 00:00:00",
		"REM HIGH-DENSITY AREA",
		`FILE "track03.bin" BINARY`,
		"  TRACK 03 MODE1/2352",
		"    INDEX 01 00:00:00",
	}, lines(t, filepath.Join(outputDir, "disc.cue")))
}

func TestCUEToCHDToCUE(t *testing.T) {
	t.Parallel()

	img := newImage(t).
		track("track01.bin", 12, 0x11).
		track("track02.raw", 8, 0x22)

	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "track02.raw" AUDIO
  TRACK 02 AUDIO
    INDEX 01 00:00:00
`)

	chdPath := filepath.Join(t.TempDir(), "disc.chd")

	_, err := convert.Run(t.Context(), convert.FormatCUE, convert.FormatCHD, cuePath, chdPath, convert.Options{})
	require.NoError(t, err)
	assert.FileExists(t, chdPath)

	outputDir := filepath.Join(t.TempDir(), "restored")

	result, err := convert.Run(t.Context(), convert.FormatCHD, convert.FormatCUE, chdPath, outputDir, convert.Options{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.Tracks)

	for name, want := range map[string][]byte{
		"track01.bin": bytes.Repeat([]byte{0x11}, 12*disc.SectorBytes),
		"track02.raw": bytes.Repeat([]byte{0x22}, 8*disc.SectorBytes),
	} {
		got, err := os.ReadFile(filepath.Join(outputDir, name))
		require.NoError(t, err)
		assert.Equal(t, want, got, "%s survived the round trip", name)
	}
}

func TestGDIToCHD(t *testing.T) {
	t.Parallel()

	img := newImage(t).
		track("track01.bin", 8, 0x11).
		track("track02.raw", 8, 0x22)

	gdiPath := img.sheet("disc.gdi", `2
1 0 4 2352 track01.bin 0
2 8 0 2352 track02.raw 0
`)

	chdPath := filepath.Join(t.TempDir(), "disc.chd")

	result, err := convert.Run(t.Context(), convert.FormatGDI, convert.FormatCHD, gdiPath, chdPath, convert.Options{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.Tracks)

	outputDir := t.TempDir()

	restored, err := convert.Run(t.Context(), convert.FormatCHD, convert.FormatCUE, chdPath, outputDir, convert.Options{})
	require.NoError(t, err)
	assert.Equal(t, 2, restored.Tracks)

	got, err := os.ReadFile(filepath.Join(outputDir, "track02.raw"))
	require.NoError(t, err)
	assert.Equal(t, bytes.Repeat([]byte{0x22}, 8*disc.SectorBytes), got)
}

// TestChdToCueNoHighDensityMarker is a regression test: an ordinary CD must not
// get a GD-ROM high-density marker just because it has three tracks.
func TestChdToCueNoHighDensityMarker(t *testing.T) {
	t.Parallel()

	img := newImage(t).
		track("track01.bin", 8, 0x01).
		track("track02.raw", 8, 0x02).
		track("track03.raw", 8, 0x03)

	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "track02.raw" AUDIO
  TRACK 02 AUDIO
    INDEX 01 00:00:00
FILE "track03.raw" AUDIO
  TRACK 03 AUDIO
    INDEX 01 00:00:00
`)

	chdPath := filepath.Join(t.TempDir(), "disc.chd")
	_, err := convert.Run(t.Context(), convert.FormatCUE, convert.FormatCHD, cuePath, chdPath, convert.Options{})
	require.NoError(t, err)

	outputDir := filepath.Join(t.TempDir(), "restored")
	_, err = convert.Run(t.Context(), convert.FormatCHD, convert.FormatCUE, chdPath, outputDir, convert.Options{})
	require.NoError(t, err)

	got := lines(t, filepath.Join(outputDir, "disc.cue"))
	assert.NotContains(t, got, "REM HIGH-DENSITY AREA", "a plain CD has no high-density area")
}

// TestRunRefusesNonEmptyOutputDir is a safety regression test: conversion replaces
// the whole output directory, so it must never do so by accident.
func TestRunRefusesNonEmptyOutputDir(t *testing.T) {
	t.Parallel()

	img := newImage(t).track("track01.bin", 10, 0x00)
	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
`)

	outputDir := t.TempDir()
	precious := filepath.Join(outputDir, "precious.txt")
	require.NoError(t, os.WriteFile(precious, []byte("do not delete"), 0o600))

	_, err := convert.Run(t.Context(), convert.FormatCUE, convert.FormatGDI, cuePath, outputDir, convert.Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--force")
	assert.FileExists(t, precious, "the existing file must survive a refused conversion")

	_, err = convert.Run(t.Context(), convert.FormatCUE, convert.FormatGDI, cuePath, outputDir,
		convert.Options{Force: true})
	require.NoError(t, err)
	assert.NoFileExists(t, precious, "--force replaces the directory")
}

func TestRunRefusesExistingOutputFile(t *testing.T) {
	t.Parallel()

	img := newImage(t).track("track01.bin", 8, 0x00)
	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
`)

	chdPath := filepath.Join(t.TempDir(), "disc.chd")
	require.NoError(t, os.WriteFile(chdPath, []byte("existing"), 0o600))

	_, err := convert.Run(t.Context(), convert.FormatCUE, convert.FormatCHD, cuePath, chdPath, convert.Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--force")

	_, err = convert.Run(t.Context(), convert.FormatCUE, convert.FormatCHD, cuePath, chdPath,
		convert.Options{Force: true})
	require.NoError(t, err)
}

func TestRunUnsupportedConversion(t *testing.T) {
	t.Parallel()

	_, err := convert.Run(t.Context(), convert.FormatCHD, convert.FormatGDI, "in.chd", t.TempDir(), convert.Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
}

// TestRunReportsProgress checks that every step is announced before it runs and
// that the reported bytes add up to what was copied.
func TestRunReportsProgress(t *testing.T) {
	t.Parallel()

	img := newImage(t).
		track("track01.bin", 20, 0x01).
		track("track02.raw", 10, 0x02)

	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "track02.raw" AUDIO
  TRACK 02 AUDIO
    INDEX 01 00:00:00
`)

	var steps []convert.Step

	sink := &bytes.Buffer{}

	_, err := convert.Run(t.Context(), convert.FormatCUE, convert.FormatGDI, cuePath,
		filepath.Join(t.TempDir(), "out"), convert.Options{
			Progress: func(s convert.Step) io.Writer {
				steps = append(steps, s)

				return sink
			},
		})
	require.NoError(t, err)

	assert.Equal(t, []convert.Step{
		{Index: 1, Total: 2, Name: "track01.bin", Audio: false, Bytes: 20 * disc.SectorBytes},
		{Index: 2, Total: 2, Name: "track02.raw", Audio: true, Bytes: 10 * disc.SectorBytes},
	}, steps)

	assert.Equal(t, 30*disc.SectorBytes, sink.Len(), "every copied byte reaches the progress writer")
}

// TestRunFailureKeepsOutputDir is a safety regression test:
// --force is set, so the existing contents survive only because the new ones are
// staged elsewhere until the conversion has finished.
func TestRunFailureKeepsOutputDir(t *testing.T) {
	t.Parallel()

	img := newImage(t).track("track01.bin", 8, 0x01)

	// A directory stats like a file but cannot be read, so the copy fails after
	// track 1 has already been written.
	require.NoError(t, os.Mkdir(filepath.Join(img.dir, "track02.raw"), 0o755))

	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "track02.raw" AUDIO
  TRACK 02 AUDIO
    INDEX 01 00:00:00
`)

	parent := t.TempDir()
	outputDir := filepath.Join(parent, "out")
	require.NoError(t, os.Mkdir(outputDir, 0o755))

	precious := filepath.Join(outputDir, "precious.txt")
	require.NoError(t, os.WriteFile(precious, []byte("keep me"), 0o600))

	_, err := convert.Run(t.Context(), convert.FormatCUE, convert.FormatGDI, cuePath, outputDir,
		convert.Options{Force: true})
	require.Error(t, err)

	assert.FileExists(t, precious, "a failed conversion must not have deleted anything")

	entries, err := os.ReadDir(outputDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "and must not have left a partial conversion behind")

	siblings, err := os.ReadDir(parent)
	require.NoError(t, err)
	assert.Len(t, siblings, 1, "and must not have left its staging directory behind")
}

// TestDescribeAgreesAcrossFormats pins what makes swirl info trustworthy: one
// disc reports one track layout whichever format holds it, because a conversion
// and a listing walk the same LBAs.
func TestDescribeAgreesAcrossFormats(t *testing.T) {
	t.Parallel()

	img := newImage(t).
		track("track01.bin", 12, 0x01).
		track("track02.raw", 8, 0x02)

	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "track02.raw" AUDIO
  TRACK 02 AUDIO
    INDEX 01 00:00:00
`)

	gdiDir := filepath.Join(t.TempDir(), "gdi")
	_, err := convert.Run(t.Context(), convert.FormatCUE, convert.FormatGDI, cuePath, gdiDir, convert.Options{})
	require.NoError(t, err)

	gdiPath := filepath.Join(gdiDir, "disc.gdi")
	chdPath := filepath.Join(t.TempDir(), "disc.chd")
	_, err = convert.Run(t.Context(), convert.FormatGDI, convert.FormatCHD, gdiPath, chdPath, convert.Options{})
	require.NoError(t, err)

	want := []convert.TrackDesc{
		{Number: 1, Type: disc.TrackTypeMode1, StartLBA: 0, Frames: 12},
		{Number: 2, Type: disc.TrackTypeAudio, StartLBA: 12, Frames: 8},
	}

	for name, test := range map[string]struct {
		format convert.Format
		path   string
	}{
		"cue": {convert.FormatCUE, cuePath},
		"gdi": {convert.FormatGDI, gdiPath},
		"chd": {convert.FormatCHD, chdPath},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := convert.Describe(test.format, test.path)
			require.NoError(t, err)

			assert.Equal(t, test.format, got.Format)
			assert.False(t, got.GDROM, "a two track CD has no high-density area")

			layout := make([]convert.TrackDesc, 0, len(got.Tracks))
			for _, tr := range got.Tracks {
				layout = append(layout, convert.TrackDesc{
					Number: tr.Number, Type: tr.Type, StartLBA: tr.StartLBA, Frames: tr.Frames,
				})
			}

			assert.Equal(t, want, layout)
			assert.Equal(t, int64(20*disc.SectorBytes), got.Bytes())
		})
	}
}

func TestDescribeGDROMLayout(t *testing.T) {
	t.Parallel()

	img := newImage(t).
		track("track01.bin", 12, 0x01).
		track("track02.raw", 8, 0x02).
		track("track03.bin", 4, 0x03)

	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "track02.raw" AUDIO
  TRACK 02 AUDIO
    INDEX 01 00:00:00
REM HIGH-DENSITY AREA
FILE "track03.bin" BINARY
  TRACK 03 MODE1/2352
    INDEX 01 00:00:00
`)

	got, err := convert.Describe(convert.FormatCUE, cuePath)
	require.NoError(t, err)

	assert.True(t, got.GDROM)
	require.Len(t, got.Tracks, 3)
	assert.Equal(t, disc.HDAStartLBA, got.Tracks[2].StartLBA, "the marker pushes track 3 to the boundary")
}

func TestDescribeUnsupportedFormat(t *testing.T) {
	t.Parallel()

	_, err := convert.Describe(convert.Format("iso"), "disc.iso")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot describe")
}

// TestDescribeCHDCountsPadFrames pins that a listing advances past a bridge
// track's padding, not just its data: a GD-ROM's later tracks are addressed from
// the sectors allocated, not the sectors filled.
func TestDescribeCHDCountsPadFrames(t *testing.T) {
	t.Parallel()

	sectors := func(fill byte) *bytes.Reader {
		return bytes.NewReader(bytes.Repeat([]byte{fill}, 4*disc.SectorBytes))
	}

	path := filepath.Join(t.TempDir(), "disc.chd")
	require.NoError(t, chd.Write(t.Context(), path, []chd.Track{
		{Number: 1, Type: disc.TrackTypeMode1, Frames: 4, Data: sectors(0x01)},
		{Number: 2, Type: disc.TrackTypeAudio, Frames: 4, StoredFrames: 16, Data: sectors(0x02)},
		{Number: 3, Type: disc.TrackTypeMode1, Frames: 4, Data: sectors(0x03)},
	}, nil))

	got, err := convert.Describe(convert.FormatCHD, path)
	require.NoError(t, err)

	assert.True(t, got.GDROM, "bridge padding means a GD-ROM")
	require.Len(t, got.Tracks, 3)
	assert.Equal(t, 4, got.Tracks[1].Frames, "the bridge track holds four sectors of data")
	assert.Equal(t, 20, got.Tracks[2].StartLBA, "track 3 starts past the padding, not past the data")
}

// TestDescribeNameFromTrackOne pins selection to the track number rather
// than its position: the sheet below lists track 3 first, and only track 1 has an
// IP.BIN header to read.
func TestDescribeNameFromTrackOne(t *testing.T) {
	t.Parallel()

	img := newImage(t).
		namedTrack("track01.bin", 2, 0, "REAL NAME").
		namedTrack("track03.bin", 2, 0, "WRONG TRACK")

	gdiPath := img.sheet("disc.gdi", "2\n3 45000 4 2352 track03.bin 0\n1 0 4 2352 track01.bin 0\n")

	got, err := convert.Describe(convert.FormatGDI, gdiPath)
	require.NoError(t, err)
	assert.Equal(t, "REAL NAME", got.Name)
}

// TestDescribeNamePastPregap covers a track 1 that opens with 150
// sectors of run-out: IP.BIN starts after it, not at the head of the file.
func TestDescribeNamePastPregap(t *testing.T) {
	t.Parallel()

	const pregap = 150

	img := newImage(t).namedTrack("track01.bin", pregap+2, pregap, "PREGAPPED")

	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 00 00:00:00
    INDEX 01 00:02:00
`)

	got, err := convert.Describe(convert.FormatCUE, cuePath)
	require.NoError(t, err)
	assert.Equal(t, "PREGAPPED", got.Name)
}

func TestDescribeWithoutName(t *testing.T) {
	t.Parallel()

	img := newImage(t).track("track01.bin", 2, 0x00)
	cuePath := img.sheet("disc.cue", `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
`)

	got, err := convert.Describe(convert.FormatCUE, cuePath)
	require.NoError(t, err)
	assert.Empty(t, got.Name, "a blank track 1 has no name, which is not an error")
}

// TestDescribeWithoutTrackOne covers a sheet that numbers its tracks from 2.
// There is then no track 1 to read a name out of, and looking anyway would index
// past the front of the slice.
func TestDescribeWithoutTrackOne(t *testing.T) {
	t.Parallel()

	img := newImage(t).namedTrack("track02.raw", 2, 0, "NOT TRACK ONE")
	gdiPath := img.sheet("disc.gdi", "1\n2 600 0 2352 track02.raw 0\n")

	got, err := convert.Describe(convert.FormatGDI, gdiPath)
	require.NoError(t, err)

	assert.Empty(t, got.Name, "no track 1 means no name, not a crash")
	require.Len(t, got.Tracks, 1)
	assert.Equal(t, 2, got.Tracks[0].Number)
}
