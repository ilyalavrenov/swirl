package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/ilyalavrenov/swirl/internal/convert"
)

const (
	flagTo    = "to"
	flagForce = "force"
	flagCodec = "codec"
	flagJSON  = "json"

	argInput  = "input"
	argOutput = "output"
	argPath   = "path"
)

const (
	barWidth    = 30
	barThrottle = 65 * time.Millisecond

	// Pads track names so the bars line up.
	descWidth = 20
)

func Run(ctx context.Context, version string, osArgs []string) error {
	return command(version).Run(ctx, osArgs) //nolint:wrapcheck // error is already contextual
}

func command(version string) *cli.Command {
	return &cli.Command{
		Name:                  "swirl",
		Usage:                 "convert Dreamcast disc images between CUE/BIN, GDI, and CHD",
		Version:               version,
		EnableShellCompletion: true,
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: flagJSON, Usage: "print machine-readable output"},
		},
		Commands: []*cli.Command{
			convertCommand(),
			infoCommand(),
			verifyCommand(),
		},
	}
}

func findByExt(root, ext string) ([]string, error) {
	var found []string

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ext) {
			found = append(found, path)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("search %s: %w", root, err)
	}

	return found, nil
}

// A nil *bars renders nothing.
type bars struct {
	w   io.Writer
	bar *bar
}

// Nil off a terminal: the bars redraw with carriage returns, which a pipe cannot show.
func newBars(cmd *cli.Command) *bars {
	if cmd.Bool(flagJSON) {
		return nil
	}

	f, ok := cmd.Writer.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil
	}

	return &bars{w: cmd.Writer}
}

func (b *bars) step(s convert.Step) io.Writer {
	if b == nil {
		return io.Discard
	}

	b.finish()

	desc := "  " + s.Name

	if s.Total > 0 {
		kind := "data"
		if s.Audio {
			kind = "audio"
		}

		desc = fmt.Sprintf("  [%d/%d] %-*s (%s)", s.Index, s.Total, descWidth, s.Name, kind)
	}

	b.bar = &bar{w: b.w, desc: desc, total: s.Bytes}
	b.bar.render()

	return b.bar
}

// Terminating the line matters when a step processed fewer bytes than predicted.
func (b *bars) finish() {
	if b == nil || b.bar == nil {
		return
	}

	b.bar.render()
	fmt.Fprintln(b.w)

	b.bar = nil
}

// Writes are counted, never forwarded: bar is only ever the tail of an io.MultiWriter.
type bar struct {
	w        io.Writer
	desc     string
	total    int64
	written  int64
	lastDraw time.Time
}

func (b *bar) Write(p []byte) (int, error) {
	b.written += int64(len(p))

	// Redrawing per write would spend more time on escape codes than on the disc.
	if time.Since(b.lastDraw) >= barThrottle {
		b.render()
	}

	return len(p), nil
}

func (b *bar) render() {
	b.lastDraw = time.Now()

	filled := barWidth
	if b.total > 0 {
		filled = min(int(int64(barWidth)*b.written/b.total), barWidth)
	}

	fmt.Fprintf(b.w, "\r%s %s%s %s / %s",
		b.desc, strings.Repeat("█", filled), strings.Repeat("░", barWidth-filled),
		formatBytes(b.written), formatBytes(b.total))
}

const (
	bytesPerGiB = 1 << 30
	bytesPerMiB = 1 << 20
	bytesPerKiB = 1 << 10
)

func formatBytes(n int64) string {
	switch {
	case n >= bytesPerGiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(bytesPerGiB))
	case n >= bytesPerMiB:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(bytesPerMiB))
	case n >= bytesPerKiB:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(bytesPerKiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func writeJSON(cmd *cli.Command, out any) error {
	encoder := json.NewEncoder(cmd.Writer)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(out); err != nil {
		return fmt.Errorf("write json: %w", err)
	}

	return nil
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}

	return word + "s"
}
