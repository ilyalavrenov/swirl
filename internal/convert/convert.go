package convert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ilyalavrenov/swirl/internal/chd"
	"github.com/ilyalavrenov/swirl/internal/cue"
	"github.com/ilyalavrenov/swirl/internal/disc"
	"github.com/ilyalavrenov/swirl/internal/gdi"
)

type Format string

const (
	FormatCUE Format = "cue"
	FormatGDI Format = "gdi"
	FormatCHD Format = "chd"
)

const (
	dirMode  = 0o755
	fileMode = 0o644

	hdaRem = "REM HIGH-DENSITY AREA"
)

type Step struct {
	// 1-based; Total is 0 for a single unnumbered step.
	Index, Total int

	Name  string // track filename, or a label such as "Writing CHD"
	Audio bool
	Bytes int64
}

type Progress func(Step) io.Writer

type Options struct {
	Force bool

	// The compressor for CHD output. Empty means chd.CodecFLAC.
	Codec chd.Codec

	// Called once per step, before it runs. Nil reports nothing.
	Progress Progress
}

type Result struct {
	Path   string // the sheet or archive that was written
	Tracks int
	Bytes  int64 // track bytes processed, not the size on disk
}

func (o Options) codec() chd.Codec {
	if o.Codec == "" {
		return chd.CodecFLAC
	}

	return o.Codec
}

// step never returns nil.
func (o Options) step(s Step) io.Writer {
	if o.Progress == nil {
		return io.Discard
	}

	return o.Progress(s)
}

// outputPath is a directory for cue and gdi output, a file for chd. Cancelling
// ctx leaves partial output in place.
func Run(ctx context.Context, from, to Format, inputPath, outputPath string, opts Options) (Result, error) {
	switch {
	case from == FormatCUE && to == FormatGDI:
		return cueToGDI(ctx, inputPath, outputPath, opts)
	case from == FormatCUE && to == FormatCHD:
		return cueToCHD(ctx, inputPath, outputPath, opts)
	case from == FormatGDI && to == FormatCUE:
		return gdiToCUE(ctx, inputPath, outputPath, opts)
	case from == FormatGDI && to == FormatCHD:
		return gdiToCHD(ctx, inputPath, outputPath, opts)
	case from == FormatCHD && to == FormatCUE:
		return chdToSheet(ctx, inputPath, outputPath, "disc.cue", cueSheetFor, opts)
	case from == FormatCHD && to == FormatGDI:
		return chdToSheet(ctx, inputPath, outputPath, "disc.gdi", gdiSheetFor, opts)
	default:
		return Result{}, fmt.Errorf("converting %s to %s is not supported", from, to)
	}
}

// Pregaps are stripped: a GDI records each track's start sector instead.
func cueToGDI(ctx context.Context, cuePath, outputDir string, opts Options) (Result, error) {
	sheet, err := cue.Parse(cuePath)
	if err != nil {
		return Result{}, err
	}

	workingDir := filepath.Dir(cuePath)

	l, err := cueTrackLayout(workingDir, sheet)
	if err != nil {
		return Result{}, err
	}

	tracks := l.tracks

	out, err := newStagedDir(outputDir, opts.Force)
	if err != nil {
		return Result{}, err
	}
	defer out.cleanup()

	lines := make([]string, 0, len(tracks)+1)
	lines = append(lines, strconv.Itoa(len(tracks)))

	var totalBytes int64

	for i, t := range tracks {
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("convert %s: %w", cuePath, err)
		}

		dstName := disc.TrackFileName(t.Number, t.Type)

		bar := opts.step(Step{
			Index: i + 1,
			Total: len(tracks),
			Name:  dstName,
			Audio: disc.IsAudio(t.Type),
			Bytes: t.Bytes(),
		})

		written, err := copyTrack(out.path(dstName), filepath.Join(workingDir, t.File), t.Pregap, bar)
		if err != nil {
			return Result{}, err
		}

		totalBytes += written

		lines = append(lines, gdiLine(t.Number, t.StartLBA, t.Type, dstName))
	}

	if err := writeSheet(out.path("disc.gdi"), lines); err != nil {
		return Result{}, err
	}

	if err := out.commit(); err != nil {
		return Result{}, err
	}

	return Result{Path: filepath.Join(outputDir, "disc.gdi"), Tracks: len(tracks), Bytes: totalBytes}, nil
}

