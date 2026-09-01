package chd

import (
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The length field widens past a 64 KiB hunk. chdman only writes 8-frame CD hunks, so no
// fixture can reach this path; libchdr stays the authority on the threshold itself.
func TestDecompressFramedReadsAWideLengthField(t *testing.T) {
	t.Parallel()

	const frames = 27 // 27 * 2448 = 66096, past the threshold

	hunkBytes := frames * slotBytes
	require.GreaterOrEqual(t, hunkBytes, wideLengthHunkBytes)

	sectors := make([]byte, frames*sectorBytes)
	for i := range sectors {
		sectors[i] = byte(i * 7)
	}

	sectorCmp, err := zlibCompress(sectors)
	require.NoError(t, err)

	subcodeCmp, err := zlibCompress(make([]byte, frames*subcodeBytes))
	require.NoError(t, err)

	eccBytes := (frames + bitsPerByte - 1) / bitsPerByte
	blob := make([]byte, eccBytes, eccBytes+wideLengthBytes+len(sectorCmp)+len(subcodeCmp))
	blob = append(blob, byte(len(sectorCmp)>>16), byte(len(sectorCmp)>>8), byte(len(sectorCmp)))
	blob = append(blob, sectorCmp...)
	blob = append(blob, subcodeCmp...)

	got, err := decompressFramed(blob, hunkBytes, zlibDecompress, zlibDecompress)
	require.NoError(t, err)
	assert.Equal(t, sectors[:sectorBytes], got[:sectorBytes])
}

// DecodeAll expands a frame in full, so the refusal has to come from the decoder: a length
// check afterwards runs once the memory is already committed.
func TestZstdDecompressRefusesAnOversizedFrame(t *testing.T) {
	t.Parallel()

	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)

	bomb := enc.EncodeAll(make([]byte, zstdMaxDecoded+1), nil)
	require.Less(t, len(bomb), 4096, "a small input has to be what expands")

	_, err = zstdDecompress(bomb, sectorBytes)
	require.ErrorIs(t, err, zstd.ErrDecoderSizeExceeded)
}
