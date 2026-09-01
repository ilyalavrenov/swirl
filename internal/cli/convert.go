package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/ilyalavrenov/swirl/internal/chd"
	"github.com/ilyalavrenov/swirl/internal/convert"
)

func convertCommand() *cli.Command {
	return &cli.Command{
		Name:  "convert",
		Usage: "convert a disc image between cue, gdi, and chd",
		Description: `An input directory must hold exactly one image. <output> is a file for chd and
a directory for cue and gdi.

The output format comes from the output extension, defaulting to gdi; --to
overrides it. Supported: cue to gdi or chd, gdi to cue or chd, chd to cue.

--codec sets chd compression, spelled as chdman spells a codec set. The default
cdzl,cdfl takes the smaller of deflate and FLAC on audio hunks; cdzl deflates
everything, for a slightly faster write.`,
		Arguments: []cli.Argument{
			&cli.StringArg{Name: argInput},
			&cli.StringArg{Name: argOutput},
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    flagTo,
				Aliases: []string{"t"},
				Usage:   "output format: `FORMAT` is cue, gdi, or chd",
			},
			&cli.BoolFlag{
				Name:  flagForce,
				Usage: "replace an existing output file or a non-empty output directory",
			},
			&cli.StringFlag{
				Name:  flagCodec,
				Usage: "chd compressor: `CODEC` is " + codecList(),
				Value: string(chd.CodecFLAC),
			},
		},
		Action: runConvert,
	}
}

func runConvert(ctx context.Context, cmd *cli.Command) error {
	inputArg, outputArg := cmd.StringArg(argInput), cmd.StringArg(argOutput)
	if inputArg == "" || outputArg == "" {
		return errors.New("both <input> and <output> are required")
	}

	inputArg, err := absPath(inputArg)
	if err != nil {
		return err
	}

	outputArg, err = absPath(outputArg)
	if err != nil {
		return err
	}

	inputPath, from, err := detectInput(inputArg)
	if err != nil {
		return err
	}

	to, err := detectOutput(outputArg, cmd.String(flagTo))
	if err != nil {
		return err
	}

	if from == to {
		return fmt.Errorf("input and output are both %s, nothing to do", from)
	}

	codec, err := chd.ParseCodec(cmd.String(flagCodec))
	if err != nil {
		return err
	}

	asJSON := cmd.Bool(flagJSON)
	if !asJSON {
		fmt.Fprintf(cmd.Writer, "Converting %s\n", filepath.Base(inputPath))
	}

	bars := newBars(cmd)

	result, err := convert.Run(ctx, from, to, inputPath, outputArg, convert.Options{
		Force:    cmd.Bool(flagForce),
		Codec:    codec,
		Progress: bars.step,
	})

	bars.finish()

	if err != nil {
		return err
	}

	out := convertJSON{
		Output: result.Path,
		Format: string(to),
		Tracks: result.Tracks,
		Bytes:  result.Bytes,
	}

	if !asJSON {
		fmt.Fprintf(cmd.Writer, "  Written: %s (%d %s · %s)\n",
			result.Path, result.Tracks, plural(result.Tracks, "track"), formatBytes(result.Bytes))
	}

	// ODE menus read the game name from a name.txt beside the sheet.
	if to == convert.FormatGDI {
		name, nameErr := writeGameName(result.Path)

		switch {
		case nameErr != nil:
			fmt.Fprintf(cmd.ErrWriter, "  warning: could not write name.txt: %v\n", nameErr)
		case asJSON:
			out.Name = name
		default:
			fmt.Fprintf(cmd.Writer, "  Name: %s\n", name)
		}
	}

	if asJSON {
		return writeJSON(cmd, out)
	}

	return nil
}

type convertJSON struct {
	Output string `json:"output"`
	Format string `json:"format"`
	Tracks int    `json:"tracks"`
	Bytes  int64  `json:"bytes"`
	Name   string `json:"name,omitempty"`
}

func absPathArg(cmd *cli.Command) (string, error) {
	arg := cmd.StringArg(argPath)
	if arg == "" {
		return "", errors.New("<path> is required")
	}

	return absPath(arg)
}

func absPath(arg string) (string, error) {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", arg, err)
	}

	return abs, nil
}

func detectInput(arg string) (string, convert.Format, error) {
	info, err := os.Stat(arg)
	if err != nil {
		return "", "", fmt.Errorf("input: %w", err)
	}

	if !info.IsDir() {
		format, err := parseFormat(strings.TrimPrefix(filepath.Ext(arg), "."))
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", filepath.Base(arg), err)
		}

		return arg, format, nil
	}

	for _, format := range []convert.Format{convert.FormatCUE, convert.FormatGDI, convert.FormatCHD} {
		found, err := findByExt(arg, "."+string(format))
		if err != nil {
			return "", "", err
		}

		switch {
		case len(found) == 1:
			return found[0], format, nil
		case len(found) > 1:
			return "", "", fmt.Errorf("found %d .%s files in %s, expected exactly one", len(found), format, arg)
		}
	}

	return "", "", fmt.Errorf("no .cue, .gdi, or .chd file found in %s", arg)
}

func detectOutput(outputArg, toFlag string) (convert.Format, error) {
	if toFlag != "" {
		format, err := parseFormat(toFlag)
		if err != nil {
			return "", fmt.Errorf("--to: %w", err)
		}

		return format, nil
	}

	if ext := strings.TrimPrefix(filepath.Ext(outputArg), "."); ext != "" {
		if format, err := parseFormat(ext); err == nil {
			return format, nil
		}
	}

	return convert.FormatGDI, nil
}

func parseFormat(s string) (convert.Format, error) {
	format := convert.Format(strings.ToLower(s))

	switch format {
	case convert.FormatCUE, convert.FormatGDI, convert.FormatCHD:
		return format, nil
	default:
		return "", fmt.Errorf("unknown format %q, want cue, gdi, or chd", s)
	}
}

func codecList() string {
	names := make([]string, 0, len(chd.Codecs()))
	for _, c := range chd.Codecs() {
		names = append(names, string(c))
	}

	return strings.Join(names, " or ")
}