func gdiToCUE(ctx context.Context, gdiPath, outputDir string, opts Options) (Result, error) {
	sheet, err := gdi.Parse(gdiPath)
	if err != nil {
		return Result{}, err
	}

	out, err := newStagedDir(outputDir, opts.Force)
	if err != nil {
		return Result{}, err
	}
	defer out.cleanup()

	workingDir := filepath.Dir(gdiPath)

	l, err := gdiTrackLayout(workingDir, sheet)
	if err != nil {
		return Result{}, err
	}

	total := len(l.tracks)

	var (
		lines      []string
		totalBytes int64
	)

	for i, t := range l.tracks {
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("convert %s: %w", gdiPath, err)
		}

		dstName := disc.TrackFileName(t.Number, t.Type)

		bar := opts.step(Step{
			Index: i + 1,
			Total: total,
			Name:  dstName,
			Audio: disc.IsAudio(t.Type),
			Bytes: t.Bytes(),
		})

		copied, err := copyTrack(out.path(dstName), filepath.Join(workingDir, t.File), 0, bar)
		if err != nil {
			return Result{}, err
		}

		totalBytes += copied

		if l.gdrom && i == l.bridge+1 {
			lines = append(lines, hdaRem)
		}

		lines = append(lines, cueStanza(t.Number, t.Type, dstName, 0)...)
	}

	if err := writeSheet(out.path("disc.cue"), lines); err != nil {
		return Result{}, err
	}

	if err := out.commit(); err != nil {
		return Result{}, err
	}

	return Result{Path: filepath.Join(outputDir, "disc.cue"), Tracks: total, Bytes: totalBytes}, nil
}

func chdToSheet(
	ctx context.Context,
	chdPath, outputDir, sheetName string,
	sheetFor func([]chd.TrackInfo) []string,
	opts Options,
) (Result, error) {
	size, err := chd.LogicalBytes(chdPath)
	if err != nil {
		return Result{}, err
	}

	out, err := newStagedDir(outputDir, opts.Force)
	if err != nil {
		return Result{}, err
	}
	defer out.cleanup()

	bar := opts.step(Step{Name: "Reading CHD", Bytes: size})

	tracks, err := chd.Read(ctx, chdPath, out.work, bar)
	if err != nil {
		return Result{}, fmt.Errorf("read CHD: %w", err)
	}

	var totalBytes int64
	for _, t := range tracks {
		totalBytes += int64(t.RealFrames) * disc.SectorBytes
	}

	if err := writeSheet(out.path(sheetName), sheetFor(tracks)); err != nil {
		return Result{}, err
	}

	if err := out.commit(); err != nil {
		return Result{}, err
	}

	return Result{Path: filepath.Join(outputDir, sheetName), Tracks: len(tracks), Bytes: totalBytes}, nil
}

func cueToCHD(ctx context.Context, cuePath, outputPath string, opts Options) (Result, error) {
	sheet, err := cue.Parse(cuePath)
	if err != nil {
		return Result{}, err
	}

	l, err := cueTrackLayout(filepath.Dir(cuePath), sheet)
	if err != nil {
		return Result{}, err
	}

	tracks, closeAll, err := openTracks(filepath.Dir(cuePath), l.tracks)
	if err != nil {
		return Result{}, err
	}
	defer closeAll()

	shiftPregaps(tracks, l.bridge >= 0)
	padBridge(tracks, l.bridge)

	return writeCHD(ctx, outputPath, tracks, opts)
}

func gdiToCHD(ctx context.Context, gdiPath, outputPath string, opts Options) (Result, error) {
	sheet, err := gdi.Parse(gdiPath)
	if err != nil {
		return Result{}, err
	}

	l, err := gdiTrackLayout(filepath.Dir(gdiPath), sheet)
	if err != nil {
		return Result{}, err
	}

	tracks, closeAll, err := openTracks(filepath.Dir(gdiPath), l.tracks)
	if err != nil {
		return Result{}, err
	}
	defer closeAll()

	shiftPregaps(tracks, l.bridge >= 0)
	padBridge(tracks, l.bridge)

	return writeCHD(ctx, outputPath, tracks, opts)
}

