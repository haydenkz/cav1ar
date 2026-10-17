# cav1ar's image: the daemon, with the web UI on 6767, and an ffmpeg that has
# every encoder it uses. See examples/stack for running it with the *arr apps.

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /cav1ar ./cmd/cav1ar

FROM debian:bookworm-slim
# Jellyfin's ffmpeg has NVENC, VAAPI with the Intel and AMD drivers, SVT-AV1
# and Opus in one package. MKVToolNix fixes tags without rewriting files, and
# pci.ids names GPUs.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg mkvtoolnix pci.ids \
 && install -d /etc/apt/keyrings \
 && curl -fsSL https://repo.jellyfin.org/jellyfin_team.gpg.key | gpg --dearmor -o /etc/apt/keyrings/jellyfin.gpg \
 && echo "deb [signed-by=/etc/apt/keyrings/jellyfin.gpg] https://repo.jellyfin.org/debian bookworm main" > /etc/apt/sources.list.d/jellyfin.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends jellyfin-ffmpeg8 \
 && ln -s /usr/lib/jellyfin-ffmpeg/ffmpeg /usr/local/bin/ffmpeg \
 && ln -s /usr/lib/jellyfin-ffmpeg/ffprobe /usr/local/bin/ffprobe \
 && apt-get purge -y curl gnupg && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*
COPY --from=build /cav1ar /usr/local/bin/cav1ar
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
ENV XDG_CONFIG_HOME=/config XDG_CACHE_HOME=/config/cache XDG_RUNTIME_DIR=/tmp \
    PUID=1000 PGID=1000 NVIDIA_DRIVER_CAPABILITIES=compute,video,utility
EXPOSE 6767
VOLUME /config
ENTRYPOINT ["entrypoint.sh"]
CMD ["cav1ar", "-daemon"]
