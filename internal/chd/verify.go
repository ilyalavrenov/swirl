package chd

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // SHA1 is required by the CHD v5 format
	"fmt"
	"io"
	"os"
)

type chdFile struct {
	f       *os.File
	header  fileHeader
	metas   []rawMeta
	tracks  []TrackInfo
	records []mapReadRecord
}

func openCHD(path string) (*chdFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("CHD: %w", err)
	}

	c, err := parseCHD(f, path)
	if err != nil {
		f.Close()

		return nil, err
	}

	return c, nil
}

func parseCHD(f *os.File, path string) (*chdFile, error) {
	h, err := readHeader(f, path)
	if err != nil {
		return nil, err
	}

	metas, err := readMetaChain(f, h)
	if err != nil {
		return nil, err
	}

	tracks, err := parseTrackMetas(metas)
	if err != nil {
		return nil, err
	}

	if len(tracks) == 0 {
		return nil, fmt.Errorf("%s: no track metadata found", path)
	}

	records, err := readCompressedMap(f, h)
	if err != nil {
		return nil, err
	}

	return &chdFile{f: f, header: h, metas: metas, tracks: tracks, records: records}, nil
}

func (c *chdFile) close() {
	c.f.Close()
}

type Info struct {
	Version      uint32
	Codec        string
	LogicalBytes int64
	HunkBytes    int
	Hunks        int
	Tracks       []TrackInfo
}

// Stat reads the layout without decompressing anything.
func Stat(path string) (Info, error) {
	c, err := openCHD(path)
	if err != nil {
		return Info{}, err
	}
	defer c.close()

	return Info{
		Version: c.header.version,
		Codec:   codecList(c.header.codecs),

		LogicalBytes: c.header.logicalBytes,
		HunkBytes:    c.header.hunkBytes,
		Hunks:        len(c.records),
		Tracks:       c.tracks,
	}, nil
}

type VerifyReport struct {
	Hunks        int
	Tracks       []TrackInfo
	LogicalBytes int64
	RawSHA1      []byte
	CombinedSHA1 []byte
}

// Verify decompresses every hunk, CRC-checks it, and recomputes both header digests.
// Hashing the decompressed side catches asymmetry with Write, which hashes what it
// assembles. Agreement does not prove chdman would write the same digests.
func Verify(ctx context.Context, path string, progress io.Writer) (VerifyReport, error) {
	if progress == nil {
		progress = io.Discard
	}

	c, err := openCHD(path)
	if err != nil {
		return VerifyReport{}, err
	}
	defer c.close()

	rawHash := sha1.New() //nolint:gosec // SHA1 is required by the CHD v5 format

	remaining := c.header.logicalBytes

	for raw, hunkErr := range hunkSeq(ctx, c) {
		if hunkErr != nil {
			return VerifyReport{}, fmt.Errorf("%s: %w", path, hunkErr)
		}

		// The digest covers the logical size, so a final part-used hunk contributes only its claimed bytes.
		if int64(len(raw)) > remaining {
			raw = raw[:remaining]
		}

		remaining -= int64(len(raw))

		rawHash.Write(raw)
		progress.Write(raw) //nolint:errcheck // progress reporting never affects the result
	}

	raw := rawHash.Sum(nil)
	if !bytes.Equal(raw, c.header.rawSHA1) {
		return VerifyReport{}, fmt.Errorf("%s: hunk data hashes to %x, header says %x", path, raw, c.header.rawSHA1)
	}

	combined := combinedSHA1(raw, verifyTuples(c.metas))
	if !bytes.Equal(combined, c.header.combinedSHA1) {
		return VerifyReport{}, fmt.Errorf("%s: metadata hashes to %x, header says %x",
			path, combined, c.header.combinedSHA1)
	}

	return VerifyReport{
		Hunks:        len(c.records),
		Tracks:       c.tracks,
		LogicalBytes: c.header.logicalBytes,
		RawSHA1:      raw,
		CombinedSHA1: combined,
	}, nil
}

// Only entries flagged CHD_MDFLAGS_CHECKSUM contribute to the digest.
func verifyTuples(metas []rawMeta) [][]byte {
	var tuples [][]byte

	for _, m := range metas {
		if m.flags&metaFlags != 0 {
			tuples = append(tuples, metaTuple(m.tag, m.payload))
		}
	}

	return tuples
}

// FirstSector holds a Dreamcast data track's IP.BIN header. Only hunk 0 is read.
func FirstSector(path string) ([]byte, error) {
	c, err := openCHD(path)
	if err != nil {
		return nil, err
	}
	defer c.close()

	batch, err := decompressBatch(c.f, c.header, c.records, 0, 1)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return batch[0][:sectorBytes], nil
}
