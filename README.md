# cav1ar

Batch-encode video to AV1 from your terminal, or let it watch a library and
encode new files as they arrive.

- CPU (SVT-AV1) or GPU (NVIDIA NVENC, AMD/Intel VAAPI) encoding
- Opus audio, downmixing, and language filters for audio and subtitles
- Metadata stripping, custom titles, and release-style file names
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
cav1ar -h               # list every flag
```

In the UI, press `a` to add files, `s` for settings, `p` to pause, and `?` for
all keys. In the file browser, `w` watches a folder for new videos.

Settings are saved to `~/.config/cav1ar/config.json`. A running daemon picks
up changes made in the UI.

## File names

Encodes are named from a template, set in Settings → Output:

```
{title} ({year}) {episode} [{resolution}] [{source}] [AV1] [{audio}] - {group}
```

which gives names like `Movie Title (2024) [1080p] [BRRip] [AV1] [Opus5.1] - Group.mkv`.
Placeholders with nothing to fill them are dropped, along with their brackets.

## Background service

`./install.sh --service` sets up a systemd user service that runs
`cav1ar -daemon`.

```sh
journalctl --user -u cav1ar -f    # follow the log
loginctl enable-linger $USER      # keep it running after you log out
```

## License

[MIT](LICENSE)
