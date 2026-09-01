package chd

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/mewkiz/flac"
	flacframe "github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
	"github.com/ulikunitz/xz/lzma"
)

const (
	// Above this the sector-block length field widens to three bytes.
	wideLengthHunkBytes = 65536
	narrowLengthBytes   = 2
	wideLengthBytes     = 3

	// (pb*5 + lp)*9 + lc for the SDK defaults lc=3, lp=0, pb=2.
	lzmaProps = 0x5D

	// CD audio: 16-bit stereo at 44.1 kHz.
	audioChannels      = 2
	audioSampleBytes   = 2
	audioBitsPerSample = 16
	audioFrameBytes    = audioChannels * audioSampleBytes
	audioSampleRate    = 44100

	// chdman's block size; MAME's decoder rejects any other.
	flacBlockSamples = 2352

	// Far above any real hunk, but a bound: klauspost defaults to 64 GiB.
	zstdMaxDecoded = 1 << 24
)

func decompressHunk(codec uint32, data []byte, hunkBytes int) ([]byte, error) {
	switch codec {
	case codecCDZlib:
		return decompressFramed(data, hunkBytes, zlibDecompress, zlibDecompress)
	case codecCDLzma:
		return decompressFramed(data, hunkBytes, lzmaDecompress, zlibDecompress)
	case codecCDZstd:
		return decompressFramed(data, hunkBytes, zstdDecompress, zstdDecompress)
	case codecCDFlac:
		return decompressCDFL(data, hunkBytes)
	default:
		return nil, fmt.Errorf("no decoder for compressor %q", tagString(codec))
	}
}

func supportedCodec(tag uint32) bool {
	switch tag {
	case codecCDZlib, codecCDLzma, codecCDZstd, codecCDFlac:
		return true
	default:
		return false
	}
}

type blockDecoder func(data []byte, want int) ([]byte, error)

// cdzl, cdlz and cdzs differ only in their two inner codecs. The subcode block is present
// even on discs that carry none.
// https://github.com/rtissera/libchdr/blob/5f82799f2c8cad1e9cd26d39a0f8d36369a5534b/src/libchdr_chd.c#L718-L756
func decompressFramed(data []byte, hunkBytes int, base, sub blockDecoder) ([]byte, error) {
	frames := hunkBytes / slotBytes
	eccBytes := (frames + bitsPerByte - 1) / bitsPerByte

	lengthBytes := narrowLengthBytes
	if hunkBytes >= wideLengthHunkBytes {
		lengthBytes = wideLengthBytes
	}

	if len(data) < eccBytes+lengthBytes {
		return nil, fmt.Errorf("hunk too short (%d bytes)", len(data))
	}

	baseLen := 0
	for _, b := range data[eccBytes : eccBytes+lengthBytes] {
		baseLen = baseLen<<bitsPerByte | int(b)
	}

	start := eccBytes + lengthBytes
	if start+baseLen > len(data) {
		return nil, errors.New("sector block overflows hunk")
	}

	sectorData, err := base(data[start:start+baseLen], frames*sectorBytes)
	if err != nil {
		return nil, fmt.Errorf("decompress sector data: %w", err)
	}

	// Unused here, but covered by the stored hunk CRC, so it still has to be decoded.
	subcodeData, err := sub(data[start+baseLen:], frames*subcodeBytes)
	if err != nil {
		return nil, fmt.Errorf("decompress subcode data: %w", err)
	}

	return assembleHunk(sectorData, subcodeData, data[:eccBytes], hunkBytes), nil
}

