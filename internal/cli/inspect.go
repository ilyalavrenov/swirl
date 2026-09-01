package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/urfave/cli/v3"

	"github.com/ilyalavrenov/swirl/internal/chd"
	"github.com/ilyalavrenov/swirl/internal/convert"
	"github.com/ilyalavrenov/swirl/internal/disc"
	"github.com/ilyalavrenov/swirl/internal/redump"
)

type trackJSON struct {
	Number int    `json:"number"`
	Type   string `json:"type"`
	LBA    int    `json:"lba"`
	Frames int    `json:"frames"`
	Bytes  int64  `json:"bytes"`
	File   string `json:"file,omitempty"`
}

type infoJSON struct {
	File        string      `json:"file"`
	Name        string      `json:"name,omitempty"`
	Format      string      `json:"format"`
	GDROM       bool        `json:"gdrom"`
	HardwareID  string      `json:"hardware_id,omitempty"`
	MakerID     string      `json:"maker_id,omitempty"`
	Maker       string      `json:"maker,omitempty"`
	ProductNo   string      `json:"product_number,omitempty"`
	Version     string      `json:"version,omitempty"`
	ReleaseDate string      `json:"release_date,omitempty"`
	Disc        string      `json:"disc,omitempty"`
	Regions     []string    `json:"regions,omitempty"`
	Peripherals []string    `json:"peripherals,omitempty"`
	BootFile    string      `json:"boot_filename,omitempty"`
	Codec       string      `json:"codec,omitempty"`
	Hunks       int         `json:"hunks,omitempty"`
	Stored      int64       `json:"stored_bytes,omitempty"`
	Bytes       int64       `json:"bytes"`
	Tracks      []trackJSON `json:"tracks"`
}

type redumpJSON struct {
	DATVersion  string   `json:"dat_version,omitempty"`
	Titles      []string `json:"titles"`
	TracksKnown int      `json:"tracks_known"`
}

// CHD-only fields drop out of a CUE report.
type verifyJSON struct {
	File         string      `json:"file"`
	Hunks        int         `json:"hunks,omitempty"`
	Tracks       int         `json:"tracks"`
	LogicalBytes int64       `json:"logical_bytes,omitempty"`
	RawSHA1      string      `json:"raw_sha1,omitempty"`
	CombinedSHA1 string      `json:"combined_sha1,omitempty"`
	Redump       *redumpJSON `json:"redump,omitempty"`
}

func infoCommand() *cli.Command {
	return &cli.Command{
		Name:        "info",
		Usage:       "print a disc image's track table",
		Description: `A directory must hold exactly one image. Nothing is written.`,
		Arguments: []cli.Argument{
			&cli.StringArg{Name: argPath},
		},
		Action: runInfo,
	}
}

func runInfo(_ context.Context, cmd *cli.Command) error {
	pathArg, err := absPathArg(cmd)
	if err != nil {
		return err
	}

	imagePath, format, err := detectInput(pathArg)
	if err != nil {
		return err
	}

	desc, err := convert.Describe(format, imagePath)
	if err != nil {
		return err
	}

	if cmd.Bool(flagJSON) {
		return writeJSON(cmd, infoOutput(filepath.Base(imagePath), desc))
	}

	layout := "CD"
	if desc.GDROM {
		layout = fmt.Sprintf("GD-ROM, high-density area from LBA %d", disc.HDAStartLBA)
	}

	fmt.Fprintln(cmd.Writer, filepath.Base(imagePath))

	h := desc.IPBin

	infoRow(cmd.Writer, "name", h.Title)
	infoRow(cmd.Writer, "maker", h.Maker)
	infoRow(cmd.Writer, "product", strings.TrimSpace(h.ProductNo+" "+h.Version))
	infoRow(cmd.Writer, "disc", h.Disc)
	infoRow(cmd.Writer, "region", strings.Join(h.Regions, ", "))
	infoRow(cmd.Writer, "date", h.ReleaseDate)
	infoRow(cmd.Writer, "boot", h.BootFile)
	infoRow(cmd.Writer, "input", strings.Join(h.Peripherals, ", "))
	infoRow(cmd.Writer, "format", string(desc.Format))
	infoRow(cmd.Writer, "layout", layout)

	if desc.Format == convert.FormatCHD {
		infoRow(cmd.Writer, "codec", desc.Codec)
		infoRow(cmd.Writer, "hunks", strconv.Itoa(desc.Hunks))
		infoRow(cmd.Writer, "stored", storedSize(desc))
	}

	fmt.Fprintf(cmd.Writer, "  tracks  %d (%s)\n\n", len(desc.Tracks), formatBytes(desc.Bytes()))

	return writeTrackTable(cmd.Writer, desc)
}

