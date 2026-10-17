#!/bin/sh
# Builds and installs cav1ar, and optionally a systemd user service that runs
# it as a daemon. Run it from a clone of the repository:
#
#   ./install.sh [--service] [--uninstall] [--bin-dir DIR]
#
# Options:
#   --service     also install and start a systemd user service running `cav1ar -daemon`
#   --uninstall   remove cav1ar and its service (settings are kept)
#   --bin-dir DIR where to put the binary (default: ~/.local/bin)
set -eu

REPO="haydenkz/cav1ar"
BIN_DIR="${CAV1AR_BIN_DIR:-$HOME/.local/bin}"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
SERVICE=0
UNINSTALL=0

while [ $# -gt 0 ]; do
	case "$1" in
	--service) SERVICE=1 ;;
	--uninstall) UNINSTALL=1 ;;
	--bin-dir)
		if [ $# -lt 2 ] || [ -z "$2" ]; then
			echo "--bin-dir needs a folder, as in: --bin-dir ~/bin" >&2
			exit 2
		fi
		BIN_DIR="$2"
		shift
		;;
	-h | --help)
		echo "usage: install.sh [--service] [--uninstall] [--bin-dir DIR]"
		exit 0
		;;
	*)
		echo "unknown option: $1" >&2
		exit 2
		;;
	esac
	shift
done

say() { printf '\033[1;35m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die() {
	printf '\033[1;31merror:\033[0m %s\n' "$*" >&2
	exit 1
}
has() { command -v "$1" >/dev/null 2>&1; }

if [ "$UNINSTALL" = 1 ]; then
	if has systemctl && [ -f "$UNIT_DIR/cav1ar.service" ]; then
		systemctl --user disable --now cav1ar.service || true
		rm -f "$UNIT_DIR/cav1ar.service"
		systemctl --user daemon-reload || true
		say "removed the cav1ar service"
	fi
	rm -f "$BIN_DIR/cav1ar"
	say "removed $BIN_DIR/cav1ar"
	say "your settings are still in ${XDG_CONFIG_HOME:-$HOME/.config}/cav1ar"
	exit 0
fi

[ "$(uname -s)" = Linux ] || die "cav1ar only supports Linux"

# cav1ar is built from the clone this script is in.
script_dir=$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)
if [ ! -f "$script_dir/go.mod" ] || ! grep -q "module github.com/$REPO" "$script_dir/go.mod"; then
	die "run install.sh from a clone of the repository: git clone https://github.com/$REPO"
fi

# Find a Go new enough to build cav1ar. With mise, `go` may be a shim that
# fails until a version is set globally, so ask mise for one instead.
GO_NEED=$(sed -n 's/^go \([0-9.]*\).*/\1/p' "$script_dir/go.mod")
GO_MINOR=$(echo "$GO_NEED" | cut -d. -f1,2)
# go_ok reports whether the go command given is new enough. Run from the
# clone, go switches to the toolchain go.mod asks for where it is allowed to.
go_ok() {
	have=$(cd "$script_dir" && "$@" env GOVERSION 2>/dev/null) || return 1
	awk -v have="${have#go}" -v need="$GO_NEED" 'BEGIN {
		split(have, h, "."); split(need, n, ".")
		for (i = 1; i <= 3; i++) { if (h[i] + 0 != n[i] + 0) exit !(h[i] + 0 > n[i] + 0) }
	}'
}
if go_ok go; then
	GO=go
elif has mise && go_ok mise exec "go@$GO_MINOR" -- go; then
	GO="mise exec go@$GO_MINOR -- go"
	say "using Go $GO_MINOR from mise"
else
	die "cav1ar is built with Go $GO_NEED or newer: https://go.dev/dl/"
fi

# ffmpeg does the encoding, so check it has what cav1ar needs.
ffmpeg_hint() {
	if has pacman; then
		echo "sudo pacman -S ffmpeg"
	elif has apt-get; then
		echo "sudo apt install ffmpeg"
	elif has dnf; then
		echo "sudo dnf install ffmpeg (from RPM Fusion)"
	else
		echo "your package manager's ffmpeg package"
	fi
}
if ! has ffmpeg || ! has ffprobe; then
	warn "ffmpeg and ffprobe are needed; install them with: $(ffmpeg_hint)"
else
	encoders=$(ffmpeg -hide_banner -encoders 2>/dev/null)
	echo "$encoders" | grep -q libsvtav1 || warn "this ffmpeg has no SVT-AV1 (libsvtav1); CPU encoding won't work"
	echo "$encoders" | grep -q libopus || warn "this ffmpeg has no Opus (libopus); only copying audio will work"
fi
if ! has mkvpropedit || ! has mkvextract; then
	say "optional: install MKVToolNix (mkvpropedit, mkvextract) to fix tags without rewriting files"
fi

mkdir -p "$BIN_DIR"
say "building from $script_dir"
version=$(git -C "$script_dir" describe --tags --always --dirty 2>/dev/null || echo dev)
(cd "$script_dir" && $GO build -trimpath -ldflags "-s -w -X main.version=$version" -o "$BIN_DIR/cav1ar" ./cmd/cav1ar)
say "installed $BIN_DIR/cav1ar"

case ":$PATH:" in
*":$BIN_DIR:"*) ;;
*) warn "$BIN_DIR isn't on your PATH; add it in your shell's startup file" ;;
esac

if [ "$SERVICE" = 1 ]; then
	has systemctl || die "--service needs systemd"
	mkdir -p "$UNIT_DIR"
	cat >"$UNIT_DIR/cav1ar.service" <<EOF
[Unit]
Description=cav1ar AV1 encoder, watching folders for new videos

[Service]
ExecStart=$BIN_DIR/cav1ar -daemon
Restart=on-failure
# Keep encoding from slowing down the rest of the system.
Nice=10
IOSchedulingClass=idle

[Install]
WantedBy=default.target
EOF
	systemctl --user daemon-reload
	systemctl --user enable --now cav1ar.service
	say "the cav1ar service is running; follow it with: journalctl --user -u cav1ar -f"
	say "to keep it running while you're logged out: loginctl enable-linger $USER"
fi

say "run 'cav1ar' to configure it"
