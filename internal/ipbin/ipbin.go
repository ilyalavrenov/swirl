package ipbin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// https://mc.pp.se/dc/ip0000.bin.html
// https://github.com/KallistiOS/KallistiOS/blob/aec2454e610edabc1bfdcff7333f20d3637222df/utils/makeip/src/field.c
const (
	hardwareID  = 0x00
	makerID     = 0x10
	device      = 0x20
	area        = 0x30
	peripherals = 0x38
	productNo   = 0x40
	version     = 0x4A
	date        = 0x50
	bootFile    = 0x60
	maker       = 0x70
	title       = 0x80
	headerBytes = 0x100

	// Raw 2352-byte sectors put the header past the sync and address.
	headerOffset = 0x10
)

// The peripherals bitfield; the gaps are unassigned bits.
// https://mc.pp.se/dc/ip0000.bin.html#peripherals
const (
	windowsCE = 1 << 0
	vgaBox    = 1 << 4

	lowestGroupBit = 8
)

const (
	otherExpansions = 1 << (iota + lowestGroupBit)
	jumpPack
	microphone
	memoryCard
	standardController
	buttonC
	buttonD
	buttonX
	buttonY
	buttonZ
	expandedDirections
	analogR
	analogL
	analogHorizontal
	analogVertical
	expandedAnalogHorizontal
	expandedAnalogVertical
	lightGun
	keyboard
	mouse
)

// Header is the Dreamcast bootstrap header.
type Header struct {
	HardwareID  string
	MakerID     string
	Title       string
	Maker       string
	ProductNo   string
	Version     string
	ReleaseDate string
	Disc        string // "N/M"
	Regions     []string
	Peripherals []string
	BootFile    string
}

func ProductName(binPath string) (string, error) {
	f, err := os.Open(binPath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", binPath, err)
	}
	defer f.Close()

	h, err := Read(f)
	if err != nil {
		return "", fmt.Errorf("%s: %w", binPath, err)
	}

	return h.Title, nil
}

// Read parses the bootstrap header. r's first byte must be the start of track 1's first sector.
func Read(r io.ReaderAt) (Header, error) {
	buf := make([]byte, headerBytes)

	n, err := r.ReadAt(buf, headerOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return Header{}, fmt.Errorf("read IP.BIN: %w", err)
	}

	buf = buf[:n]

	return Header{
		HardwareID:  field(buf, hardwareID, makerID),
		MakerID:     field(buf, makerID, device),
		Title:       field(buf, title, headerBytes),
		Maker:       field(buf, maker, title),
		ProductNo:   field(buf, productNo, version),
		Version:     field(buf, version, date),
		ReleaseDate: releaseDate(field(buf, date, bootFile)),
		Disc:        discNumber(field(buf, device, area)),
		Regions:     regions(raw(buf, area, peripherals)),
		Peripherals: peripheralList(field(buf, peripherals, productNo)),
		BootFile:    field(buf, bootFile, maker),
	}, nil
}

func raw(buf []byte, from, to int) []byte {
	if from >= len(buf) {
		return nil
	}

	return buf[from:min(to, len(buf))]
}

// Discs pad with spaces, homebrew tools with nulls.
func field(buf []byte, from, to int) string {
	return strings.TrimSpace(strings.TrimRight(string(raw(buf, from, to)), "\x00"))
}

// Device Information reads like "8B40 GD-ROM2/3".
func discNumber(device string) string {
	_, n, _ := strings.Cut(device, "GD-ROM")

	return strings.TrimSpace(n)
}

// Some discs carry an impossible date, so the stored text stands in.
func releaseDate(stored string) string {
	t, err := time.Parse("20060102", stored)
	if err != nil {
		return stored
	}

	return t.Format(time.DateOnly)
}

// Position encodes the region, so the symbols must stay untrimmed.
func regions(area []byte) []string {
	symbols := []struct {
		letter byte
		name   string
	}{{'J', "Japan"}, {'U', "USA"}, {'E', "Europe"}}

	out := make([]string, 0, len(symbols))

	for i, s := range symbols {
		if i < len(area) && area[i] == s.letter {
			out = append(out, s.name)
		}
	}

	return out
}

func peripheralList(stored string) []string {
	bits, err := strconv.ParseUint(stored, 16, 32)
	if err != nil {
		return nil
	}

	named := []struct {
		mask uint64
		name string
	}{
		{standardController, "controller"},
		{buttonC, "C button"},
		{buttonD, "D button"},
		{buttonX, "X button"},
		{buttonY, "Y button"},
		{buttonZ, "Z button"},
		{expandedDirections, "expanded directions"},
		{analogR, "analog R trigger"},
		{analogL, "analog L trigger"},
		{analogHorizontal, "analog horizontal"},
		{analogVertical, "analog vertical"},
		{expandedAnalogHorizontal, "expanded analog horizontal"},
		{expandedAnalogVertical, "expanded analog vertical"},
		{memoryCard, "VMU"},
		{jumpPack, "jump pack"},
		{microphone, "microphone"},
		{otherExpansions, "other expansions"},
		{mouse, "mouse"},
		{keyboard, "keyboard"},
		{lightGun, "light gun"},
		{vgaBox, "VGA box"},
		{windowsCE, "Windows CE"},
	}

	out := make([]string, 0, len(named))

	for _, p := range named {
		if bits&p.mask != 0 {
			out = append(out, p.name)
		}
	}

	return out
}
