# swirl

Converts Dreamcast disc images between CUE/BIN, GDI, and CHD, including
multi-track discs, pregaps, and the GD-ROM high-density area. The CHDs it writes
pass `chdman verify`. Single static Go binary.

## Install

```
go install github.com/ilyalavrenov/swirl@latest
```

Prebuilt binaries are on the
[releases page](https://github.com/ilyalavrenov/swirl/releases).

## Use

```
swirl convert "./My Game (Japan)/disc.cue" ./out     # CUE/BIN to GDI
swirl convert "./My Game (Japan)" ./out/disc.chd     # input dir, CHD out
swirl convert --to cue ./out/disc.chd ./restored     # format set explicitly
swirl convert --force ./out/disc.gdi ./out/disc.chd  # replace existing output
swirl convert --codec cdzl ./d.gdi ./d.chd           # skip FLAC, write faster
swirl info ./out/disc.chd                            # name, track table, layout
swirl verify ./out/disc.chd                          # hunk CRCs and header SHA1s
swirl verify --dat ./redump.dat ./out/disc.chd       # also: a known-good rip?
swirl verify --redump ./out/disc.chd                 # same, fetching redump's datfile
swirl --json info ./out/disc.chd                     # same, machine-readable
```

`<input>` is a `.cue`, `.gdi`, or `.chd`, or a directory holding exactly one;
`<output>` is a file for chd and a directory for cue and gdi. The format follows
the output extension unless `--to` overrides it. Supported: cue to gdi or chd,
gdi to cue or chd, chd to cue or gdi.

CHD output defaults to `cdzl,cdfl`: audio hunks take the smaller of deflate and
FLAC, which is roughly a fifth off a disc with a redbook soundtrack. `--codec
cdzl` deflates everything instead.

`verify` says a CHD is undamaged; matching its tracks against a redump datfile says it is
a known-good rip. Pass one with `--dat`, or let `--redump` fetch and cache
<http://redump.org/datfile/dc/>. A GDI cannot match: it drops the pregaps redump records.

Output is staged and renamed into place, so a failed run leaves nothing
half-written. GDI output gets a `name.txt` holding the IP.BIN game name.

## License

MIT, see [LICENSE](LICENSE).
