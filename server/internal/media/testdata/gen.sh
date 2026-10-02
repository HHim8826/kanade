#!/usr/bin/env bash
# Regenerates the media test fixtures (3-second tones with Japanese tags and a cover).
# Requires ffmpeg; defaults to the static build in tools/.
set -euo pipefail
cd "$(dirname "$0")"
FF=${FFMPEG:-/data/music-platform/tools/ffmpeg/bin/ffmpeg}
ff() { "$FF" -hide_banner -loglevel error -y "$@"; }

ff -f lavfi -i color=c=red:s=64x64 -frames:v 1 cover.png
tone=(-f lavfi -i "sine=frequency=440:duration=3")
cover=(-i cover.png -map 0:a -map 1:v -c:v png -disposition:v attached_pic)
tags=(-metadata title=テスト曲 -metadata artist=歌手 -metadata album=アルバム
      -metadata album_artist="Various Artists" -metadata track=3/10 -metadata disc=1/2 -metadata date=2025)

ff "${tone[@]}" "${cover[@]}" "${tags[@]}" -c:a flac -ar 44100 -sample_fmt s16 tone.flac
ff "${tone[@]}" "${tags[@]}" -c:a flac -ar 96000 -sample_fmt s32 -bits_per_raw_sample 24 tone-hires.flac
ff "${tone[@]}" "${cover[@]}" "${tags[@]}" -c:a libmp3lame -b:a 128k -id3v2_version 4 tone-cbr.mp3
ff "${tone[@]}" "${tags[@]}" -c:a libmp3lame -q:a 4 -id3v2_version 3 tone-vbr.mp3
ff "${tone[@]}" -c:a libmp3lame -b:a 128k -id3v2_version 0 -write_xing 0 tone-notag.mp3
ff "${tone[@]}" "${cover[@]}" "${tags[@]}" -c:a aac -b:a 128k tone.m4a
ff "${tone[@]}" "${tags[@]}" -c:a alac tone-alac.m4a
ff "${tone[@]}" "${tags[@]}" -c:a libvorbis -q:a 3 tone.ogg
ff "${tone[@]}" "${tags[@]}" -c:a libopus -b:a 64k tone.opus
ff "${tone[@]}" -c:a pcm_s16le tone.wav
ls -la
