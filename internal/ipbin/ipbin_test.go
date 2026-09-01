package ipbin_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilyalavrenov/swirl/internal/ipbin"
)

// Offsets are restated, not shared with the parser, so a slip cannot cancel itself out.
func sector(fields map[int]string) []byte {
	const sectorHeader = 0x10 // sync and address

	buf := make([]byte, sectorHeader+0x100)
	for offset, text := range fields {
		copy(buf[sectorHeader+offset:], text)
	}

	return buf
}

func TestReadBlankFields(t *testing.T) {
	t.Parallel()

	got, err := ipbin.Read(bytes.NewReader(sector(map[int]string{0x80: "HOMEBREW\x00\x00"})))
	require.NoError(t, err)

	assert.Equal(t, ipbin.Header{Title: "HOMEBREW", Regions: []string{}}, got)
}

func TestReadAreaSymbols(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		symbols string
		want    []string
	}{
		"worldwide":        {"JUE     ", []string{"Japan", "USA", "Europe"}},
		"europe only":      {"  E     ", []string{"Europe"}},
		"unplayable":       {"        ", []string{}},
		"position decides": {"E       ", []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ipbin.Read(bytes.NewReader(sector(map[int]string{0x30: test.symbols})))
			require.NoError(t, err)
			assert.Equal(t, test.want, got.Regions)
		})
	}
}

func TestReadPeripherals(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		bits string
		want []string
	}{
		"absent":    {"        ", nil},
		"none set":  {"0000000 ", []string{}},
		"platform":  {"0000011 ", []string{"VGA box", "Windows CE"}},
		"expansion": {"0000F00 ", []string{"VMU", "jump pack", "microphone", "other expansions"}},
		"buttons": {"007F000 ", []string{
			"controller", "C button", "D button", "X button", "Y button", "Z button", "expanded directions",
		}},
		"analog": {"1F80000 ", []string{
			"analog R trigger", "analog L trigger", "analog horizontal", "analog vertical",
			"expanded analog horizontal", "expanded analog vertical",
		}},
		"optional": {"E000000 ", []string{"mouse", "keyboard", "light gun"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ipbin.Read(bytes.NewReader(sector(map[int]string{0x38: test.bits})))
			require.NoError(t, err)
			assert.Equal(t, test.want, got.Peripherals)
		})
	}
}

func TestReadReleaseDate(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		stored, want string
	}{
		"parsed":     {"20000102        ", "2000-01-02"},
		"impossible": {"20000895        ", "20000895"},
		"absent":     {"                ", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ipbin.Read(bytes.NewReader(sector(map[int]string{0x50: test.stored})))
			require.NoError(t, err)
			assert.Equal(t, test.want, got.ReleaseDate)
		})
	}
}

func TestProductName(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "track01.bin")
	require.NoError(t, os.WriteFile(path, sector(map[int]string{0x80: "TEST TITLE"}), 0o600))

	got, err := ipbin.ProductName(path)
	require.NoError(t, err)
	assert.Equal(t, "TEST TITLE", got)
}

// TestProductNameShortFile covers a track truncated inside the title: ReadAt reports
// EOF alongside a partial read.
func TestProductNameShortFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "short.bin")
	require.NoError(t, os.WriteFile(path, sector(map[int]string{0x80: "ABCD"})[:0x94], 0o600))

	got, err := ipbin.ProductName(path)
	require.NoError(t, err)
	assert.Equal(t, "ABCD", got)
}

func TestProductNameMissingFile(t *testing.T) {
	t.Parallel()

	_, err := ipbin.ProductName(filepath.Join(t.TempDir(), "nope.bin"))
	require.Error(t, err)
}
