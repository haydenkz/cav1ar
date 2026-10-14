# cav1ar

Batch-encode video to AV1 from your terminal, or let it watch a library and
keep every file in it encoded the way you like.

- CPU (SVT-AV1) or GPU (NVIDIA NVENC, AMD/Intel VAAPI) encoding, with optional
  downscaling to a maximum resolution
- Opus or AAC audio, downmixing, and language filters for audio and subtitles,
  including keeping just the best audio track
- Subtitle tidying: drops tracks in the wrong language and duplicates, and
  flags forced and SDH tracks
- Only does what a file needs: AV1 video is copied rather than re-encoded, and
  a file that only has the wrong name is renamed
- Metadata stripping, custom titles, and release-style file names, taken from
  Movies and TV folders the way media servers lay them out
- Folder watching, with an optional background service

## Install

Needs Linux, Go 1.27+, and ffmpeg built with `libsvtav1` and `libopus`.

```sh
git clone https://github.com/haydenkz/cav1ar
cd cav1ar
./install.sh            # add --service to also run it in the background
```

## Usage

```sh
cav1ar                  # open the UI
cav1ar ~/Videos         # open it with a folder's videos queued
cav1ar -daemon          # run without the UI, using your saved settings
cav1ar -dry-run         # say what would happen to the watched folders, and touch nothing
cav1ar -h               # list every flag
```

When a cav1ar daemon is running, `cav1ar` opens the UI on the daemon's queue:
it shows what the daemon is doing, and adding, pausing, canceling and
retrying act on the daemon's jobs, which carry on once the UI closes. Pass
`-local` for a queue of the UI's own instead.

In the UI, press `a` to add files, `s` for settings, `p` to pause once the
running jobs finish (`P` pauses them where they are), `h` to hide finished
jobs, and `?` for all keys. In the file browser, `w` watches a folder; press it again to watch
it as movies or shows.

Settings are saved to `~/.config/cav1ar/config.json`. A running daemon picks
up changes made in the UI.

## File names

Encodes are named from a template, set in Settings → Output:

```
{title} ({year}) {episode} [{resolution}] [{source}] [AV1] [{audio}] - {group}
```

which gives names like `Movie Title (2024) [1080p] [BRRip] [AV1] [Opus5.1] - Group.mkv`.
Placeholders with nothing to fill them are dropped, along with their brackets.

## Keeping a library encoded

A watched folder can be marked as movies or shows, as in a Jellyfin or Plex
library. Then titles come from the folders rather than from each download's
name:

```
Movies/Title (Year)/anything.mkv     → Title (Year) [1080p] … .mkv
TV/Show/[Season N/]show.s01e02.mkv   → Show S01E02 [1080p] … .mkv
```

Set **Videos already in a folder when it is first watched** to *handle them
too*, and cav1ar checks every video in the library against your settings and
queues only the ones that don't match. What it has found is remembered in
`~/.config/cav1ar/cav1ar.db`, including which files still need work, so later
scans only look at new and changed files, until the settings change, and a
restart queues the rest again straight away. Every finished job is kept there
too, so the UI can say how much space has been saved over every run.

Results are written to a hidden file next to the original, and only take its
place once every track is checked to run its full length. With *delete
originals* on, a file whose name doesn't change is replaced in place.
`-retry-failed` gives files that failed another go.

## Prerolls

Settings → Output → **Preroll** (or `-preroll`) takes a clip to play before
every result, such as an intro. There's none unless you set one. The clip is
encoded once for each kind of video it meets, matching it down to the AV1
settings, then joined on without re-encoding the video, so players see one
continuous stream. Matching is best effort: when the encoder can't reproduce
the video's AV1 sequence header, the log says so, since some players may
stutter where the two meet. The file it's joined onto is kept in
`~/.cache/cav1ar/work`, so the library's disks only read the source and write
the result.

Each result records which clip it starts with and how long it runs, so a
preroll is never added twice. Changing the clip swaps it in every file that
has one, and clearing it takes it back out, rewriting each of those files.

## Background service

`./install.sh --service` sets up a systemd user service that runs
`cav1ar -daemon`. Run `cav1ar` to see what it's doing.

```sh
journalctl --user -u cav1ar -f    # follow the log
loginctl enable-linger $USER      # keep it running after you log out
```

## License

[MIT](LICENSE)
