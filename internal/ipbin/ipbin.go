package ipbin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	nameLength = 128

	// https://mc.pp.se/dc/ip.bin.html
	nameOffset = 0x90
)

func ProductName(binPath string) (string, error) {
	f, err := os.Open(binPath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", binPath, err)
	}
	defer f.Close()

	name, err := ProductNameAt(f)
	if err != nil {
		return "", fmt.Errorf("%s: %w", binPath, err)
	}

	return name, nil
}

// r's first byte must be the start of track 1's first sector.
func ProductNameAt(r io.ReaderAt) (string, error) {
	buf := make([]byte, nameLength)

	n, err := r.ReadAt(buf, nameOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read name: %w", err)
	}

	return strings.TrimSpace(strings.TrimRight(string(buf[:n]), "\x00")), nil
}
