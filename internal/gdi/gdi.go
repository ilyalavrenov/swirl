package gdi

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ilyalavrenov/swirl/internal/disc"
)

const (
	// A track line: number, LBA, type, block size, filename, offset.
	lineMinParts = 6

	TrackTypeAudio = 0
	TrackTypeData  = 4
)

type Track struct {
	Number    int
	LBA       int
	TrackType int
	Filename  string // relative to the .gdi file's directory
}

type Sheet struct {
	Tracks  []Track
	IsGDROM bool // any track begins at or past disc.HDAStartLBA
}

func Parse(gdiPath string) (*Sheet, error) {
	f, err := os.Open(gdiPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", gdiPath, err)
	}
	defer f.Close()

	sheet := &Sheet{}
	scanner := bufio.NewScanner(f)

	// The leading track count is not trusted; the per-track lines are authoritative.
	scanner.Scan()

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := splitFields(line)
		if len(parts) < lineMinParts {
			continue
		}

		num, numErr := strconv.Atoi(parts[0])
		lba, lbaErr := strconv.Atoi(parts[1])
		trackType, typeErr := strconv.Atoi(parts[2])
		_, sizeErr := strconv.Atoi(parts[3]) // block size: always the raw sector size

		if numErr != nil || lbaErr != nil || typeErr != nil || sizeErr != nil {
			continue
		}

		if lba >= disc.HDAStartLBA {
			sheet.IsGDROM = true
		}

		sheet.Tracks = append(sheet.Tracks, Track{
			Number:    num,
			LBA:       lba,
			TrackType: trackType,
			Filename:  parts[4],
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", gdiPath, err)
	}

	if len(sheet.Tracks) == 0 {
		return nil, fmt.Errorf("%s: no tracks found, not a GDI file", gdiPath)
	}

	return sheet, nil
}

// GDI writers quote filenames containing spaces; strings.Fields would split those
// into extra fields that slip past the minimum-count check.
func splitFields(line string) []string {
	quoted := false

	parts := strings.FieldsFunc(line, func(r rune) bool {
		if r == '"' {
			quoted = !quoted
		}

		return !quoted && r == ' '
	})

	for i, p := range parts {
		parts[i] = strings.Trim(p, `"`)
	}

	return parts
}

func (t Track) CUEType() string {
	if t.TrackType == TrackTypeAudio {
		return disc.TrackTypeAudio
	}

	return disc.TrackTypeMode1
}
