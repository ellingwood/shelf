# shelf

Watches download folders and files media into a Jellyfin library with clean,
correctly matched names. It's like FileBot's AMC script, but with Go templates.

```
in/Dune.Part.Two.2024.2160p.WEB-DL.DDP5.1.Atmos.DV.HDR.H.265-GRP.mkv
  → /mnt/media/Movies/Dune - Part Two (2024) [tmdbid-693134]/Dune - Part Two (2024).mkv
in/Breaking Bad Season 1/Breaking.Bad.S01E02.720p.mkv
  → /mnt/media/Shows/Breaking Bad (2008) [tmdbid-1396]/Season 01/Breaking Bad S01E02 - Cat's in the Bag.mkv
```

## Requirements

- Linux, Go ≥ 1.23, systemd (for the daemon)
- A free [TMDB API key](https://www.themoviedb.org/settings/api)
- Optional: `ffprobe` for real stream detection. Jellyfin's bundled `/usr/lib/jellyfin-ffmpeg/ffprobe` is found automatically.
- Optional: a Jellyfin API key for instant library refresh

**Full step-by-step setup (drive mounting, keys, config, systemd, Jellyfin libraries, troubleshooting): [docs/SETUP.md](docs/SETUP.md).**

## Quick start

```sh
make install                 # → ~/.local/bin/shelf
shelf init                   # writes ~/.config/shelf/config.yaml
$EDITOR ~/.config/shelf/config.yaml     # set watch/output paths
cat > ~/.config/shelf/env <<EOF         # secrets, chmod 600
TMDB_API_KEY=...
JELLYFIN_API_KEY=...
EOF
chmod 600 ~/.config/shelf/env      # read automatically; no need to export
shelf check                  # validates config, tests TMDB + Jellyfin
shelf rename -dry-run ~/Downloads/shelf    # see what it would do
make install-service         # run `shelf watch` under systemd --user
make logs
```

## Commands

| Command | What it does |
|---|---|
| `shelf watch` | Daemon: watch rules' folders and import files once they finish arriving |
| `shelf rename [-dry-run] [-output DIR] [-action copy] paths…` | One-shot import of files/folders |
| `shelf preview names…` | Show parsed fields, TMDB match and destination; touches nothing (files needn't exist) |
| `shelf retrofit [-apply] [-duplicates keep-larger] [paths…]` | Re-organize an existing library in place: merge duplicate show/movie folders, add `[tmdbid-N]` tags and episode titles, carry sidecars and artwork along. Dry run unless `-apply` |
| `shelf history [-n 50]` | Recent operations |
| `shelf undo [-last N \| -id ID] [-dry-run]` | Reverse operations (moves go back, copies/links are removed) |
| `shelf check` | Validate config, check dirs/drive, test TMDB key, ffprobe and Jellyfin |
| `shelf init` | Write a starter config |

All commands take `-config PATH` and `-v` (debug logging). `watch`, `rename` and `preview` take `-offline` to skip TMDB.

## How a file flows

1. **Detect.** fsnotify events plus a rescan on startup and every `rescan_interval`. Files matching `ignore` (`*.part`, `*.crdownload`, …), files below `min_size` and the wrong extensions are skipped. A file is handled only once its size and mtime have been unchanged for `stable_for`.
2. **Parse.** The filename is parsed into title, year, SxxEyy (including `S01E01-E02`, `1x02`, `Season 1 Episode 2`), resolution, source, codecs, HDR and edition. Parent folders fill gaps: `Show/Season 1/E02.mkv` and `Heat (1995)/heat.mkv` both work.
3. **Match.** TMDB supplies the canonical title, year, ID and episode title. Year agreement settles same-name remakes. A match below `min_confidence` goes to the review folder (`on_no_match`). Responses are cached for 7 days in `~/.cache/shelf`.
4. **Probe** (if `ffprobe` is found on `PATH`, in Jellyfin's bundle, or at the `ffprobe:` config path): real resolution, codecs and HDR/DV from the streams, overriding what the filename claims.
5. **Render.** The Go template becomes the destination path. Each segment is sanitized (`:` → ` -`, no `?*<>|"`, no trailing dots) and `..` is rejected.
6. **Place.** `move` uses rename on the same filesystem. Across drives it copies to a hidden temp file, fsyncs, checks the size, renames into place and only then deletes the source. Subtitles come along renamed to Jellyfin's `.en.srt`, `.en.forced.srt` and `.en.sdh.srt` (from same-folder sidecars, `Subs/` and `Subs/<episode>/`). Empty source folders are removed.
7. **Record and notify.** Each operation goes to `~/.local/state/shelf/history.jsonl` for `undo`. Jellyfin is told about new files through `/Library/Media/Updated`, debounced so a season pack sends one request.

## Templates

Templates use Go's [`text/template`](https://pkg.go.dev/text/template). Override them globally (`templates:`) or per rule (`rules[].templates:`). Use `/` for folders.

The built-in `jellyfin` preset:

```gotemplate
movie:   Movies/{{.Title | clean}}{{with .Year}} ({{.}}){{end}}{{with .TMDBID}} [tmdbid-{{.}}]{{end}}/{{.Title | clean}}{{with .Year}} ({{.}}){{end}}{{with .Edition}} - {{. | clean}}{{end}}{{.Ext}}
episode: Shows/{{.Show | clean}}{{with .ShowYear}} ({{.}}){{end}}{{with .TMDBID}} [tmdbid-{{.}}]{{end}}/Season {{pad .Season 2}}/{{.Show | clean}} {{epcode .}}{{with .EpisodeTitle}} - {{. | clean}}{{end}}{{.Ext}}
```

`jellyfin-versions` names movies `Title (Year) - 2160p.mkv`, so several versions can share a folder.

**Fields:** `.Title .Year .Edition .Show .ShowYear .Season .Episode .Episodes .EpisodeTitle .Resolution .VideoCodec .AudioCodec .Source .HDR .TMDBID .Ext .Original .Path`. Unknown values are zero, so wrap optional parts in `{{with .X}}…{{end}}`.

**Functions:**

| Function | Example |
|---|---|
| `clean` | filesystem-safe: `{{.Title \| clean}}` |
| `pad N` | `{{pad .Season 2}}` → `01` |
| `epcode` | `{{epcode .}}` → `S01E02` or `S01E01-E02` |
| `sortname` | `The Matrix` → `Matrix, The` |
| `first N` | `{{.Title \| first 1}}` → `T` (A–Z folders) |
| `default D` | `{{default "Unknown" .EpisodeTitle}}` |
| `join SEP a b…` | joins non-empty values |
| `lower` `upper` `title` `trim` `replace OLD NEW` | string helpers |

Try templates safely with `shelf preview "Some.Movie.2020.1080p.mkv"`.

## Retrofitting an existing library

`shelf retrofit` puts a library that was organized by hand, or by older templates, into the current layout. It only renames inside the library drive, so moves are instant.

```sh
shelf retrofit                                   # dry run: print the full plan
shelf retrofit "/mnt/media/Shows/FROM (2022)"    # limit to part of the library
shelf retrofit -apply                            # do it (Jellyfin rescans afterwards)
shelf undo -last N                               # reverse it (N is printed after -apply)
```

- Every video is re-identified. An existing `[tmdbid-N]` folder tag is used directly instead of searching, and copies of the same title and year are given that ID, so split folders such as `Show (2026)` and `Show (2026) [tmdbid-1]` merge.
- Sidecars named after a video move with it and keep their suffix: `.en.srt`, `.nfo`, `-thumb.jpg`, `.trickplay/`. Folder-level files (`poster.jpg`, `tvshow.nfo`, `season.nfo`, trailers) are merged into the new folder when the old one is being emptied. Anything whose name is already taken is left in place and listed.
- Extras (`extras/`, `trailers/`, `*-trailer.mkv`, …) are never renamed as movies or episodes.
- Files that can't be matched confidently are listed and left alone.
- **Duplicates:** when two files want the same name, the default `-duplicates skip` reports them and moves neither. `-duplicates keep-larger` keeps the larger file and moves the others to `<output>/.shelf-review/duplicates/`, preserving their old paths. Nothing is ever deleted.

## Notes

- **Different drives:** keep `action: move` (the default). `hardlink` can't cross filesystems, and `shelf check` warns if you try.
- **Drive not mounted:** set `RequiresMountsFor=` in `deploy/shelf.service` so the daemon never writes into an empty mount-point directory on the root fs. `shelf check` also flags a missing output dir.
- **Jellyfin in Docker:** use `jellyfin.path_map` to translate host paths to container paths.
- **Review folder:** unmatched files go to `<output>/.shelf-review/`. Fix the name and drop the file back into the watch folder.
- **Jellyfin libraries:** point a Movies library at `<output>/Movies` and a Shows library at `<output>/Shows`.

## Project layout

```
cmd/shelf/          CLI entry point and subcommands
internal/parse/     release-name parser (title, year, SxxEyy, quality tags)
internal/match/     TMDB client, scoring, response cache
internal/probe/     ffprobe integration
internal/naming/    Go template engine, presets, path sanitizing
internal/fileops/   move/copy/link with cross-device safety and conflict policies
internal/history/   JSONL operation log and undo
internal/watch/     fsnotify watcher with stability detection
internal/pipeline/  parse → match → probe → render → place → record → notify
internal/jellyfin/  library refresh notifications
deploy/             systemd user unit
docs/SETUP.md       installation and operations guide
```

## Development

```sh
make test     # go vet + unit tests
make build    # → bin/shelf
```