func writeCHD(ctx context.Context, outputPath string, tracks []chd.Track, opts Options) (Result, error) {
	if err := prepareOutputFile(outputPath, opts.Force); err != nil {
		return Result{}, err
	}

	bar := opts.step(Step{Name: "Writing CHD", Bytes: chd.WriteBytes(tracks)})

	if err := chd.Write(ctx, outputPath, tracks, opts.codec(), bar); err != nil {
		return Result{}, fmt.Errorf("write CHD: %w", err)
	}

	var totalBytes int64
	for _, t := range tracks {
		totalBytes += int64(t.Frames) * disc.SectorBytes
	}

	return Result{Path: outputPath, Tracks: len(tracks), Bytes: totalBytes}, nil
}

// The lead-in before INDEX 01 holds the previous track's run-out, not this track's data.
func trackPregap(track cue.Track) int {
	if len(track.Indexes) > 1 {
		return cue.MSFToFrames(track.Indexes[1])
	}

	return 0
}

// The returned func closes every file opened here.
func openTracks(workingDir string, layout []TrackDesc) ([]chd.Track, func(), error) {
	var (
		tracks []chd.Track
		open   []*os.File
	)

	closeAll := func() {
		for _, f := range open {
			f.Close()
		}
	}

	for _, t := range layout {
		f, err := os.Open(filepath.Join(workingDir, t.File))
		if err != nil {
			closeAll()

			return nil, nil, fmt.Errorf("track %d: %w", t.Number, err)
		}

		open = append(open, f)
		tracks = append(tracks, chd.Track{
			Number: t.Number,
			Type:   t.Type,
			// TrackDesc.Frames excludes the pregap; CHD stores the whole file.
			Frames: t.Frames + t.Pregap,
			Pregap: t.Pregap,
			Data:   f,
		})
	}

	return tracks, closeAll, nil
}

// Only a GD-ROM can hold a pregap for the next track: CHT2 carries FRAMES alone, with no
// PAD to set those frames apart. Reads stay sequential, so the earlier track just takes
// the head of the later one's file. Runs before padBridge, which sizes from these lengths.
func shiftPregaps(tracks []chd.Track, gdrom bool) {
	if !gdrom {
		return
	}

	for i := 1; i < len(tracks); i++ {
		pregap := tracks[i].Pregap
		if pregap == 0 {
			continue
		}

		tracks[i].Frames -= pregap
		tracks[i-1].TrailingPregap = pregap
		tracks[i-1].Data = io.MultiReader(
			tracks[i-1].Data,
			io.LimitReader(tracks[i].Data, int64(pregap)*disc.SectorBytes),
		)
	}
}

// Inflates tracks[idx] so the tracks up to and including it fill exactly
// disc.HDAStartLBA sectors. idx is negative when the disc has no high-density area.
func padBridge(tracks []chd.Track, idx int) {
	if idx < 0 || idx >= len(tracks) {
		return
	}

	sumPrev := 0
	for _, t := range tracks[:idx] {
		sumPrev += chd.PadFrames(t.Frames + t.TrailingPregap)
	}

	// >=, not >: StoredFrames doubles as the CHGD tag, and a GD-ROM read as CHT2 has its
	// sector addressing shifted.
	if required := disc.HDAStartLBA - sumPrev; required >= tracks[idx].Frames {
		tracks[idx].StoredFrames = required
	}
}

// The high-density area is found by running sector count, not track number: a plain
// CD can have three tracks too.
func cueSheetFor(tracks []chd.TrackInfo) []string {
	var lines []string

	startLBA := 0

	for _, t := range tracks {
		if t.GDROM && startLBA == disc.HDAStartLBA {
			lines = append(lines, hdaRem)
		}

		lines = append(lines, cueStanza(t.Number, t.CUEType, disc.TrackFileName(t.Number, t.CUEType), t.Pregap)...)
		startLBA += t.TotalFrames
	}

	return lines
}

// Start LBAs advance by TotalFrames, not RealFrames: bridge padding is sectors too.
func gdiSheetFor(tracks []chd.TrackInfo) []string {
	lines := make([]string, 0, len(tracks)+1)
	lines = append(lines, strconv.Itoa(len(tracks)))

	startLBA := 0

	for _, t := range tracks {
		lines = append(lines, gdiLine(t.Number, startLBA, t.CUEType, disc.TrackFileName(t.Number, t.CUEType)))
		startLBA += t.TotalFrames
	}

	return lines
}

