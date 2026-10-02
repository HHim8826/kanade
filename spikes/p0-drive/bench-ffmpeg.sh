#!/usr/bin/env bash
# P0 §2: let ffmpeg act as the player. It seeks with -ss, which on a FLAC without a SEEKTABLE
# means a binary search over byte offsets, like Media3's FlacBinarySearchSeeker.
# usage: bench-ffmpeg.sh MODE FILE_ID SEEK_SECONDS [LABEL]
set -euo pipefail
mode=$1 id=$2 seek=$3 label=${4:-}
dir=$(cd "$(dirname "$0")" && pwd)
key=$(cat /data/music-platform/secrets/p0-stream-key)
ffmpeg=/data/music-platform/tools/ffmpeg/bin/ffmpeg
url="http://127.0.0.1:8082/stream/$id?mode=$mode&k=$key"

before=$(grep -c ' bytes=' "$dir/serve.log" || true)
t0=$(date +%s.%N)
"$ffmpeg" -hide_banner -nostdin -v error -ss "$seek" -i "$url" -t 3 -f null -
t1=$(date +%s.%N)
sleep 0.3 # let the server log the requests ffmpeg just closed
reqs=$(( $(grep -c ' bytes=' "$dir/serve.log") - before ))
printf '%-28s mode=%-6s seek=%5ss  decode 3s of audio took %6.3fs  HTTP requests=%d\n' \
  "$label" "$mode" "$seek" "$(python3 -c "print($t1 - $t0)")" "$reqs"
