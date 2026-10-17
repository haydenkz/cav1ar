# Changelog

What changes in each release of cav1ar.

## Unreleased

The first release. cav1ar batch-encodes video to AV1, or watches a library
and keeps every file in it encoded the way you like.

### Encoding

- Encodes on every GPU it finds at once (NVIDIA NVENC, AMD and Intel VAAPI),
  or on the CPU with SVT-AV1, spreading jobs over them
- One quality setting, matched across encoders so a file looks about the same
  whichever device encoded it, and a speed for each kind of encoder
- Optional downscaling to a maximum resolution
- Workers: a daemon can lend itself other machines' or containers' GPUs, or
  their CPU when they have none, with folders mapped where the paths differ

### Audio and subtitles

- Opus or AAC audio, downmixing to stereo, and copying tracks already in the
  right codec and layout
- Language filters for audio and subtitles, including keeping just the best
  audio track in your language
- Subtitle tidying: drops duplicate tracks and ones in the wrong language,
  and flags forced and SDH tracks

### Keeping a library encoded

- Folder watching, with movie and show folders named the way media servers
  lay them out, and a priority for each folder
- Only does what a file needs: AV1 video is copied, a file with only the
  wrong name is renamed, and with MKVToolNix, an MKV whose only problems are
  its tags, titles, flags or chapters is fixed in place
- New arrivals go ahead of the backlog
- Results are checked to run their full length before they replace anything
- What it has found, and every finished job, is kept in SQLite, so restarts
  and rescans only look at new and changed files
- Prerolls: a clip played before every result, joined on without
  re-encoding the video, and swapped or removed when the setting changes
- Metadata stripping, custom titles, and release-style file names from a
  template

### Running it

- A terminal UI for the queue, each job's steps and output, and every setting
- A daemon mode, with a systemd user service from `install.sh --service`;
  the terminal UI shows and controls a running daemon's queue
- A web UI served by the daemon, with the queue, history, encoders, folders
  and settings, and an optional password
- A Docker image with Jellyfin's ffmpeg, and an example stack with Radarr,
  Sonarr, SABnzbd and qBittorrent
