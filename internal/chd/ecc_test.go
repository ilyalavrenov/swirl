package chd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ilyalavrenov/swirl/internal/disc"
)

// goldenSector is a MODE1 sector layout with a fixed pattern in its user data.
func goldenSector() []byte {
	s := make([]byte, sectorBytes)
	copy(s, syncHeader())
	copy(s[12:], []byte{0x00, 0x02, 0x10, 0x01})

	for i := range 2048 {
		s[16+i] = byte(i*7) ^ 0x5A
	}

	return s
}

// The parity below is pinned, not recomputed, because a self-consistent
// strip-and-restore passes just as happily with the wrong algorithm. These bytes
// were taken from an implementation checked against real chdman output: every
// sector of a 1.1 GB GD-ROM track came back byte-identical through them.
func TestRestoreSectorMatchesGoldenParity(t *testing.T) {
	t.Parallel()

	s := goldenSector()
	newECCTables().restoreSector(s)

	assert.Equal(t,
		[]byte{0xb4, 0xe2, 0x8f, 0x2e, 0xd1, 0x09, 0xd5, 0xca, 0x3f, 0x97, 0x61, 0x4e, 0xda, 0x0a, 0xa5, 0x4b},
		s[eccPOffset:eccPOffset+16], "ECC P")
	assert.Equal(t,
		[]byte{0xb1, 0xa4, 0x72, 0x1c, 0xe8, 0x96, 0x64, 0xdc, 0xbe, 0xbf, 0xe2, 0x77, 0xeb, 0xdc, 0xcd, 0x06},
		s[eccQOffset:eccQOffset+16], "ECC Q")
}

// A MODE1 sector's sync pattern and P/Q parity are fully determined by the rest
// of it, which is why chdman is free to drop them.
func TestRestoreSectorRebuildsParity(t *testing.T) {
	t.Parallel()

	sector := goldenSector()
	newECCTables().restoreSector(sector)
	want := bytes.Clone(sector)

	// Strip exactly what chdman strips, then put it back.
	copy(sector, make([]byte, len(syncHeader())))
	copy(sector[eccPOffset:], make([]byte, sectorBytes-eccPOffset))
	newECCTables().restoreSector(sector)

	assert.Equal(t, want, sector)
	assert.Equal(t, syncHeader(), sector[:len(syncHeader())])
	assert.NotEqual(t, make([]byte, sectorBytes-eccPOffset), sector[eccPOffset:], "parity must not be left blank")
}

func TestRestoreSectorDependsOnTheHeader(t *testing.T) {
	t.Parallel()

	build := func(minute byte) []byte {
		s := make([]byte, sectorBytes)
		copy(s, syncHeader())
		copy(s[12:], []byte{minute, 0x02, 0x00, 0x01})
		newECCTables().restoreSector(s)

		return s
	}

	assert.NotEqual(t, build(0x00)[eccPOffset:], build(0x01)[eccPOffset:],
		"mode 1 parity covers the sector header, so a different address changes it")
}

func TestSwapAudioIsItsOwnInverse(t *testing.T) {
	t.Parallel()

	orig := []byte{0x01, 0x02, 0x03, 0x04, 0xAA, 0xBB}
	got := bytes.Clone(orig)

	swapAudio(got)
	assert.Equal(t, []byte{0x02, 0x01, 0x04, 0x03, 0xBB, 0xAA}, got)

	swapAudio(got)
	assert.Equal(t, orig, got)
}

// CD audio is big-endian inside a CHD and little-endian in a track file, so the
// bytes on disk must be the swapped ones and a read has to undo it.
func TestAudioIsStoredByteSwapped(t *testing.T) {
	t.Parallel()

	const frames = 4

	audio := make([]byte, frames*sectorBytes)
	for i := range audio {
		audio[i] = byte(i)
	}

	path := writeCHD(t, []Track{
		{Number: 1, Type: disc.TrackTypeAudio, Frames: frames, Data: bytes.NewReader(bytes.Clone(audio))},
	})

	c, err := openCHD(path)
	require.NoError(t, err)

	defer c.close()

	raw, err := readHunk(c.f, c.records, 0, c.header.hunkBytes)
	require.NoError(t, err)

	swapped := bytes.Clone(audio)
	swapAudio(swapped)
	assert.Equal(t, swapped[:sectorBytes], raw[:sectorBytes], "the stored hunk holds the swapped bytes")
	assert.NotEqual(t, audio[:sectorBytes], raw[:sectorBytes])
}

func TestDataTracksAreNotSwapped(t *testing.T) {
	t.Parallel()

	const frames = 4

	data := make([]byte, frames*sectorBytes)
	for i := range data {
		data[i] = byte(i)
	}

	path := writeCHD(t, []Track{
		{Number: 1, Type: disc.TrackTypeMode1, Frames: frames, Data: bytes.NewReader(bytes.Clone(data))},
	})

	c, err := openCHD(path)
	require.NoError(t, err)

	defer c.close()

	raw, err := readHunk(c.f, c.records, 0, c.header.hunkBytes)
	require.NoError(t, err)
	assert.Equal(t, data[:sectorBytes], raw[:sectorBytes])
}

// A self-reference carries no data of its own, so the reader has to follow it to
// the hunk that does. swirl never writes one; chdman writes them constantly.
func TestReadHunkFollowsSelfReferences(t *testing.T) {
	t.Parallel()

	const frames = 4

	data := make([]byte, frames*sectorBytes)
	for i := range data {
		data[i] = byte(i * 3)
	}

	path := writeCHD(t, []Track{
		{Number: 1, Type: disc.TrackTypeMode1, Frames: frames, Data: bytes.NewReader(bytes.Clone(data))},
	})

	c, err := openCHD(path)
	require.NoError(t, err)

	defer c.close()

	want, err := readHunk(c.f, c.records, 0, c.header.hunkBytes)
	require.NoError(t, err)

	// A second record that owns no bytes and points at hunk 0.
	records := []mapReadRecord{c.records[0], {selfHunk: 0}}

	got, err := readHunk(c.f, records, 1, c.header.hunkBytes)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestReadHunkRejectsASelfReferenceCycle(t *testing.T) {
	t.Parallel()

	records := []mapReadRecord{{selfHunk: 1}, {selfHunk: 0}}

	_, err := readHunk(nil, records, 0, hunkBytes)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle")
}
