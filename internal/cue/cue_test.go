package cue

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeCue(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "disc.cue")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func TestMSFToFrames(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		msf  MSF
		want int
	}{
		"zero":       {MSF{0, 0, 0}, 0},
		"one frame":  {MSF{0, 0, 1}, 1},
		"one second": {MSF{0, 1, 0}, 75},
		"one minute": {MSF{1, 0, 0}, 4500},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, MSFToFrames(test.msf))
		})
	}
}

func TestParseMSF(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		in      string
		want    MSF
		wantErr bool
	}{
		"fields in order":  {in: "01:02:03", want: MSF{1, 2, 3}},
		"wrong part count": {in: "01:02", wantErr: true},
		"bad minutes":      {in: "aa:bb:cc", wantErr: true},
		"bad frames":       {in: "01:02:xx", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := parseMSF(test.in)
			if test.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.want, got, "parseMSF(%q)", test.in)
		})
	}
}

func TestExtractQuotedString(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct{ in, want string }{
		"quoted name":  {`FILE "my game (japan).bin" BINARY`, "my game (japan).bin"},
		"no quotes":    {`FILE track01.bin BINARY`, ""},
		"one quote":    {`FILE "track01.bin BINARY`, ""},
		"empty quotes": {`FILE "" BINARY`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, extractQuotedString(test.in))
		})
	}
}

func TestParseSingleDataTrack(t *testing.T) {
	t.Parallel()

	sheet, err := Parse(writeCue(t, `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
`))
	require.NoError(t, err)
	require.Len(t, sheet.Files, 1)

	assert.Equal(t, File{
		Name:   "track01.bin",
		Tracks: []Track{{Number: 1, Type: "MODE1/2352", Indexes: []MSF{{}}}},
	}, sheet.Files[0])
}

func TestParseAudioTrackWithPregap(t *testing.T) {
	t.Parallel()

	sheet, err := Parse(writeCue(t, `FILE "track02.raw" AUDIO
  TRACK 02 AUDIO
    INDEX 00 00:00:00
    INDEX 01 00:02:00
`))
	require.NoError(t, err)

	assert.Equal(t, Track{
		Number:  2,
		Type:    "AUDIO",
		Indexes: []MSF{{}, {Sec: 2}},
	}, sheet.Files[0].Tracks[0])
}

// TestParseHighDensityAreaRem checks that a REM lands against the FILE it follows,
// which is how the high-density boundary is located later.
func TestParseHighDensityAreaRem(t *testing.T) {
	t.Parallel()

	sheet, err := Parse(writeCue(t, `FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
FILE "track02.raw" AUDIO
  TRACK 02 AUDIO
    INDEX 01 00:00:00
REM HIGH-DENSITY AREA
FILE "track03.bin" BINARY
  TRACK 03 MODE1/2352
    INDEX 01 00:00:00
`))
	require.NoError(t, err)

	assert.Len(t, sheet.Files, 3)
	assert.Equal(t, []string{"", "HIGH-DENSITY AREA", ""}, sheet.Rems)
	assert.Equal(t, 3, sheet.TrackCount())
}

func TestParseRejectsBadInput(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct{ content, want string }{
		"not a cue sheet": {"this is not a cue sheet\nat all\n", "no tracks found"},
		"empty file":      {"", "no tracks found"},
		"file but no track": {`FILE "track01.bin" BINARY
`, "no tracks found"},
		"bad track number": {`FILE "track01.bin" BINARY
  TRACK xx MODE1/2352
`, "invalid track number"},
		"bad index time": {`FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 not-a-timecode
`, "invalid MSF"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(writeCue(t, test.content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestParseMissingFile(t *testing.T) {
	t.Parallel()

	_, err := Parse(filepath.Join(t.TempDir(), "nope.cue"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open cue")
}

// A timecode is base 75 and unsigned. Without the range check an out-of-range
// pregap parses cleanly and silently skips the wrong number of sectors.
func TestParseMSFRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]string{
		"seconds past 59": "00:60:00",
		"frames past 74":  "00:00:75",
		"negative minute": "-1:00:00",
		"negative frame":  "00:00:-1",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := writeAndParseIndex(t, in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "out of range")
		})
	}
}

func writeAndParseIndex(t *testing.T, timecode string) (*Sheet, error) {
	t.Helper()

	return Parse(writeCue(t, `FILE "t.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 `+timecode+`
`))
}
