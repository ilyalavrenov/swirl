package chd

// chdman strips each sector's 12-byte sync pattern and 276 bytes of Reed-Solomon P/Q
// parity, both derivable from the rest, and flags the sector in the hunk's ECC bitmap.
// https://github.com/mamedev/mame/blob/33c42e9e0e89c879e0fc5b654cc70b947bf1473c/src/lib/util/cdrom.cpp#L1436-L1445

const (
	// The parity covers the 4-byte header plus everything up to the P field.
	eccDataOffset = 12
	eccPOffset    = 2076
	eccQOffset    = 2248

	// Reed-Solomon shape: P has 86 codewords of 24 symbols, Q has 52 of 43.
	eccPCount, eccPComp, eccPMajorMult, eccPMinorInc = 86, 24, 2, 86
	eccQCount, eccQComp, eccQMajorMult, eccQMinorInc = 52, 43, 86, 88

	// Mode 2 computes its parity as though the 4-byte header were zero; mode 1 does not.
	modeOffset = 15
	modeTwo    = 2

	// GF(2^8) reducing polynomial, x^8 + x^4 + x^3 + x^2 + 1.
	gf8Poly = 0x1D

	highBit = 0x80
)

func syncHeader() []byte {
	return []byte{0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00}
}

// f doubles in GF(2^8); b inverts it.
type eccTables struct{ f, b [256]byte }

func newECCTables() eccTables {
	var t eccTables

	for i := range 256 {
		j := byte(i) << 1
		if i&highBit != 0 {
			j ^= gf8Poly
		}

		t.f[i] = j
		t.b[byte(i)^j] = byte(i)
	}

	return t
}

func (t eccTables) restoreSector(sector []byte) {
	copy(sector, syncHeader())

	// Q covers the P field as well as the data, so P has to be written first.
	t.writeParity(sector, eccPCount, eccPComp, eccPMajorMult, eccPMinorInc, sector[eccPOffset:])
	t.writeParity(sector, eccQCount, eccQComp, eccQMajorMult, eccQMinorInc, sector[eccQOffset:])
}

func (t eccTables) writeParity(sector []byte, majorCount, minorCount, majorMult, minorInc int, out []byte) {
	mode2 := sector[modeOffset] == modeTwo
	size := majorCount * minorCount

	for major := range majorCount {
		index := (major>>1)*majorMult + (major & 1)

		var a, b byte

		for range minorCount {
			var v byte
			if !mode2 || index >= 4 {
				v = sector[eccDataOffset+index]
			}

			index += minorInc
			if index >= size {
				index -= size
			}

			a ^= v
			b ^= v
			a = t.f[a]
		}

		a = t.b[t.f[a]^b]
		out[major] = a
		out[major+majorCount] = a ^ b
	}
}

// CD audio is byte-swapped inside a CHD: big-endian there, little-endian in a BIN track.
// https://github.com/mamedev/mame/blob/33c42e9e0e89c879e0fc5b654cc70b947bf1473c/src/lib/util/cdrom.cpp#L431-L438
func swapAudio(sector []byte) {
	for i := 0; i+1 < len(sector); i += 2 {
		sector[i], sector[i+1] = sector[i+1], sector[i]
	}
}
