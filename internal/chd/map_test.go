package chd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mapStream builds a compressed-map bitstream: a Huffman tree giving eight
// symbols a 3-bit code each, then the symbols themselves. Write never emits the
// run-length or self-reference symbols, so nothing in this package produces a map
// that exercises them and they have to be assembled by hand.
func mapStream(t *testing.T, syms ...int) (*bitReader, []huffmanCodeEntry) {
	t.Helper()

	lengths := make([]int, huffmanNumSymbols)
	for _, s := range []int{0, 1, 2, mapCompNone, mapCompRLESmall, mapCompRLELarge, mapCompSelf0, mapCompSelf1} {
		lengths[s] = 3
	}

	bw := &bitWriter{}
	for _, l := range lengths {
		bw.write(uint32(l), huffmanRLEBitWidth)
	}

	codes, err := buildCanonicalCodes(lengths)
	require.NoError(t, err)

	byIndex := make(map[int]huffmanCodeEntry, len(codes))
	for _, c := range codes {
		byIndex[c.sym] = c
	}

	for _, s := range syms {
		c, ok := byIndex[s]
		require.True(t, ok, "symbol %d has no code", s)
		bw.write(c.code, c.length)
	}

	br := &bitReader{buf: bw.buf}

	treeLengths, err := readHuffmanTree(br)
	require.NoError(t, err)
	require.Equal(t, lengths, treeLengths)

	return br, codes
}

// run is n entries of typ followed by a single literal 0, which is the symbol
// every case emits last to prove the run stopped where it should.
func run(typ uint8, n int) []uint8 {
	out := make([]uint8, n, n+1)
	for i := range out {
		out[i] = typ
	}

	return append(out, 0)
}

func TestReadMapTypesExpandsRuns(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		syms []int
		want []uint8
	}{
		// A run entry repeats the previous type for itself plus 2+count more, so a
		// count of 1 covers four hunks on top of the literal that set the type.
		"small run": {
			syms: []int{mapCompNone, mapCompRLESmall, 1, 0},
			want: run(mapCompNone, 5),
		},
		"small run of the minimum length": {
			syms: []int{mapCompNone, mapCompRLESmall, 0, 0},
			want: run(mapCompNone, 4),
		},
		// A large run's count is two symbols, high nibble first: 18 + (1<<4) + 2.
		"large run": {
			syms: []int{mapCompNone, mapCompRLELarge, 1, 2},
			want: run(mapCompNone, 38),
		},
		"literals only": {
			syms: []int{0, mapCompNone, mapCompSelf1, mapCompSelf0},
			want: []uint8{0, mapCompNone, mapCompSelf1, mapCompSelf0},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			br, codes := mapStream(t, test.syms...)
			records := make([]mapReadRecord, len(test.want))
			require.NoError(t, readMapTypes(br, codes, records))

			got := make([]uint8, len(records))
			for i, r := range records {
				got[i] = r.compType
			}

			assert.Equal(t, test.want, got)
		})
	}
}

// SELF_0 repeats the last target and SELF_1 steps one past it, so a run of them
// walks forward rather than pinning every hunk to the same one.
func TestReadMapEntriesWalksSelfTargets(t *testing.T) {
	t.Parallel()

	records := []mapReadRecord{
		{compType: mapCompSelf1},
		{compType: mapCompSelf0},
		{compType: mapCompSelf1},
		{compType: mapCompSelf1},
	}

	br := &bitReader{}
	require.NoError(t, readMapEntries(br, records, fileHeader{hunkBytes: hunkBytes}, 0, 0, 0))

	got := make([]int, len(records))
	for i, r := range records {
		got[i] = r.selfHunk
	}

	assert.Equal(t, []int{1, 1, 2, 3}, got)
}

func TestReadMapEntriesRejectsParentReferences(t *testing.T) {
	t.Parallel()

	records := []mapReadRecord{{compType: mapCompParent}}

	err := readMapEntries(&bitReader{}, records, fileHeader{hunkBytes: hunkBytes}, 0, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent CHD")
}