func infoRow(w io.Writer, key, value string) {
	if value != "" {
		fmt.Fprintf(w, "  %-7s %s\n", key, value)
	}
}

func infoOutput(name string, desc convert.Description) infoJSON {
	tracks := make([]trackJSON, 0, len(desc.Tracks))
	for _, t := range desc.Tracks {
		tracks = append(tracks, trackJSON{
			Number: t.Number,
			Type:   t.Type,
			LBA:    t.StartLBA,
			Frames: t.Frames,
			Bytes:  t.Bytes(),
			File:   t.File,
		})
	}

	h := desc.IPBin

	return infoJSON{
		File:        name,
		Name:        h.Title,
		Format:      string(desc.Format),
		GDROM:       desc.GDROM,
		HardwareID:  h.HardwareID,
		MakerID:     h.MakerID,
		Maker:       h.Maker,
		ProductNo:   h.ProductNo,
		Version:     h.Version,
		ReleaseDate: h.ReleaseDate,
		Disc:        h.Disc,
		Regions:     h.Regions,
		Peripherals: h.Peripherals,
		BootFile:    h.BootFile,
		Codec:       desc.Codec,
		Hunks:       desc.Hunks,
		Stored:      desc.Stored,
		Bytes:       desc.Bytes(),
		Tracks:      tracks,
	}
}

func storedSize(desc convert.Description) string {
	if desc.Stored == 0 || desc.Bytes() == 0 {
		return formatBytes(desc.Stored)
	}

	return fmt.Sprintf("%s (%d%% of %s)",
		formatBytes(desc.Stored), desc.Stored*100/desc.Bytes(), formatBytes(desc.Bytes()))
}

func writeTrackTable(w io.Writer, desc convert.Description) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	// A CHD keeps its tracks in one archive: no file column to fill.
	named := slices.ContainsFunc(desc.Tracks, func(t convert.TrackDesc) bool { return t.File != "" })

	header := "  #\tTYPE\tLBA\tFRAMES\tSIZE"
	if named {
		header += "\tFILE"
	}

	fmt.Fprintln(tw, header)

	for _, t := range desc.Tracks {
		fmt.Fprintf(tw, "  %d\t%s\t%d\t%d\t%s",
			t.Number, t.Type, t.StartLBA, t.Frames, formatBytes(t.Bytes()))

		if named {
			fmt.Fprintf(tw, "\t%s", t.File)
		}

		fmt.Fprintln(tw)
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write track table: %w", err)
	}

	return nil
}

func verifyCommand() *cli.Command {
	return &cli.Command{
		Name:  "verify",
		Usage: "check a CHD's integrity, and optionally against redump",
		Description: `Hunk CRCs and header SHA1s prove a CHD is undamaged, not that it is a
known-good rip. --dat matches every track against a redump datfile, which does.
--redump fetches one over plain HTTP, unauthenticated.`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: flagDAT, Usage: "match every track against this datfile"},
			&cli.BoolFlag{Name: flagRedump, Usage: "fetch and cache redump's current datfile"},
		},
		Arguments: []cli.Argument{
			&cli.StringArg{Name: argPath},
		},
		Action: runVerify,
	}
}

func runVerify(ctx context.Context, cmd *cli.Command) error {
	pathArg, err := absPathArg(cmd)
	if err != nil {
		return err
	}

	imagePath, format, err := detectInput(pathArg)
	if err != nil {
		return err
	}

	datPath, useRedump := cmd.String(flagDAT), cmd.Bool(flagRedump)
	if datPath != "" && useRedump {
		return errors.New("pass --dat or --redump, not both")
	}

	wantDAT := datPath != "" || useRedump
	if err := verifiable(format, filepath.Base(imagePath), wantDAT); err != nil {
		return err
	}

	var dat *redump.DAT

	if wantDAT {
		if dat, err = loadDAT(ctx, cmd, datPath, useRedump); err != nil {
			return err
		}
	}

	if format == convert.FormatCUE {
		return verifyCUE(cmd, imagePath, dat)
	}

	return verifyCHD(ctx, cmd, imagePath, dat)
}

