# Setting up shelf

This guide goes from nothing to a running daemon that files new downloads into your Jellyfin library. The example layout matches a typical home server:

| What | Where | Filesystem |
|---|---|---|
| Watch (incoming) folder | `~/Downloads/shelf` | root disk (small) |
| Media library | `/mnt/media` | separate, larger drive |

Substitute your own paths throughout.

---

## 1. Prerequisites

| Requirement | Why | Check |
|---|---|---|
| Linux with systemd | runs the daemon | `systemctl --user status` |
| Go ≥ 1.23 | builds shelf | `go version` |
| A TMDB API key | accurate titles, years, episode names | see step 4 |
| `ffprobe` (optional, recommended) | real resolution/codec/HDR detection | see below |
| Jellyfin (optional) | instant library refresh | `curl -s localhost:8096/System/Info/Public` |

**ffprobe.** If Jellyfin is installed from its official packages, you already have it: `jellyfin-ffmpeg` installs `/usr/lib/jellyfin-ffmpeg/ffprobe`, and shelf finds it automatically even though it isn't on `PATH`. Otherwise run `sudo apt install ffmpeg`. Without ffprobe, shelf relies on whatever the filename claims ("1080p" etc.).

---

## 2. Mount the library drive permanently

Skip this if the drive already mounts at boot.

shelf must never write into an *unmounted* mount point. That would quietly fill your root disk. Mount the drive by UUID in `/etc/fstab`:

```sh
lsblk -f                                  # find the drive's UUID and filesystem
sudo mkdir -p /mnt/media
echo 'UUID=<your-uuid>  /mnt/media  ext4  defaults,nofail  0  2' | sudo tee -a /etc/fstab
sudo systemctl daemon-reload && sudo mount -a
sudo chown "$USER": /mnt/media            # shelf runs as your user
findmnt /mnt/media                        # should print the mount
```

`nofail` lets the machine boot without the drive. Step 8 keeps shelf from starting in that case.

---

## 3. Install shelf

```sh
cd ~/dev/shelf
make test        # optional: run the test suite
make install     # builds and installs to ~/go/bin/shelf
```

Make sure `~/go/bin` is on your `PATH` (`export PATH="$HOME/go/bin:$PATH"` in `~/.zshrc` or `~/.bashrc`), then check with `shelf help`.

---

## 4. Get API keys

**TMDB** (required for matching)
1. Create a free account at https://www.themoviedb.org/signup.
2. Go to Settings → API → request a key (choose "Developer", personal use).
3. Copy either the **API Key** (v3, short hex) or the **API Read Access Token** (v4, starts with `eyJ`). shelf accepts both.

**Jellyfin** (optional, for instant refresh)
1. Jellyfin web UI → Dashboard → API Keys → **+**.
2. Name it `shelf` and copy the key.

---

## 5. Store secrets

Keep keys out of the config file, in an env file only you can read:

```sh
mkdir -p ~/.config/shelf
cat > ~/.config/shelf/env <<'EOF'
TMDB_API_KEY=paste-tmdb-key-here
JELLYFIN_API_KEY=paste-jellyfin-key-here
EOF
chmod 600 ~/.config/shelf/env
```

systemd loads this file for the daemon. For interactive use, load it into your shell:

```sh
set -a; . ~/.config/shelf/env; set +a
```

---

## 6. Write the config

```sh
shelf init          # creates ~/.config/shelf/config.yaml
$EDITOR ~/.config/shelf/config.yaml
```

Change at least:

```yaml
rules:
  - name: incoming
    watch: ~/Downloads/shelf     # where new files land
    output: /mnt/media           # library root on the big drive

jellyfin:
  url: http://localhost:8096
  refresh: true                  # once JELLYFIN_API_KEY is set
```

Leave `action: move` as it is. Since watch and output are on different drives, shelf copies, verifies and then deletes the original, which frees space on the root disk. (`hardlink` cannot cross drives, and `shelf check` warns if you try it.)

Useful options:

