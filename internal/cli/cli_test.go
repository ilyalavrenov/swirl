package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilyalavrenov/swirl/internal/convert"
	"github.com/ilyalavrenov/swirl/internal/disc"
)

// run captures output; no bars are drawn because the writer is not a terminal.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()

	out := &bytes.Buffer{}

	cmd := command("test")
	cmd.Writer = out
	cmd.ErrWriter = out

	err := cmd.Run(t.Context(), append([]string{"swirl"}, args...))

	return out.String(), err
}

// testProductName is the IP.BIN product name every fixture disc carries.
const testProductName = "TEST DISC"

func discImage(t *testing.T, name string) string {
	t.Helper()

	dir := t.TempDir()

	// A data track long enough to hold an IP.BIN product name.
	track := make([]byte, 2*disc.SectorBytes)
	copy(track[0x90:], name)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "track01.bin"), track, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "disc.cue"), []byte(`FILE "track01.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
`), 0o600))

	return dir
}

func TestConvertWritesGDIAndName(t *testing.T) {
	t.Parallel()

	dir := discImage(t, testProductName)
	outputDir := filepath.Join(t.TempDir(), "out")

	out, err := run(t, "convert", filepath.Join(dir, "disc.cue"), outputDir)
	require.NoError(t, err)

	assert.Contains(t, out, "Converting disc.cue")
	assert.Contains(t, out, "(1 track · ", "one track is reported in the singular")
	assert.Contains(t, out, "Name: "+testProductName)

	assert.FileExists(t, filepath.Join(outputDir, "disc.gdi"))
	assert.FileExists(t, filepath.Join(outputDir, "track01.bin"))

	got, err := os.ReadFile(filepath.Join(outputDir, "name.txt"))
	require.NoError(t, err)
	assert.Equal(t, testProductName, string(got))
}

func TestConvertFindsImageInDirectory(t *testing.T) {
	t.Parallel()

	dir := discImage(t, testProductName)

	out, err := run(t, "convert", dir, filepath.Join(t.TempDir(), "out"))
	require.NoError(t, err)
	assert.Contains(t, out, "Converting disc.cue")
}

