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

// TestInfoWithoutName covers a disc whose track 1 holds no IP.BIN header: the
// line is left out rather than printed empty.
