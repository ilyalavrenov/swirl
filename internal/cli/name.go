package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/ilyalavrenov/swirl/internal/gdi"
	"github.com/ilyalavrenov/swirl/internal/ipbin"
)

// Matches the mode converted track files land as.
const nameFileMode = 0o644

func writeGameName(gdiPath string) (string, error) {
	sheet, err := gdi.Parse(gdiPath)
	if err != nil {
		return "", err
	}

	// Only track 1 carries the IP.BIN bootstrap.
	i := slices.IndexFunc(sheet.Tracks, func(t gdi.Track) bool { return t.Number == 1 })
	if i < 0 {
		return "", fmt.Errorf("%s: no track 1 to read a name from", gdiPath)
	}

	workingDir := filepath.Dir(gdiPath)

	name, err := ipbin.ProductName(filepath.Join(workingDir, sheet.Tracks[i].Filename))
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(filepath.Join(workingDir, "name.txt"), []byte(name), nameFileMode); err != nil {
		return "", fmt.Errorf("write name.txt: %w", err)
	}

	return name, nil
}