// A GDI can never match: its track files drop the pregaps redump hashes.
func verifiable(format convert.Format, name string, withDAT bool) error {
	switch {
	case format == convert.FormatCHD:
		return nil
	case format == convert.FormatCUE && withDAT:
		return nil
	case format == convert.FormatGDI && withDAT:
		return fmt.Errorf("%s is a gdi image: redump records CUE/BIN track files, which keep the pregaps a GDI drops", name)
	default:
		return fmt.Errorf("%s is a %s image, want chd", name, format)
	}
}

func loadDAT(ctx context.Context, cmd *cli.Command, datPath string, useRedump bool) (*redump.DAT, error) {
	if useRedump {
		notice := cmd.Writer
		if cmd.Bool(flagJSON) {
			notice = nil
		}

		cached, err := redump.Fetch(ctx, "swirl/"+cmd.Root().Version, notice)
		if err != nil {
			return nil, err
		}

		return redump.Load(cached)
	}

	abs, err := absPath(datPath)
	if err != nil {
		return nil, err
	}

	return redump.Load(abs)
}

func verifyCHD(ctx context.Context, cmd *cli.Command, chdPath string, dat *redump.DAT) error {
	size, err := chd.LogicalBytes(chdPath)
	if err != nil {
		return err
	}

	asJSON := cmd.Bool(flagJSON)
	if !asJSON {
		fmt.Fprintf(cmd.Writer, "Verifying %s\n", filepath.Base(chdPath))
	}

	bars := newBars(cmd)
	bar := bars.step(convert.Step{Name: "Reading CHD", Bytes: size})

	report, err := chd.Verify(ctx, chdPath, bar)

	bars.finish()

	if err != nil {
		return err
	}

	match := dat.Match(report.TrackSHA1)

	if asJSON {
		return writeJSON(cmd, verifyJSON{
			File:         filepath.Base(chdPath),
			Hunks:        report.Hunks,
			Tracks:       len(report.Tracks),
			LogicalBytes: report.LogicalBytes,
			RawSHA1:      hex.EncodeToString(report.RawSHA1),
			CombinedSHA1: hex.EncodeToString(report.CombinedSHA1),
			Redump:       redumpOutput(dat, match),
		})
	}

	fmt.Fprintf(cmd.Writer, "  OK: %d hunks, %d %s, %s\n",
		report.Hunks, len(report.Tracks), plural(len(report.Tracks), "track"), formatBytes(report.LogicalBytes))
	fmt.Fprintf(cmd.Writer, "  raw SHA1       %x\n", report.RawSHA1)
	fmt.Fprintf(cmd.Writer, "  combined SHA1  %x\n", report.CombinedSHA1)

	writeRedump(cmd.Writer, dat, match)

	return nil
}

func verifyCUE(cmd *cli.Command, cuePath string, dat *redump.DAT) error {
	desc, err := convert.Describe(convert.FormatCUE, cuePath)
	if err != nil {
		return err
	}

	asJSON := cmd.Bool(flagJSON)
	if !asJSON {
		fmt.Fprintf(cmd.Writer, "Verifying %s\n", filepath.Base(cuePath))
	}

	bars := newBars(cmd)
	bar := bars.step(convert.Step{Name: "Hashing tracks", Bytes: desc.Bytes()})

	sums, err := convert.TrackSHA1(cuePath, desc, bar)

	bars.finish()

	if err != nil {
		return err
	}

	match := dat.Match(sums)

	if asJSON {
		return writeJSON(cmd, verifyJSON{
			File:   filepath.Base(cuePath),
			Tracks: len(sums),
			Redump: redumpOutput(dat, match),
		})
	}

	fmt.Fprintf(cmd.Writer, "  %d %s, %s\n", len(sums), plural(len(sums), "track"), formatBytes(desc.Bytes()))

	writeRedump(cmd.Writer, dat, match)

	return nil
}

func redumpOutput(dat *redump.DAT, res redump.Result) *redumpJSON {
	if dat == nil {
		return nil
	}

	return &redumpJSON{DATVersion: dat.Version, Titles: res.Titles, TracksKnown: res.KnownCount()}
}

func writeRedump(w io.Writer, dat *redump.DAT, res redump.Result) {
	if dat == nil {
		return
	}

	if len(res.Titles) > 0 {
		fmt.Fprintf(w, "  redump         %s\n", strings.Join(res.Titles, ", "))

		return
	}

	// No match is a normal answer; the known count separates unlisted from wrong.
	fmt.Fprintf(w, "  redump         no match (%d of %d tracks known)\n", res.KnownCount(), len(res.Known))
}
