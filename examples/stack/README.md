# The *arr stack with cav1ar

Radarr and Sonarr find releases, SABnzbd and qBittorrent download them, and
cav1ar keeps the library encoded to AV1, replacing each video with its encode
once it checks out.

## Setting it up

1. Make the shared folder, owned by the user the apps run as:

   ```sh
   mkdir -p /srv/data/{usenet,torrents,media/movies,media/tv}
   chown -R 1000:1000 /srv/data
   ```

2. Copy `.env.example` to `.env` and set `PUID`, `PGID`, `TZ` and `DATA`. In
   `compose.yaml`, uncomment cav1ar's GPU: `devices` for Intel or AMD, or
   `deploy` for NVIDIA, which needs the
   [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
   on the host.

3. Run `docker compose up -d`. In Dockge, make a new stack and paste in
   `compose.yaml` and `.env`.

4. Connect the apps. They reach each other by name, such as `sabnzbd`:

   | App | Address | Set up |
   |---|---|---|
   | SABnzbd | `:8080` | Run the wizard. Under Config → Folders, set the temporary folder to `/data/usenet/incomplete` and the completed one to `/data/usenet/complete`, and add the categories `movies` and `tv`. |
   | qBittorrent | `:8090` | Log in as `admin` with the password from `docker logs qbittorrent`, then change it. Under Options → Downloads, set the save path to `/data/torrents`. |
   | Radarr | `:7878` | Add the root folder `/data/media/movies`. Under Download Clients, add SABnzbd (host `sabnzbd`, port 8080, its API key, category `movies`) and qBittorrent (host `qbittorrent`, port 8090). |
   | Sonarr | `:8989` | The same, with `/data/media/tv` and the category `tv`. |
   | cav1ar | `:6767` | Set a password under Settings → Web UI. Encoders should list your GPU, and Folders already watches the library's two folders. |

## What cav1ar does here

Each movie or episode Radarr or Sonarr imports is encoded to AV1. The result
replaces the original under the same name, so Radarr and Sonarr keep track of
it, and Jellyfin or Plex just sees a smaller file.

- **What's already in the library** is left alone, unless you set Folders →
  Videos already there to encode them too.
- **To keep the originals,** set Settings → Output → Original file → keep. The
  results then need names of their own: set Settings → Naming → File name, to
  `{title} ({year}) {episode} [AV1]` for example.
- **qBittorrent has no VPN** in this stack. If you need one, put it behind
  [gluetun](https://github.com/qdm12/gluetun).
- **Port 6767** is also Bazarr's default. If you run Bazarr on the same host,
  change the left side of cav1ar's `ports` entry.