// A set bitmap bit means chdman stripped that sector's sync header and parity; Write
// never does, and cdfl passes no bitmap at all.
func assembleHunk(sectorData, subcodeData, stripped []byte, hunkBytes int) []byte {
	out := make([]byte, hunkBytes)
	tables := newECCTables()

	for i := range hunkBytes / slotBytes {
		copy(out[i*slotBytes:], sectorData[i*sectorBytes:(i+1)*sectorBytes])
		copy(out[i*slotBytes+sectorBytes:], subcodeData[i*subcodeBytes:(i+1)*subcodeBytes])

		if len(stripped) > 0 && stripped[i/bitsPerByte]&(1<<(i%bitsPerByte)) != 0 {
			tables.restoreSector(out[i*slotBytes : i*slotBytes+sectorBytes])
		}
	}

	return out
}

// CHD stores a bare LZMA1 stream, so MAME's encoder properties have to be rebuilt; the
// synthesized 13-byte classic header feeds them to a stock decoder.
// https://github.com/rtissera/libchdr/blob/5f82799f2c8cad1e9cd26d39a0f8d36369a5534b/src/libchdr_chd.c#L589-L637
func lzmaDecompress(data []byte, want int) ([]byte, error) {
	header := make([]byte, lzma.HeaderLen)
	header[0] = lzmaProps
	binary.LittleEndian.PutUint32(header[1:], uint32(max(want, lzma.MinDictCap))) //nolint:gosec // G115: block sizes are positive
	binary.LittleEndian.PutUint64(header[5:], uint64(want))                       //nolint:gosec // G115: block sizes are positive

	r, err := lzma.NewReader(io.MultiReader(bytes.NewReader(header), bytes.NewReader(data)))
	if err != nil {
		return nil, fmt.Errorf("lzma init: %w", err)
	}

	out := make([]byte, want)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("lzma decompress: %w", err)
	}

	return out, nil
}

// DecodeAll is safe for concurrent use, so one decoder serves the process, and it expands
// a frame in full, so the ceiling has to be the decoder's own.
//
//nolint:gochecknoglobals // lazily built shared decoder, no observable state
var zstdDecoder = sync.OnceValues(func() (*zstd.Decoder, error) {
	return zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(0),
		zstd.WithDecoderMaxMemory(zstdMaxDecoded),
	)
})

func zstdDecompress(data []byte, want int) ([]byte, error) {
	d, err := zstdDecoder()
	if err != nil {
		return nil, fmt.Errorf("zstd init: %w", err)
	}

	out, err := d.DecodeAll(data, make([]byte, 0, want))
	if err != nil {
		return nil, fmt.Errorf("zstd decompress: %w", err)
	}

	if len(out) != want {
		return nil, fmt.Errorf("zstd produced %d bytes, want %d", len(out), want)
	}

	return out, nil
}

// Compressor slots as chdman prints them: "cdlz, cdzl, cdfl".
func codecList(codecs [numCompressors]uint32) string {
	tags := make([]string, 0, len(codecs))

	for _, c := range codecs {
		if c != 0 {
			tags = append(tags, tagString(c))
		}
	}

	return strings.Join(tags, ", ")
}

