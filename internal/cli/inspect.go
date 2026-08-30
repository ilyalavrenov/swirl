package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"text/tabwriter"

	"github.com/urfave/cli/v3"

	"github.com/ilyalavrenov/swirl/internal/chd"
	"github.com/ilyalavrenov/swirl/internal/convert"
	"github.com/ilyalavrenov/swirl/internal/disc"
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
	File   string      `json:"file"`
	Name   string      `json:"name,omitempty"`
	Format string      `json:"format"`
	GDROM  bool        `json:"gdrom"`
	Codec  string      `json:"codec,omitempty"`
	Hunks  int         `json:"hunks,omitempty"`
	Stored int64       `json:"stored_bytes,omitempty"`
	Bytes  int64       `json:"bytes"`
	Tracks []trackJSON `json:"tracks"`
}

type verifyJSON struct {
	File         string `json:"file"`
	Hunks        int    `json:"hunks"`
	Tracks       int    `json:"tracks"`
	LogicalBytes int64  `json:"logical_bytes"`
	RawSHA1      string `json:"raw_sha1"`
	CombinedSHA1 string `json:"combined_sha1"`
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

	if desc.Name != "" {
		fmt.Fprintf(cmd.Writer, "  name    %s\n", desc.Name)
	}

	fmt.Fprintf(cmd.Writer, "  format  %s\n", desc.Format)
	fmt.Fprintf(cmd.Writer, "  layout  %s\n", layout)

	if desc.Format == convert.FormatCHD {
		fmt.Fprintf(cmd.Writer, "  codec   %s\n", desc.Codec)
		fmt.Fprintf(cmd.Writer, "  hunks   %d\n", desc.Hunks)
		fmt.Fprintf(cmd.Writer, "  stored  %s\n", storedSize(desc))
	}

	fmt.Fprintf(cmd.Writer, "  tracks  %d (%s)\n\n", len(desc.Tracks), formatBytes(desc.Bytes()))

	return writeTrackTable(cmd.Writer, desc)
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

	return infoJSON{
		File:   name,
		Name:   desc.Name,
		Format: string(desc.Format),
		GDROM:  desc.GDROM,
		Codec:  desc.Codec,
		Hunks:  desc.Hunks,
		Stored: desc.Stored,
		Bytes:  desc.Bytes(),
		Tracks: tracks,
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
		Usage: "check a CHD's hunk CRCs and SHA1 digests",
		Description: `Every hunk is decompressed and checked against the CRC stored in the map, then
both header SHA1 digests are recomputed from what was read.

Agreement means the file is undamaged and internally consistent. It does not
prove the digests match what chdman would write for the same disc.`,
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

	chdPath, format, err := detectInput(pathArg)
	if err != nil {
		return err
	}

	if format != convert.FormatCHD {
		return fmt.Errorf("%s is a %s image, want chd", filepath.Base(chdPath), format)
	}

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

	if asJSON {
		return writeJSON(cmd, verifyJSON{
			File:         filepath.Base(chdPath),
			Hunks:        report.Hunks,
			Tracks:       len(report.Tracks),
			LogicalBytes: report.LogicalBytes,
			RawSHA1:      hex.EncodeToString(report.RawSHA1),
			CombinedSHA1: hex.EncodeToString(report.CombinedSHA1),
		})
	}

	fmt.Fprintf(cmd.Writer, "  OK: %d hunks, %d %s, %s\n",
		report.Hunks, len(report.Tracks), plural(len(report.Tracks), "track"), formatBytes(report.LogicalBytes))
	fmt.Fprintf(cmd.Writer, "  raw SHA1       %x\n", report.RawSHA1)
	fmt.Fprintf(cmd.Writer, "  combined SHA1  %x\n", report.CombinedSHA1)

	return nil
}
