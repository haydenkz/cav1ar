#!/bin/sh
# Runs cav1ar as PUID:PGID, in the groups that own the GPUs' device nodes, so
# it can use them without root. On the first run, CAV1AR_MOVIES and
# CAV1AR_SHOWS, if set, are watched as the library's movie and show folders.
set -e

if [ "$(id -u)" != 0 ]; then # started as a user already, as compose's user: does
	exec "$@"
fi

mkdir -p /config/cav1ar /config/cache
config=/config/cav1ar/config.json
if [ ! -e "$config" ] && [ -n "$CAV1AR_MOVIES$CAV1AR_SHOWS" ]; then
	watch=""
	[ -n "$CAV1AR_MOVIES" ] && watch="{\"path\": \"$CAV1AR_MOVIES\", \"kind\": \"movies\", \"priority\": 1}"
	[ -n "$CAV1AR_SHOWS" ] && watch="${watch:+$watch, }{\"path\": \"$CAV1AR_SHOWS\", \"kind\": \"shows\"}"
	# Results replace their originals under the same name, so the *arr apps
	# keep track of them.
	printf '{"watch": [%s], "name_template": "{name}", "delete_original": true}\n' "$watch" > "$config"
fi
chown "$PUID:$PGID" /config /config/cav1ar /config/cache
[ -e "$config" ] && chown "$PUID:$PGID" "$config"

groups=$PGID
for dev in /dev/dri/renderD* /dev/dri/card*; do
	[ -e "$dev" ] && groups="$groups,$(stat -c %g "$dev")"
done
exec setpriv --reuid="$PUID" --regid="$PGID" --groups="$groups" --inh-caps=-all "$@"
