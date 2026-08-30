package convert

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ilyalavrenov/swirl/internal/chd"
	"github.com/ilyalavrenov/swirl/internal/cue"
	"github.com/ilyalavrenov/swirl/internal/disc"
	"github.com/ilyalavrenov/swirl/internal/gdi"
	"github.com/ilyalavrenov/swirl/internal/ipbin"
)

type TrackDesc struct {
	Number   int
	Type     string // CUE track type
	StartLBA int
	Frames   int    // data frames, excluding the pregap
	Pregap   int    // the previous track's run-out, at the head of File
	File     string // empty for a CHD, whose tracks share one archive
}

func (t TrackDesc) Bytes() int64 {
	return int64(t.Frames) * disc.SectorBytes
}

type Description struct {
	Format Format
	Name   string // IP.BIN product name, empty when there is none to read
	GDROM  bool
	Codec  string // CHD only
	Hunks  int    // CHD only
	Stored int64  // CHD only: size on disk
	Tracks []TrackDesc
}

func (d Description) Bytes() int64 {
	var total int64
	for _, t := range d.Tracks {
		total += t.Bytes()
	}

	return total
}

func Describe(format Format, path string) (Description, error) {
	switch format {
	case FormatCUE:
		return describeCUE(path)
	case FormatGDI:
		return describeGDI(path)
	case FormatCHD:
		return describeCHD(path)
	default:
		return Description{}, fmt.Errorf("cannot describe a %s image", format)
	}
}

func describeCUE(path string) (Description, error) {
	sheet, err := cue.Parse(path)
	if err != nil {
		return Description{}, err
	}

	workingDir := filepath.Dir(path)

	l, err := cueTrackLayout(workingDir, sheet)
	if err != nil {
		return Description{}, err
	}

	return Description{
		Format: FormatCUE,
		Name:   productName(workingDir, l.tracks),
		GDROM:  l.gdrom,
		Tracks: l.tracks,
	}, nil
}

func describeGDI(path string) (Description, error) {
	sheet, err := gdi.Parse(path)
	if err != nil {
		return Description{}, err
	}

	workingDir := filepath.Dir(path)

	l, err := gdiTrackLayout(workingDir, sheet)
	if err != nil {
		return Description{}, err
	}

	return Description{
		Format: FormatGDI,
		Name:   productName(workingDir, l.tracks),
		GDROM:  l.gdrom,
		Tracks: l.tracks,
	}, nil
}

func describeCHD(path string) (Description, error) {
	info, err := chd.Stat(path)
	if err != nil {
		return Description{}, err
	}

	tracks := make([]TrackDesc, 0, len(info.Tracks))
	lba := 0

	for _, t := range info.Tracks {
		tracks = append(tracks, TrackDesc{
			Number:   t.Number,
			Type:     t.CUEType,
			StartLBA: lba,
			Frames:   t.RealFrames,
		})

		// Pad frames still occupy sectors.
		lba += t.TotalFrames
	}

	desc := Description{
		Format: FormatCHD,
		Name:   chdName(path),
		GDROM:  len(info.Tracks) > 0 && info.Tracks[0].GDROM,
		Codec:  info.Codec,
		Hunks:  info.Hunks,
		Tracks: tracks,
	}

	stored, err := fileSize(path)
	if err != nil {
		return Description{}, err
	}

	desc.Stored = stored

	return desc, nil
}

// Any failure yields an empty name: a headerless track 1 is not worth failing a listing over.
func productName(workingDir string, tracks []TrackDesc) string {
	i := slices.IndexFunc(tracks, func(t TrackDesc) bool { return t.Number == 1 })
	if i < 0 {
		return ""
	}

	f, err := os.Open(filepath.Join(workingDir, tracks[i].File))
	if err != nil {
		return ""
	}
	defer f.Close()

	// IP.BIN starts after the pregap, not at the head of the file.
	head := io.NewSectionReader(f, int64(tracks[i].Pregap)*disc.SectorBytes, disc.SectorBytes)

	name, err := ipbin.ProductNameAt(head)
	if err != nil {
		return ""
	}

	return name
}

func chdName(path string) string {
	sector, err := chd.FirstSector(path)
	if err != nil {
		return ""
	}

	name, err := ipbin.ProductNameAt(bytes.NewReader(sector))
	if err != nil {
		return ""
	}

	return name
}

type discLayout struct {
	tracks []TrackDesc
	gdrom  bool

	// Padded out so the high-density area starts at exactly disc.HDAStartLBA. -1 when there is none.
	bridge int
}

// The only place the pregap skip and the high-density jump are applied, so a conversion and
// an info listing cannot disagree about a track's LBA.
func cueTrackLayout(workingDir string, sheet *cue.Sheet) (discLayout, error) {
	l := discLayout{bridge: -1}
	lba := 0

	for i, file := range sheet.Files {
		if len(file.Tracks) == 0 {
			continue
		}

		track := file.Tracks[0]

		size, err := fileSize(filepath.Join(workingDir, file.Name))
		if err != nil {
			return discLayout{}, err
		}

		pregap := trackPregap(track)
		lba += pregap
		frames := int(size/disc.SectorBytes) - pregap

		l.tracks = append(l.tracks, TrackDesc{
			Number:   track.Number,
			Type:     track.Type,
			StartLBA: lba,
			Frames:   frames,
			Pregap:   pregap,
			File:     file.Name,
		})

		lba += frames

		// The REM follows Files[i], so the track just appended is the bridge.
		if i < len(sheet.Rems) && isHDARem(sheet.Rems[i]) {
			l.gdrom = true
			l.bridge = len(l.tracks) - 1

			if lba < disc.HDAStartLBA {
				lba = disc.HDAStartLBA
			}
		}
	}

	return l, nil
}

// A GDI gives each track's LBA outright, so the boundary is read off the table, not a REM.
func gdiTrackLayout(workingDir string, sheet *gdi.Sheet) (discLayout, error) {
	l := discLayout{gdrom: sheet.IsGDROM, bridge: -1}

	for _, t := range sheet.Tracks {
		size, err := fileSize(filepath.Join(workingDir, t.Filename))
		if err != nil {
			return discLayout{}, err
		}

		if t.LBA >= disc.HDAStartLBA && l.bridge < 0 {
			l.bridge = len(l.tracks) - 1
		}

		l.tracks = append(l.tracks, TrackDesc{
			Number:   t.Number,
			Type:     t.CUEType(),
			StartLBA: t.LBA,
			Frames:   int(size / disc.SectorBytes),
			File:     t.Filename,
		})
	}

	return l, nil
}

func isHDARem(rem string) bool {
	return strings.Contains(strings.ToUpper(rem), "HIGH-DENSITY")
}
