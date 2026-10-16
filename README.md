# cav1ar

Batch-encode video to AV1 from your terminal, or let it watch a library and
keep every file in it encoded the way you like.

- CPU (SVT-AV1) or GPU (NVIDIA NVENC, AMD/Intel VAAPI) encoding, with optional
  downscaling to a maximum resolution
- Opus or AAC audio, downmixing, and language filters for audio and subtitles,
  including keeping just the best audio track. A track counts as being in your
  language if it's tagged so, or untagged with a title that names it; if no
  track is, the best of each language is kept rather than a guess
- Subtitle tidying: when two tracks in a language look alike, reads them to
  drop duplicates and tracks in the wrong language, and to flag forced and SDH
  tracks
- Only does what a file needs: AV1 video is copied rather than re-encoded, a
  file that only has the wrong name is renamed, and with MKVToolNix installed,
  an MKV whose only problems are its tags, track titles, flags or chapters is
  fixed in place instead of rewritten
- Metadata stripping, custom titles, and release-style file names, taken from
  Movies and TV folders the way media servers lay them out
- Folder watching, with an optional background service

## Install

Needs Linux, Go 1.27+, and ffmpeg built with `libsvtav1` and `libopus`.
MKVToolNix (`mkvpropedit` and `mkvextract`) is optional: with it, fixing a
file's metadata takes a moment instead of a rewrite of the whole file.

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
restart queues the rest again straight away. Every finished job is kept
there too, so the UI can say how much space has been saved over every run.

Videos that arrive in a watched folder go ahead of the library already there,
marked *up next*, so new downloads don't wait behind a long backlog. A folder
can also be given a priority by starting its line with a number, as in
`+1 movies: /media/Movies`: its videos run before those of folders with a
lower one (the default is 0), whether they've just arrived or not. The job
list is in the order jobs run: those that have started, then the rest by
priority, those up next first.

Each job reads the original once, encoding the video and audio to one file
and copying the subtitles and attachments to another on local disk, then
puts them together in a quick remux. If the audio comes out short, which
ffmpeg can do without saying so, it is encoded again on its own.

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
