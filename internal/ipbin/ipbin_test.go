package ipbin_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilyalavrenov/swirl/internal/ipbin"
)

// writeTrack pads the IP.BIN name field the way a real header does.
func writeTrack(t *testing.T, name string, pad byte) string {
	t.Helper()

	const (
		nameOffset = 0x90
		nameLength = 128
	)

	data := make([]byte, nameOffset+nameLength)
	copy(data[nameOffset:], name)

	for i := nameOffset + len(name); i < len(data); i++ {
		data[i] = pad
	}

	path := filepath.Join(t.TempDir(), "track01.bin")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

func TestProductName(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		stored string
		pad    byte
		want   string
	}{
		"space padded": {"TEST DISC", ' ', "TEST DISC"},
		"null padded":  {"TESTDISC", 0x00, "TESTDISC"},
		"empty":        {"", 0x00, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ipbin.ProductName(writeTrack(t, test.stored, test.pad))
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

// TestProductNameShortFile covers a track truncated inside the name field: ReadAt
// reports EOF alongside a partial read.
func TestProductNameShortFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "short.bin")
	data := make([]byte, 0x90+4)
	copy(data[0x90:], "ABCD")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	got, err := ipbin.ProductName(path)
	require.NoError(t, err)
	assert.Equal(t, "ABCD", got)
}

func TestProductNameMissingFile(t *testing.T) {
	t.Parallel()

	_, err := ipbin.ProductName(filepath.Join(t.TempDir(), "nope.bin"))
	require.Error(t, err)
}
