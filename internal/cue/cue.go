package cue

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	// A TRACK or INDEX line: directive, number, type or timecode.
	lineMinParts = 3

	msfPartsCount = 3

	// MSF timecodes are base 75, not base 100.
	framesPerSec = 75

	secsPerMin = 60

	remPrefixLen = 4
)

type MSF struct {
	Min, Sec, Frame int
}

// FramesAsMSF renders a frame count as the timecode a cue sheet carries.
func FramesAsMSF(frames int) string {
	return fmt.Sprintf("%02d:%02d:%02d",
		frames/(framesPerSec*secsPerMin), frames/framesPerSec%secsPerMin, frames%framesPerSec)
}

type Track struct {
	Number  int
	Type    string // e.g. "MODE1/2352", "AUDIO"
	Indexes []MSF  // INDEX times in file order; INDEX 01 is Indexes[1] when a pregap is declared
}

type File struct {
	Name   string
	Tracks []Track
}

// Rems[i] is the REM following Files[i]; that alignment locates the high-density boundary.
type Sheet struct {
	Files []File
	Rems  []string
}

// A file that yields no tracks fails here rather than as an empty conversion.
//
//nolint:gocognit // complexity reflects the number of CUE directive types handled
func Parse(absPath string) (*Sheet, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open cue %s: %w", absPath, err)
	}
	defer f.Close()

	sheet := &Sheet{}

	var currentTrack *Track

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "FILE "):
			sheet.Files = append(sheet.Files, File{Name: extractQuotedString(line)})
			sheet.Rems = append(sheet.Rems, "")
			currentTrack = nil

		case strings.HasPrefix(upper, "TRACK "):
			if len(sheet.Files) == 0 {
				continue
			}

			parts := strings.Fields(line)
			if len(parts) < lineMinParts {
				continue
			}

			num, parseErr := strconv.Atoi(parts[1])
			if parseErr != nil {
				return nil, fmt.Errorf("invalid track number %q in %s: %w", parts[1], absPath, parseErr)
			}

			currentFile := &sheet.Files[len(sheet.Files)-1]
			currentFile.Tracks = append(currentFile.Tracks, Track{Number: num, Type: parts[2]})
			currentTrack = &currentFile.Tracks[len(currentFile.Tracks)-1]

		case strings.HasPrefix(upper, "INDEX "):
			if currentTrack == nil {
				continue
			}

			parts := strings.Fields(line)
			if len(parts) < lineMinParts {
				continue
			}

			if _, parseErr := strconv.Atoi(parts[1]); parseErr != nil {
				return nil, fmt.Errorf("invalid index number %q in %s: %w", parts[1], absPath, parseErr)
			}

			msf, msfErr := parseMSF(parts[2])
			if msfErr != nil {
				return nil, fmt.Errorf("invalid MSF %q in %s: %w", parts[2], absPath, msfErr)
			}

			currentTrack.Indexes = append(currentTrack.Indexes, msf)

		case strings.HasPrefix(upper, "REM "):
			if len(sheet.Rems) > 0 {
				sheet.Rems[len(sheet.Rems)-1] = line[remPrefixLen:]
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", absPath, err)
	}

	if sheet.TrackCount() == 0 {
		return nil, fmt.Errorf("%s: no tracks found, not a CUE sheet", absPath)
	}

	return sheet, nil
}

func (s *Sheet) TrackCount() int {
	total := 0
	for _, f := range s.Files {
		total += len(f.Tracks)
	}

	return total
}

func MSFToFrames(msf MSF) int {
	return msf.Frame + (msf.Sec * framesPerSec) + ((msf.Min * secsPerMin) * framesPerSec)
}

func extractQuotedString(line string) string {
	start := strings.Index(line, `"`)
	if start < 0 {
		return ""
	}

	end := strings.LastIndex(line, `"`)
	if end <= start {
		return ""
	}

	return line[start+1 : end]
}

func parseMSF(s string) (MSF, error) {
	parts := strings.Split(s, ":")
	if len(parts) != msfPartsCount {
		return MSF{}, fmt.Errorf("invalid MSF timecode %q: want MM:SS:FF", s)
	}

	var v [msfPartsCount]int

	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return MSF{}, fmt.Errorf("invalid MSF %q: %w", s, err)
		}

		v[i] = n
	}

	if v[0] < 0 || v[1] < 0 || v[1] >= secsPerMin || v[2] < 0 || v[2] >= framesPerSec {
		return MSF{}, fmt.Errorf("MSF %q is out of range: seconds < %d, frames < %d", s, secsPerMin, framesPerSec)
	}

	return MSF{Min: v[0], Sec: v[1], Frame: v[2]}, nil
}