// cdfl stores bare FLAC frames, no "fLaC" magic and no STREAMINFO, but each carries its
// blocksize, rate and channels inline and so parses standalone. Audio has no ECC bitmap.
// https://github.com/rtissera/libchdr/blob/5f82799f2c8cad1e9cd26d39a0f8d36369a5534b/src/libchdr_chd.c#L1043-L1080
func decompressCDFL(data []byte, hunkBytes int) ([]byte, error) {
	frames := hunkBytes / slotBytes
	sectorData := make([]byte, frames*sectorBytes)
	r := bytes.NewReader(data)

	for off := 0; off < len(sectorData); {
		f, err := flacframe.Parse(r)
		if err != nil {
			return nil, fmt.Errorf("flac frame at byte %d: %w", len(data)-r.Len(), err)
		}

		if len(f.Subframes) != audioChannels {
			return nil, fmt.Errorf("flac frame has %d channels, want %d", len(f.Subframes), audioChannels)
		}

		// Without this a deeper frame would truncate into plausible-looking PCM.
		if f.BitsPerSample != audioBitsPerSample {
			return nil, fmt.Errorf("flac frame is %d-bit, want %d", f.BitsPerSample, audioBitsPerSample)
		}

		left, right := f.Subframes[0].Samples, f.Subframes[1].Samples
		if len(left) != len(right) {
			return nil, fmt.Errorf("flac channels disagree on length: %d and %d", len(left), len(right))
		}

		// A frame carrying no samples would spin here forever on crafted input.
		if len(left) == 0 || off+len(left)*audioFrameBytes > len(sectorData) {
			return nil, fmt.Errorf("flac frame of %d samples does not fit the hunk", len(left))
		}

		// CD audio is big-endian inside a CHD and little-endian in a BIN.
		for i := range left {
			//nolint:gosec // G115: the bit depth is checked above
			binary.BigEndian.PutUint16(sectorData[off:], uint16(left[i]))
			//nolint:gosec // G115: the bit depth is checked above
			binary.BigEndian.PutUint16(sectorData[off+audioSampleBytes:], uint16(right[i]))
			off += audioFrameBytes
		}
	}

	subcodeData, err := zlibDecompress(data[len(data)-r.Len():], frames*subcodeBytes)
	if err != nil {
		return nil, fmt.Errorf("decompress subcode data: %w", err)
	}

	return assembleHunk(sectorData, subcodeData, nil, hunkBytes), nil
}

// Bare FLAC frames, no signature and no STREAMINFO, then the deflated subcode.
func compressCDFLAC(hunkData []byte) ([]byte, error) {
	sectorData, subcodeData := splitHunk(hunkData)

	var buf bytes.Buffer

	enc, err := flac.NewEncoder(&buf, &meta.StreamInfo{
		BlockSizeMin:  flacBlockSamples,
		BlockSizeMax:  flacBlockSamples,
		SampleRate:    audioSampleRate,
		NChannels:     audioChannels,
		BitsPerSample: audioBitsPerSample,
	})
	if err != nil {
		return nil, fmt.Errorf("flac init: %w", err)
	}

	buf.Reset() // drop the stream header the encoder just wrote

	for off := 0; off < len(sectorData); off += flacBlockSamples * audioFrameBytes {
		if err := enc.WriteFrame(flacFrame(sectorData[off:])); err != nil {
			return nil, fmt.Errorf("flac frame at byte %d: %w", off, err)
		}
	}

	subcodeCmp, err := zlibCompress(subcodeData)
	if err != nil {
		return nil, err
	}

	return append(buf.Bytes(), subcodeCmp...), nil
}

// CHD stores CD audio big-endian.
func flacFrame(sectorData []byte) *flacframe.Frame {
	// PredVerbatim asks the encoder to pick a predictor; the zero value claims constant samples.
	newSubframe := func() *flacframe.Subframe {
		return &flacframe.Subframe{
			Pred:     flacframe.PredVerbatim,
			Samples:  make([]int32, flacBlockSamples),
			NSamples: flacBlockSamples,
		}
	}

	left, right := newSubframe(), newSubframe()

	for i := range flacBlockSamples {
		off := i * audioFrameBytes
		//nolint:gosec // G115: reinterpreting the sample's bits, the value is already 16-bit
		left.Samples[i] = int32(int16(binary.BigEndian.Uint16(sectorData[off:])))
		//nolint:gosec // G115: reinterpreting the sample's bits, the value is already 16-bit
		right.Samples[i] = int32(int16(binary.BigEndian.Uint16(sectorData[off+audioSampleBytes:])))
	}

	return &flacframe.Frame{
		HasFixedBlockSize: true,
		BlockSize:         flacBlockSamples,
		SampleRate:        audioSampleRate,
		Channels:          flacframe.ChannelsLeftSide,
		BitsPerSample:     audioBitsPerSample,
		Subframes:         []*flacframe.Subframe{left, right},
	}
}