// Trailing zero is the in-file offset, always 0 once pregaps are stripped.
func gdiLine(number, startLBA int, cueType, filename string) string {
	trackType := gdi.TrackTypeData
	if disc.IsAudio(cueType) {
		trackType = gdi.TrackTypeAudio
	}

	return fmt.Sprintf("%d %d %d %d %s 0 ", number, startLBA, trackType, disc.SectorBytes, filename)
}

func cueStanza(number int, cueType, filename string, pregap int) []string {
	// AUDIO is a TRACK datatype, not a FILE type; chdman rejects a sheet using it there.
	lines := []string{
		fmt.Sprintf("FILE %q BINARY", filename),
		fmt.Sprintf("  TRACK %02d %s", number, cueType),
	}

	// INDEX 00, not a PREGAP command: the frames are in the file, and a burner told
	// otherwise would write a second copy of them.
	if pregap > 0 {
		lines = append(lines, "    INDEX 00 00:00:00", "    INDEX 01 "+cue.FramesAsMSF(pregap))
	} else {
		lines = append(lines, "    INDEX 01 00:00:00")
	}

	return lines
}

func writeSheet(path string, lines []string) error {
	content := strings.Join(lines, "\n") + "\n"

	if err := os.WriteFile(path, []byte(content), fileMode); err != nil {
		return fmt.Errorf("sheet: %w", err)
	}

	return nil
}

func copyTrack(dstPath, srcPath string, pregap int, progress io.Writer) (int64, error) {
	in, err := os.Open(srcPath)
	if err != nil {
		return 0, fmt.Errorf("source: %w", err)
	}
	defer in.Close()

	if pregap > 0 {
		if _, err := in.Seek(int64(pregap)*disc.SectorBytes, io.SeekStart); err != nil {
			return 0, fmt.Errorf("skip pregap: %w", err)
		}
	}

	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return 0, fmt.Errorf("destination: %w", err)
	}
	// Closed below so a failed flush is reported; this defer covers only the error paths.
	defer out.Close()

	copied, err := io.Copy(io.MultiWriter(out, progress), in)
	if err != nil {
		return 0, fmt.Errorf("copy %s: %w", srcPath, err)
	}

	if err := out.Close(); err != nil {
		return 0, fmt.Errorf("flush: %w", err)
	}

	return copied, nil
}

// Built beside its destination and moved in only once the conversion finishes: an
// interrupted in-place write would destroy the old tree without producing a new one.
type stagedDir struct {
	work      string
	final     string
	committed bool
}

func newStagedDir(final string, force bool) (*stagedDir, error) {
	entries, err := os.ReadDir(final)

	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read output directory %s: %w", final, err)
	case len(entries) > 0 && !force:
		return nil, fmt.Errorf("output directory %s is not empty, pass --force to replace its contents", final)
	}

	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, dirMode); err != nil {
		return nil, fmt.Errorf("create output directory %s: %w", final, err)
	}

	// In the destination's own parent so committing is a rename within one filesystem.
	work, err := os.MkdirTemp(parent, "."+filepath.Base(final)+".swirl-*")
	if err != nil {
		return nil, fmt.Errorf("create staging directory for %s: %w", final, err)
	}

	// MkdirTemp opens at 0700; the finished directory should look like any other.
	if err := os.Chmod(work, dirMode); err != nil {
		return nil, fmt.Errorf("set mode on staging directory for %s: %w", final, err)
	}

	return &stagedDir{work: work, final: final}, nil
}

func (s *stagedDir) path(name string) string {
	return filepath.Join(s.work, name)
}

func (s *stagedDir) commit() error {
	if err := os.RemoveAll(s.final); err != nil {
		return fmt.Errorf("replace output directory %s: %w", s.final, err)
	}

	if err := os.Rename(s.work, s.final); err != nil {
		return fmt.Errorf("move %s into place: %w", s.final, err)
	}

	s.committed = true

	return nil
}

func (s *stagedDir) cleanup() {
	if !s.committed {
		os.RemoveAll(s.work)
	}
}

func prepareOutputFile(path string, force bool) error {
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("output file %s already exists, pass --force to replace it", path)
	}

	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return fmt.Errorf("create output directory for %s: %w", path, err)
	}

	return nil
}

func fileSize(path string) (int64, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("track size: %w", err)
	}

	return stat.Size(), nil
}