| Option | Default | Meaning |
|---|---|---|
| `preset` | `jellyfin` | `jellyfin-versions` adds ` - 1080p` / ` - 2160p` so several versions of a movie can coexist |
| `conflict` | `skip` | when the destination exists: `skip`, `overwrite`, `suffix`, `keep-larger` (handy for upgrades) |
| `stable_for` | `10s` | how long a file must stop growing before import |
| `min_size` | `50MB` | ignore smaller files (samples, extras) |
| `tmdb.min_confidence` | `0.75` | lower = more automatic matches, higher = more sent to review |
| `tmdb.on_no_match` | `review` | `review` (move to `.shelf-review`), `parsed` (use the filename's info), `skip` (leave it) |
| `ffprobe` | auto | explicit ffprobe path if auto-detection misses it |
| `rules[].type` | `auto` | force `movie` or `tv` for a dedicated folder |

See the README for the full template reference.

---

## 7. Verify before going live

```sh
shelf check
```

Expected output (✓ = good, ! = optional, ✗ = must fix):

```
config: /home/you/.config/shelf/config.yaml
  ✓ valid (1 rule(s), preset "jellyfin")
rule "incoming":
  ✓ watch dir /home/you/Downloads/shelf
  ✓ output dir /mnt/media (writable)
  ✓ action move, conflict skip
services:
  ✓ TMDB: reachable, key valid (The Matrix → tmdb 603)
  ✓ ffprobe: /usr/lib/jellyfin-ffmpeg/ffprobe (real resolution/codec detection on)
  ✓ Jellyfin: reachable at http://localhost:8096
```

Then dry-run on real files. Nothing is touched:

```sh
shelf preview "Some.Movie.2019.1080p.BluRay.x264.mkv"   # any name, file needn't exist
shelf rename -dry-run ~/Downloads/shelf                  # what the daemon would do now
```

If the dry run looks right, organize the existing backlog once:

```sh
shelf rename ~/Downloads/shelf
shelf history          # what happened
shelf undo -last 5     # if you change your mind
```

---

## 8. Run it as a service

First tell the unit about your library mount, so the service won't start while the drive is missing. Edit `deploy/shelf.service`:

```ini
RequiresMountsFor=/mnt/media
```

Then:

```sh
make install-service            # installs + enables the systemd --user unit
loginctl enable-linger "$USER"  # keep it running when you're logged out / after reboot
make logs                       # follow the log (journalctl --user -u shelf -f)
```

Test it by copying a video into the watch folder. Within about `stable_for` seconds you should see:

```
level=INFO msg=imported src=.../Some.Movie.2019.1080p.mkv dst="/mnt/media/Movies/Some Movie (2019) [tmdbid-12345]/Some Movie (2019).mkv" action=move
level=INFO msg="jellyfin notified" files=1 mode=targeted
```

Managing the service:

```sh
systemctl --user status shelf
systemctl --user restart shelf   # after editing config.yaml
systemctl --user stop shelf
```

After code changes, run `make install && systemctl --user restart shelf`.

---

## 9. Point Jellyfin at the library

Dashboard → Libraries → Add Media Library:

| Library | Content type | Folder |
|---|---|---|
| Movies | Movies | `/mnt/media/Movies` |
| Shows | Shows | `/mnt/media/Shows` |

Leave TheMovieDB enabled as a metadata downloader. shelf puts `[tmdbid-N]` in folder names, so Jellyfin matches exactly.

**Jellyfin in Docker:** if the container sees the drive at a different path (e.g. `-v /mnt/media:/media`), add a path map so refresh notifications use container paths:

```yaml
jellyfin:
  path_map:
    - { from: /mnt/media, to: /media }
```

---

## 10. Day-to-day

- **Drop files into the watch folder.** Browser downloads (`.crdownload`), `.part` files and anything still growing are ignored until they finish.
- **Check the review folder now and then** (`/mnt/media/.shelf-review`). Files land there when TMDB isn't confident. Rename one more clearly (e.g. `Title (Year).mkv` or `Show S01E02.mkv`) and move it back into the watch folder.
- **Subtitles** next to a video, or in `Subs/`, are moved along and renamed `.en.srt`, `.en.forced.srt`, `.en.sdh.srt`.
- **Upgrading quality:** set `conflict: keep-larger`, or use `preset: jellyfin-versions` to keep both versions.

---

## Troubleshooting

| Symptom | Fix |
|---|---|
| Nothing happens when a file arrives | `make logs`. Is the extension listed? Is the file above `min_size` and not matching `ignore`? Run with `-v` to see `pending` events. |
| `output dir … does not exist — is the drive mounted?` | `findmnt /mnt/media`; `sudo mount -a`. |
| Service won't start after boot | Usually the drive isn't mounted yet (by design with `RequiresMountsFor`). `systemctl --user status shelf`. |
| Everything goes to `.shelf-review` | `shelf check` (is the TMDB key valid?), then `shelf preview <file>` shows the best candidate and score. Lower `tmdb.min_confidence` slightly if matches are right but scored low. |
| `tmdb: unauthorized` | Wrong key, or the env file isn't loaded. The systemd unit reads `~/.config/shelf/env`. |
| `Jellyfin: HTTP 401` | Regenerate the API key. Check `jellyfin.url`. |
| Wrong match | `shelf undo -id <id>` (IDs from `shelf history`), rename the file with the year, drop it back in. |
| ffprobe not detected | `ls /usr/lib/jellyfin-ffmpeg/ffprobe`; set `ffprobe:` explicitly in config. |
| `hardlink across filesystems is impossible` | Use `action: move` when watch and output are on different drives. |

## Files shelf uses

| Path | Contents |
|---|---|
| `~/.config/shelf/config.yaml` | configuration |
| `~/.config/shelf/env` | secrets (chmod 600) |
| `~/.local/state/shelf/history.jsonl` | operation log used by `history` / `undo` |
| `~/.cache/shelf/tmdb-cache.json` | TMDB response cache (7 days; safe to delete) |
| `~/.config/systemd/user/shelf.service` | installed service unit |
