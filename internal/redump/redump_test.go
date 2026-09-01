package redump

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two regional releases pressed from the same tracks, so a match is a set of titles.
const catalogue = `<?xml version="1.0"?>
<datafile>
	<header><version>2026-06-14 18-25-41</version></header>
	<game name="One (Japan)">
		<rom name="One (Japan).cue" size="1" sha1="cccccccccccccccccccccccccccccccccccccccc"/>
		<rom name="One (Japan) (Track 01).bin" size="2" sha1="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"/>
		<rom name="One (Japan) (Track 02).bin" size="2" sha1="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"/>
	</game>
	<game name="One (Europe)">
		<rom name="One (Europe) (Track 01).bin" size="2" sha1="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"/>
		<rom name="One (Europe) (Track 02).bin" size="2" sha1="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"/>
	</game>
</datafile>
`

const (
	trackA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	trackB  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cueHash = "cccccccccccccccccccccccccccccccccccccccc"
	unknown = "0000000000000000000000000000000000000000"
)

func TestMatch(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "redump.dat")
	require.NoError(t, os.WriteFile(path, []byte(catalogue), 0o600))

	d, err := Load(path)
	require.NoError(t, err)

	sums := func(hexes ...string) [][]byte {
		out := make([][]byte, 0, len(hexes))

		for _, h := range hexes {
			raw, decErr := hex.DecodeString(h)
			require.NoError(t, decErr)
			out = append(out, raw)
		}

		return out
	}

	for name, test := range map[string]struct {
		sums  [][]byte
		want  []string
		known int
	}{
		"whole disc, in any order": {sums(trackB, trackA), []string{"One (Europe)", "One (Japan)"}, 2},
		"a track short":            {sums(trackA, unknown), nil, 1},
		// Every hash known and the count right; only the multiset says no.
		"one track repeated for another": {sums(trackA, trackA), nil, 2},
		"cue sheets are not matchable":   {sums(cueHash), nil, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			res := d.Match(test.sums)

			assert.Equal(t, test.want, res.Titles)
			assert.Equal(t, test.known, res.KnownCount())
		})
	}
}
