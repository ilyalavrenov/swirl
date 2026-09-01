package redump

import (
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Logiqx XML.
type datfile struct {
	Header struct {
		Version string `xml:"version"`
	} `xml:"header"`

	Games []struct {
		Name string `xml:"name,attr"`
		ROMs []struct {
			Name string `xml:"name,attr"`
			SHA1 string `xml:"sha1,attr"`
		} `xml:"rom"`
	} `xml:"game"`
}

// DAT indexes a datfile by track hash.
type DAT struct {
	Version string

	tracks map[string][]string // title -> its track SHA1s, sorted
	bySHA1 map[string][]string // track SHA1 -> titles
}

// Load reads an uncompressed datfile.
func Load(path string) (*DAT, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("dat: %w", err)
	}
	defer f.Close()

	var parsed datfile
	if err := xml.NewDecoder(f).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}

	d := &DAT{
		Version: parsed.Header.Version,
		tracks:  make(map[string][]string, len(parsed.Games)),
		bySHA1:  make(map[string][]string),
	}

	for _, g := range parsed.Games {
		for _, r := range g.ROMs {
			// Cue sheets are regenerated on every conversion, so only track files compare.
			if strings.EqualFold(filepath.Ext(r.Name), ".cue") {
				continue
			}

			sum := strings.ToLower(r.SHA1)
			d.tracks[g.Name] = append(d.tracks[g.Name], sum)
			d.bySHA1[sum] = append(d.bySHA1[sum], g.Name)
		}

		slices.Sort(d.tracks[g.Name])
	}

	if len(d.tracks) == 0 {
		return nil, fmt.Errorf("%s: no games with track hashes, not a redump datfile", filepath.Base(path))
	}

	return d, nil
}

type Result struct {
	// Titles whose track set is exactly the hashes given; one dump can match several.
	Titles []string

	// Whether the datfile holds each supplied hash, in the order given.
	Known []bool
}

func (r Result) KnownCount() int {
	n := 0

	for _, k := range r.Known {
		if k {
			n++
		}
	}

	return n
}

// Match reports the titles whose tracks are exactly sums; a subset matches nothing. A nil
// DAT matches nothing, so a caller with no datfile need not special-case it.
func (d *DAT) Match(sums [][]byte) Result {
	if d == nil {
		return Result{}
	}

	res := Result{Known: make([]bool, len(sums))}
	want := make([]string, len(sums))

	var titles []string

	for i, sum := range sums {
		want[i] = hex.EncodeToString(sum)

		holders := d.bySHA1[want[i]]
		res.Known[i] = len(holders) > 0

		if i == 0 {
			titles = slices.Clone(holders)

			continue
		}

		titles = slices.DeleteFunc(titles, func(t string) bool { return !slices.Contains(holders, t) })
	}

	slices.Sort(want)
	slices.Sort(titles)

	// Multiset, not a set: a repeated track plus a dropped one still counts right.
	for _, title := range slices.Compact(titles) {
		if slices.Equal(d.tracks[title], want) {
			res.Titles = append(res.Titles, title)
		}
	}

	return res
}
