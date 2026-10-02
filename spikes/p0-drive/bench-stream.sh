#!/usr/bin/env bash
# P0 §2 streaming benchmark against `p0drive serve` (expects it on 127.0.0.1:8082).
# usage: bench-stream.sh MODE FILE_ID SIZE [BASE_URL]
#   MODE: direct | cache
set -euo pipefail
mode=$1 id=$2 size=$3 base=${4:-http://127.0.0.1:8082}
key=$(cat /data/music-platform/secrets/p0-stream-key)
url="$base/stream/$id?mode=$mode&k=$key"

probe() { # LABEL OFFSET: fetch 64 KiB like a player's first read after a seek
  local label=$1 off=$2 code fb tot out
  out=$(curl -sS -o /dev/null -H "Range: bytes=$off-$(( off + 65535 ))" \
    -w '%{http_code} %{time_starttransfer} %{time_total}' "$url")
  read -r code fb tot <<<"$out"
  printf '  %-24s offset %11d  status %s  first byte %6.3fs  total %6.3fs\n' "$label" "$off" "$code" "$fb" "$tot"
}

# Load file metadata first (a 1-byte direct read never touches the cache): the real service
# keeps metadata in its database, so only the audio fetch should count as playback latency.
curl -sS -o /dev/null -H 'Range: bytes=0-0' "$base/stream/$id?mode=direct&k=$key"
echo "== mode=$mode size=$size via $base"
probe "start" 0
probe "seek 50% right away" $(( size / 2 ))
sleep 4
probe "seek 25% after 4s" $(( size / 4 ))
probe "seek 75% after 4s" $(( size * 3 / 4 ))
probe "seek 90% after 4s" $(( size * 9 / 10 ))
probe "back to start" 0
