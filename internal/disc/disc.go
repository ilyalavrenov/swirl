package disc

import (
	"fmt"
	"strings"
)

const (
	// Dreamcast images always store full raw sectors, never cooked 2048-byte ones.
	SectorBytes = 2352

	// Where a GD-ROM's high-density area begins.
	HDAStartLBA = 45000
)

// CUE TRACK types; GDI and CHD type codes translate to these.
const (
	TrackTypeAudio = "AUDIO"
	TrackTypeMode1 = "MODE1/2352"
	TrackTypeMode2 = "MODE2/2352"
)

func IsAudio(cueType string) bool {
	return strings.EqualFold(cueType, TrackTypeAudio)
}

func TrackFileName(number int, cueType string) string {
	ext := "bin"
	if IsAudio(cueType) {
		ext = "raw"
	}

	return fmt.Sprintf("track%02d.%s", number, ext)
}