func TestConvertRejectsBadArguments(t *testing.T) {
	t.Parallel()

	dir := discImage(t, testProductName)

	// Absolute: a relative output path would litter the package directory if a case
	// ever stopped failing.
	out := filepath.Join(t.TempDir(), "out")

	for name, test := range map[string]struct {
		args []string
		want string
	}{
		"missing output":    {[]string{"convert", filepath.Join(dir, "disc.cue")}, "are required"},
		"no arguments":      {[]string{"convert"}, "are required"},
		"unknown format":    {[]string{"convert", filepath.Join(dir, "disc.cue"), out, "--to", "iso"}, "unknown format"},
		"same format":       {[]string{"convert", filepath.Join(dir, "disc.cue"), out, "--to", "cue"}, "nothing to do"},
		"unreadable input":  {[]string{"convert", filepath.Join(dir, "nope.cue"), out}, "no such file"},
		"unsupported input": {[]string{"convert", filepath.Join(dir, "track01.bin"), out}, "unknown format"},
		"unknown codec": {
			[]string{"convert", filepath.Join(dir, "disc.cue"), out, "--to", "chd", "--codec", "cdxx"},
			`unknown codec "cdxx"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := run(t, test.args...)
			require.Error(t, err)

			if test.want != "" {
				assert.Contains(t, err.Error(), test.want)
			}
		})
	}
}

// TestConvertRefusesToClobber is the CLI half of the output-directory guard.
func TestConvertRefusesToClobber(t *testing.T) {
	t.Parallel()

	dir := discImage(t, testProductName)
	outputDir := t.TempDir()
	precious := filepath.Join(outputDir, "precious.txt")
	require.NoError(t, os.WriteFile(precious, []byte("keep me"), 0o600))

	_, err := run(t, "convert", filepath.Join(dir, "disc.cue"), outputDir)
	require.Error(t, err)
	assert.FileExists(t, precious)

	_, err = run(t, "convert", "--force", filepath.Join(dir, "disc.cue"), outputDir)
	require.NoError(t, err)
	assert.NoFileExists(t, precious)
}

// TestNewBarsNeedsTerminal covers the gate that keeps carriage returns and escape
// codes out of a redirected log.
func TestNewBarsNeedsTerminal(t *testing.T) {
	t.Parallel()

	cmd := command("test")

	cmd.Writer = &bytes.Buffer{}
	assert.Nil(t, newBars(cmd), "a buffer is not a terminal")

	f, err := os.CreateTemp(t.TempDir(), "out")
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })

	cmd.Writer = f
	assert.Nil(t, newBars(cmd), "a redirected file is not a terminal")
}

// TestWriteGameNameUsesTrackOne pins selection to the track number rather than its
// position, hence the out-of-order GDI below: only track 1 holds the IP.BIN
// header.
func TestWriteGameNameUsesTrackOne(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for name, product := range map[string]string{"track01.bin": "REAL NAME", "track03.bin": "WRONG TRACK"} {
		track := make([]byte, 2*disc.SectorBytes)
		copy(track[0x90:], product)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), track, 0o600))
	}

	require.NoError(t, os.WriteFile(filepath.Join(dir, "disc.gdi"), []byte(`2
3 45000 4 2352 track03.bin 0
1 0 4 2352 track01.bin 0
`), 0o600))

	got, err := writeGameName(filepath.Join(dir, "disc.gdi"))
	require.NoError(t, err)
	assert.Equal(t, "REAL NAME", got)
}

func TestWriteGameNameWithoutTrackOne(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "track02.raw"), make([]byte, disc.SectorBytes), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "disc.gdi"),
		[]byte("1\n2 600 0 2352 track02.raw 0\n"), 0o600))

	_, err := writeGameName(filepath.Join(dir, "disc.gdi"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no track 1")
}

func TestDetectOutput(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		path, flag string
		want       convert.Format
		wantErr    bool
	}{
		"chd extension":    {path: "/out/disc.chd", want: convert.FormatCHD},
		"cue extension":    {path: "/out/disc.cue", want: convert.FormatCUE},
		"gdi extension":    {path: "/out/disc.gdi", want: convert.FormatGDI},
		"directory":        {path: "/out", want: convert.FormatGDI},
		"unknown suffix":   {path: "/out/disc.iso", want: convert.FormatGDI},
		"flag wins":        {path: "/out/disc.chd", flag: "cue", want: convert.FormatCUE},
		"flag is anycase":  {path: "/out", flag: "CHD", want: convert.FormatCHD},
		"flag is rejected": {path: "/out", flag: "iso", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := detectOutput(test.path, test.flag)
			if test.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestFindByExt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	require.NoError(t, os.Mkdir(nested, 0o755))

	for _, name := range []string{"a.gdi", "b.GDI", "c.cue", "nested/d.gdi"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}

	found, err := findByExt(dir, ".gdi")
	require.NoError(t, err)
	assert.Equal(t, []string{
		filepath.Join(dir, "a.gdi"),
		filepath.Join(dir, "b.GDI"),
		filepath.Join(nested, "d.gdi"),
	}, found, "the search is recursive and case insensitive")

	_, err = findByExt(filepath.Join(dir, "nope"), ".gdi")
	require.Error(t, err)
}

func TestFormatBytes(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		n    int64
		want string
	}{
		"bytes":     {512, "512 B"},
		"kilobytes": {2048, "2.0 KiB"},
		"megabytes": {5 * 1024 * 1024, "5.0 MiB"},
		"gigabytes": {3 * 1024 * 1024 * 1024, "3.00 GiB"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, formatBytes(test.n))
		})
	}
}

func TestInfoCommand(t *testing.T) {
	t.Parallel()

	dir := discImage(t, testProductName)

	out, err := run(t, "info", filepath.Join(dir, "disc.cue"))
	require.NoError(t, err)

	assert.Contains(t, out, "name    "+testProductName)
	assert.Contains(t, out, "format  cue")
	assert.Contains(t, out, "layout  CD")
	assert.Contains(t, out, "1  MODE1/2352  0")
	assert.NotContains(t, out, "codec", "codec and hunks are CHD only")
}

func TestInfoCHD(t *testing.T) {
	t.Parallel()

	dir := discImage(t, testProductName)
	chdPath := filepath.Join(t.TempDir(), "disc.chd")

	_, err := run(t, "convert", filepath.Join(dir, "disc.cue"), chdPath)
	require.NoError(t, err)

	out, err := run(t, "info", chdPath)
	require.NoError(t, err)

	assert.Contains(t, out, "name    "+testProductName, "read out of the archive, not from a track file")
	assert.Contains(t, out, "format  chd")
	assert.Contains(t, out, "codec   cdzl")
	assert.Contains(t, out, "stored  ")
	assert.NotContains(t, out, "FILE", "a CHD keeps its tracks in one archive")
}

func TestVerifyCommand(t *testing.T) {
	t.Parallel()

	dir := discImage(t, testProductName)
	chdPath := filepath.Join(t.TempDir(), "disc.chd")

	_, err := run(t, "convert", filepath.Join(dir, "disc.cue"), chdPath)
	require.NoError(t, err)

	out, err := run(t, "verify", chdPath)
	require.NoError(t, err)

	assert.Contains(t, out, "OK: ")
	assert.Contains(t, out, "raw SHA1")
	assert.Contains(t, out, "combined SHA1")
}

func TestVerifyRejectsNonCHDInput(t *testing.T) {
	t.Parallel()

	dir := discImage(t, testProductName)

	_, err := run(t, "verify", filepath.Join(dir, "disc.cue"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "want chd")
}

func TestInspectRejectsBadArguments(t *testing.T) {
	t.Parallel()

	for name, args := range map[string][]string{
		"info without a path":   {"info"},
		"verify without a path": {"verify"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := run(t, args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "<path> is required")
		})
	}
}

// TestInfoWithoutName covers a disc whose track 1 holds no IP.BIN header: the
// line is left out rather than printed empty.
func TestInfoWithoutName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "track01.bin"),
		make([]byte, 2*disc.SectorBytes), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "disc.gdi"),
		[]byte("1\n1 0 4 2352 track01.bin 0\n"), 0o600))

	out, err := run(t, "info", filepath.Join(dir, "disc.gdi"))
	require.NoError(t, err)

	assert.NotContains(t, out, "name")
	assert.Contains(t, out, "format  gdi")
}
