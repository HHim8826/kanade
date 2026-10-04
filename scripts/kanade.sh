#!/usr/bin/env bash
###############################################################################
#
# Kanade 安裝與管理腳本
#
#   curl -fsSL https://raw.githubusercontent.com/HHim8826/kanade/main/scripts/kanade.sh -o kanade.sh && sudo bash kanade.sh
#
# 不帶參數時顯示選單；也可以直接下指令：
#   install | update | uninstall | status | start | stop | restart | log |
#   password | backup | restore | config | tools | version | help
#
# 需要：Linux（amd64 或 arm64）、root、curl、tar、sha256sum。服務管理用 systemd 或
# OpenRC；兩者都沒有時（例如容器）在背景執行。安裝後可用 kanade-manager 開啟這個選單。
#
# aria2 和 FFmpeg 先用系統的套件管理員安裝；套件庫沒有時（RHEL 系、Amazon Linux 等）改用
# GitHub 上的靜態版，放在安裝位置的 tools/。之後補裝：kanade-manager tools。
#
# 不經詢問安裝（自動化）時可設定：
#   KANADE_YES=1                 全部採用預設或下列設定
#   KANADE_PUBLIC_URL=https://…  公開網址
#   KANADE_LISTEN=127.0.0.1:8080 監聽位址
#   KANADE_ADMIN=admin           管理員帳號
#   KANADE_GH_PROXY=https://…/   GitHub 下載代理（結尾要有 /）
#   KANADE_VERSION=v1.2.3        指定版本（預設最新）
#   KANADE_DIR=/opt/kanade       安裝位置（資料在其中的 data/）
#   KANADE_DEPS=skip             不安裝 aria2 和 FFmpeg
#   KANADE_INIT=systemd|openrc|none  指定服務管理方式
#   KANADE_RELEASE_URL=…         從其他位置下載（目錄內直接放 VERSION、SHA256SUMS 和壓縮檔）
#
###############################################################################

set -o pipefail

REPO="HHim8826/kanade"
MANAGER_PATH="/usr/local/bin/kanade-manager"
STATE_FILE="/etc/kanade/manager.conf"
SERVICE="kanade"
RUN_USER="kanade"

# Static aria2 and FFmpeg for systems whose packages have neither: the builds Kanade is developed
# and tested with. aria2 publishes no Linux build; this one is pinned by checksum. FFmpeg's is the
# latest build of the release branch, checked against the checksums published with it.
ARIA2_STATIC="https://github.com/abcfy2/aria2-static-build/releases/download/1.37.0"
ARIA2_SHA256_amd64="e0a09b12ef67f35f8a8e4fdddbec851d235b7c31da549d0578bff459032b499a"
ARIA2_SHA256_arm64="0c681a89a40e0f82d1f5137608e86257eb0af201459c002941ea098f2b8c26b6"
FFMPEG_STATIC="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest"
FFMPEG_BRANCH="8.1"

RED='\033[1;31m'
GREEN='\033[1;32m'
YELLOW='\033[1;33m'
CYAN='\033[1;36m'
RESET='\033[0m'

info() { printf "${GREEN}%s${RESET}\n" "$*"; }
warn() { printf "${YELLOW}%s${RESET}\n" "$*"; }
error() { printf "${RED}%s${RESET}\n" "$*" >&2; }
die() {
  error "$*"
  exit 1
}
line() { printf '%s\n' "────────────────────────────────────────────────────────────"; }

# Temporary directories of this run, removed when it ends: done, failed or interrupted. The list
# is global, as a local variable is gone by the time the shell exits (review #71); the traps are
# set again in each new one, as a menu command runs in a subshell, which starts without them.
TEMPS=()
cleanup() { [ ${#TEMPS[@]} -eq 0 ] || rm -rf "${TEMPS[@]}"; }
new_temp() { # new_temp VAR: a new temporary directory in VAR
  local d
  d=$(mktemp -d) || die "無法建立暫存目錄。"
  TEMPS+=("$d")
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM HUP
  printf -v "$1" '%s' "$d"
}

# ask PROMPT DEFAULT: reads an answer from the terminal (also when the script came through a pipe);
# with KANADE_YES or no terminal, the default.
ask() {
  local prompt="$1" def="${2:-}" answer=""
  if [ -n "${KANADE_YES:-}" ] || ! { true </dev/tty; } 2>/dev/null; then
    printf '%s\n' "$def"
    return
  fi
  if [ -n "$def" ]; then
    read -r -p "$prompt [$def]: " answer </dev/tty
  else
    read -r -p "$prompt: " answer </dev/tty
  fi
  printf '%s\n' "${answer:-$def}"
}

# confirm PROMPT [y|n]: yes or no, with a default.
confirm() {
  local def="${2:-y}" answer
  answer=$(ask "$1 (y/n)" "$def")
  case "$answer" in
  [Yy] | [Yy][Ee][Ss] | 是) return 0 ;;
  *) return 1 ;;
  esac
}

# ---- the machine ----

check_linux() {
  [ "$(uname -s)" = "Linux" ] || die "這個腳本只支援 Linux。"
}

check_root() {
  [ "$(id -u)" -eq 0 ] || die "請用 root 執行（例如：sudo kanade-manager $*）。"
}

detect_arch() {
  case "$(uname -m)" in
  x86_64 | amd64) ARCH="amd64" ;;
  aarch64 | arm64) ARCH="arm64" ;;
  *) die "目前只提供 amd64 和 arm64 的版本，這台機器是 $(uname -m)。可以從原始碼編譯：https://github.com/$REPO" ;;
  esac
}

detect_init() {
  if [ -n "${KANADE_INIT:-}" ]; then
    INIT_TYPE="$KANADE_INIT"
  elif command -v systemctl >/dev/null 2>&1 && [ "$(cat /proc/1/comm 2>/dev/null)" = "systemd" ]; then
    INIT_TYPE="systemd"
  elif command -v rc-service >/dev/null 2>&1 && command -v openrc-run >/dev/null 2>&1; then
    INIT_TYPE="openrc"
  else
    INIT_TYPE="none"
  fi
  case "$INIT_TYPE" in
  systemd | openrc | none) ;;
  *) die "KANADE_INIT 只能是 systemd、openrc 或 none。" ;;
  esac
}

need_tools() {
  local missing=""
  for t in curl tar sha256sum; do
    command -v "$t" >/dev/null 2>&1 || missing="$missing $t"
  done
  [ -z "$missing" ] || die "缺少：$missing。請先安裝後再執行。"
}

# install_packages PKG...: with the system's package manager; false when there is none or it fails.
install_packages() {
  if command -v apt-get >/dev/null 2>&1; then
    if [ -z "${APT_UPDATED:-}" ]; then
      DEBIAN_FRONTEND=noninteractive apt-get update -qq || return 1
      APT_UPDATED=1
    fi
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@"
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y "$@"
  elif command -v yum >/dev/null 2>&1; then
    yum install -y "$@"
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache "$@"
  elif command -v pacman >/dev/null 2>&1; then
    pacman -Sy --noconfirm "$@"
  elif command -v zypper >/dev/null 2>&1; then
    zypper --non-interactive install "$@"
  else
    return 1
  fi
}

# Where Kanade finds them (among other places): the static builds in tools/ of the install
# directory, or PATH. Empty when there is none.
aria2_path() {
  if [ -x "$INSTALL_DIR/tools/aria2/aria2c" ]; then
    printf '%s\n' "$INSTALL_DIR/tools/aria2/aria2c"
  else
    command -v aria2c 2>/dev/null
  fi
}

ffmpeg_path() {
  local d="$INSTALL_DIR/tools/ffmpeg/bin"
  if [ -x "$d/ffmpeg" ] && [ -x "$d/ffprobe" ]; then
    printf '%s\n' "$d/ffmpeg"
  elif command -v ffmpeg >/dev/null 2>&1 && command -v ffprobe >/dev/null 2>&1; then
    command -v ffmpeg
  fi
}

missing_tools() {
  [ -n "$(aria2_path)" ] || printf ' aria2'
  [ -n "$(ffmpeg_path)" ] || printf ' ffmpeg'
}

# unzip_one ZIP NAME OUT: one file out of a zip archive, with whatever this system has for it.
unzip_one() {
  if command -v unzip >/dev/null 2>&1; then
    unzip -p "$1" "$2" >"$3"
  elif command -v bsdtar >/dev/null 2>&1; then
    bsdtar -xOf "$1" "$2" >"$3"
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c 'import sys, zipfile; sys.stdout.buffer.write(zipfile.ZipFile(sys.argv[1]).read(sys.argv[2]))' "$1" "$2" >"$3"
  else
    return 1
  fi
}

# untar_xz ARCHIVE DIR MEMBER...: those members of a .tar.xz into DIR.
untar_xz() {
  local archive="$1" dir="$2"
  shift 2
  if command -v xz >/dev/null 2>&1; then
    tar -xJf "$archive" -C "$dir" "$@"
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$archive" "$dir" "$@" <<'EOF'
import sys, tarfile
want = set(sys.argv[3:])
safe = {"filter": "data"} if hasattr(tarfile, "data_filter") else {}
with tarfile.open(sys.argv[1]) as t:
    for m in t:  # in one pass: the archive is compressed, going back means decompressing again
        if m.name in want:
            t.extract(m, sys.argv[2], **safe)
            want.discard(m.name)
            if not want:
                break
sys.exit(1 if want else 0)
EOF
  else
    return 1
  fi
}

# static_aria2 WORK: aria2c into tools/aria2.
static_aria2() {
  local work="$1" arch sum zip
  case "$ARCH" in
  amd64) arch="x86_64" sum="$ARIA2_SHA256_amd64" ;;
  arm64) arch="aarch64" sum="$ARIA2_SHA256_arm64" ;;
  esac
  zip="aria2-$arch-linux-musl_static.zip"
  info "下載 aria2 靜態版（約 6 MB）……"
  if ! fetch "$GH_PROXY$ARIA2_STATIC/$zip" "$work/$zip"; then
    warn "下載 aria2 失敗。"
    return 1
  fi
  if ! printf '%s  %s\n' "$sum" "$work/$zip" | sha256sum -c --status; then
    warn "aria2 校驗失敗（SHA-256 不符），沒有安裝。請重試，或換一個代理。"
    return 1
  fi
  if ! unzip_one "$work/$zip" aria2c "$work/aria2c"; then
    warn "無法解開 aria2 的 zip 檔：需要 unzip、bsdtar 或 python3 其中之一。"
    return 1
  fi
  chmod 755 "$work/aria2c"
  if ! "$work/aria2c" --version >/dev/null 2>&1; then
    warn "下載的 aria2 無法在這台機器執行。"
    return 1
  fi
  install -d -m 755 "$INSTALL_DIR/tools/aria2"
  install -m 755 "$work/aria2c" "$INSTALL_DIR/tools/aria2/aria2c"
  info "已安裝 aria2：$INSTALL_DIR/tools/aria2/aria2c"
}

# static_ffmpeg WORK: ffmpeg and ffprobe into tools/ffmpeg/bin.
static_ffmpeg() {
  local work="$1" arch dir pkg free
  case "$ARCH" in
  amd64) arch="linux64" ;;
  arm64) arch="linuxarm64" ;;
  esac
  dir="ffmpeg-n$FFMPEG_BRANCH-latest-$arch-gpl-$FFMPEG_BRANCH"
  pkg="$dir.tar.xz"
  free=$(df -Pk "$INSTALL_DIR" 2>/dev/null | awk 'NR == 2 {print $4}')
  if [ "${free:-0}" -lt $((600 * 1024)) ]; then
    warn "$INSTALL_DIR 所在的磁碟剩不到 600 MB，沒有下載 FFmpeg。"
    return 1
  fi
  info "下載 FFmpeg $FFMPEG_BRANCH 靜態版（約 150 MB）……"
  if ! fetch "$GH_PROXY$FFMPEG_STATIC/$pkg" "$work/$pkg" || ! fetch "$GH_PROXY$FFMPEG_STATIC/checksums.sha256" "$work/ffmpeg.sha256"; then
    warn "下載 FFmpeg 失敗。"
    return 1
  fi
  # The build is replaced daily: a mismatch can also mean it changed between the two downloads.
  if ! (cd "$work" && grep " $pkg\$" ffmpeg.sha256 | sha256sum -c --status); then
    warn "FFmpeg 校驗失敗（SHA-256 不符），沒有安裝。請稍後重試，或換一個代理。"
    return 1
  fi
  info "解壓縮 FFmpeg……"
  if ! untar_xz "$work/$pkg" "$work" "$dir/bin/ffmpeg" "$dir/bin/ffprobe" "$dir/LICENSE.txt"; then
    warn "無法解壓縮 FFmpeg：需要 xz 或 python3。"
    return 1
  fi
  rm -f "$work/$pkg"
  if ! "$work/$dir/bin/ffmpeg" -hide_banner -version >/dev/null 2>&1; then
    warn "下載的 FFmpeg 無法在這台機器執行（需要 glibc 2.28 以上）。"
    return 1
  fi
  install -d -m 755 "$INSTALL_DIR/tools/ffmpeg" "$INSTALL_DIR/tools/ffmpeg/bin"
  # Moved, not copied: each is about 165 MB.
  chown 0:0 "$work/$dir/bin/ffmpeg" "$work/$dir/bin/ffprobe" "$work/$dir/LICENSE.txt"
  chmod 755 "$work/$dir/bin/ffmpeg" "$work/$dir/bin/ffprobe"
  chmod 644 "$work/$dir/LICENSE.txt"
  mv -f "$work/$dir/bin/ffmpeg" "$work/$dir/bin/ffprobe" "$INSTALL_DIR/tools/ffmpeg/bin/"
  mv -f "$work/$dir/LICENSE.txt" "$INSTALL_DIR/tools/ffmpeg/"
  info "已安裝 FFmpeg：$INSTALL_DIR/tools/ffmpeg/bin/ffmpeg"
}

# install_static NAME...: static builds of aria2 and FFmpeg. They are downloaded in the install
# directory, not /tmp, which can be in memory.
install_static() {
  local work="$INSTALL_DIR/tools/.download" t
  install -d -m 755 "$INSTALL_DIR/tools"
  rm -rf "$work"
  mkdir -p "$work"
  for t in "$@"; do
    case "$t" in
    aria2) static_aria2 "$work" ;;
    ffmpeg) static_ffmpeg "$work" ;;
    esac
  done
  rm -rf "$work"
  return 0
}

remove_static() {
  local d="$INSTALL_DIR/tools"
  rm -rf "$d/.download"
  rm -f "$d/aria2/aria2c" "$d/ffmpeg/bin/ffmpeg" "$d/ffmpeg/bin/ffprobe" "$d/ffmpeg/LICENSE.txt"
  rmdir "$d/aria2" "$d/ffmpeg/bin" "$d/ffmpeg" "$d" 2>/dev/null
  return 0
}

# aria2 downloads BitTorrent; FFmpeg converts lossless formats to FLAC and cuts CUE disc images.
# Kanade works without either, without those features. From the system's packages first; static
# builds for what those do not have.
install_deps() {
  [ "${KANADE_DEPS:-}" = "skip" ] && return 0
  local want p
  want=$(missing_tools)
  [ -n "$want" ] || return 0
  echo
  info "Kanade 用 aria2 下載 BitTorrent，用 FFmpeg 轉檔與切割 CUE 整軌；沒有也能執行，只是少了這些功能。"
  if confirm "要用系統的套件管理員安裝$want 嗎？" y; then
    for p in $want; do
      install_packages "$p" || warn "套件管理員裝不了 $p。"
    done
    want=$(missing_tools)
  fi
  if [ -n "$want" ]; then
    echo
    info "可以改用 GitHub 上的靜態版（Kanade 開發時用的同一版），裝在 $INSTALL_DIR/tools，解除安裝時一併移除。"
    case "$want" in
    *ffmpeg*) echo "  FFmpeg 要下載約 150 MB，裝好後佔約 330 MB。" ;;
    esac
    if confirm "要下載$want 的靜態版嗎？" y; then
      [ -n "${PROXY_ASKED:-}" ] || ask_proxy
      # shellcheck disable=SC2086
      install_static $want
    fi
  fi
  [ -n "$(aria2_path)" ] || warn "沒有 aria2c：BitTorrent 下載會停用。之後可以用 kanade-manager tools 安裝。"
  [ -n "$(ffmpeg_path)" ] || warn "沒有 FFmpeg：APE、WAV、整軌 CUE 等檔案會等到裝了 FFmpeg 再處理。之後可以用 kanade-manager tools 安裝。"
  return 0
}

# ---- state ----

load_state() {
  INSTALL_DIR="${KANADE_DIR:-/opt/kanade}"
  GH_PROXY="${KANADE_GH_PROXY:-}"
  if [ -r "$STATE_FILE" ]; then # only root can read it; help and version run without
    # shellcheck disable=SC1090
    . "$STATE_FILE"
    [ -n "${KANADE_DIR:-}" ] && INSTALL_DIR="$KANADE_DIR"
    [ -n "${KANADE_GH_PROXY:-}" ] && GH_PROXY="$KANADE_GH_PROXY"
  fi
  DATA_DIR="$INSTALL_DIR/data"
  BIN="$INSTALL_DIR/kanade"
  [ -n "${SAVED_INIT:-}" ] && [ -z "${KANADE_INIT:-}" ] && KANADE_INIT="$SAVED_INIT"
}

save_state() {
  mkdir -p "$(dirname "$STATE_FILE")"
  {
    printf 'INSTALL_DIR=%q\n' "$INSTALL_DIR"
    printf 'SAVED_INIT=%q\n' "$INIT_TYPE"
    printf 'GH_PROXY=%q\n' "$GH_PROXY"
  } >"$STATE_FILE"
  chmod 600 "$STATE_FILE"
}

installed() { [ -x "$BIN" ]; }

require_installed() {
  installed || die "還沒有安裝 Kanade（找不到 $BIN）。"
}

# as_service CMD...: runs a command as the service user.
as_service() {
  if command -v runuser >/dev/null 2>&1; then
    runuser -u "$RUN_USER" -- "$@"
  else
    su -s /bin/sh "$RUN_USER" -c "$(printf '%q ' "$@")"
  fi
}

kanade() { as_service "$BIN" -data "$DATA_DIR" "$@"; }

installed_version() {
  installed && "$BIN" version 2>/dev/null | awk '{print $2}'
}

# setting KEY: from the data directory's config.json.
setting() {
  kanade config 2>/dev/null | sed -n "s/^ *\"$1\": \"\(.*\)\",\{0,1\}$/\1/p"
}

local_url() {
  local listen host port
  listen=$(setting listen)
  port="${listen##*:}"
  host="${listen%:*}"
  case "$host" in
  "" | 0.0.0.0 | "[::]" | ::) host="127.0.0.1" ;;
  esac
  printf 'http://%s:%s' "$host" "$port"
}

# ---- downloads ----

release_url() { # release_url FILE
  if [ -n "${KANADE_RELEASE_URL:-}" ]; then
    printf '%s/%s' "${KANADE_RELEASE_URL%/}" "$1"
  elif [ -n "${WANT_VERSION:-}" ] && [ "$WANT_VERSION" != "latest" ]; then
    printf '%shttps://github.com/%s/releases/download/%s/%s' "$GH_PROXY" "$REPO" "$WANT_VERSION" "$1"
  else
    printf '%shttps://github.com/%s/releases/latest/download/%s' "$GH_PROXY" "$REPO" "$1"
  fi
}

fetch() { # fetch URL FILE
  local i
  for i in 1 2 3; do
    if curl -fL --connect-timeout 15 --retry 2 -o "$2" "$1" && [ -s "$2" ]; then
      return 0
    fi
    [ "$i" -lt 3 ] && warn "下載失敗，$((i * 3)) 秒後重試……" && sleep $((i * 3))
  done
  return 1
}

ask_proxy() {
  PROXY_ASKED=1
  [ -n "${KANADE_RELEASE_URL:-}" ] && return
  echo
  info "在中國大陸連 GitHub 較慢時，可以用代理（https 開頭、/ 結尾，例如 https://ghproxy.net/）。"
  GH_PROXY=$(ask "GitHub 代理（不用就直接按 Enter）" "$GH_PROXY")
  if [ -n "$GH_PROXY" ]; then
    case "$GH_PROXY" in
    https://*/) ;;
    https://*) GH_PROXY="$GH_PROXY/" ;;
    *) die "代理位址要以 https:// 開頭。" ;;
    esac
  fi
}

latest_version() {
  local tmp
  new_temp tmp
  if fetch "$(release_url VERSION)" "$tmp/VERSION" 2>/dev/null; then
    tr -d ' \r\n' <"$tmp/VERSION"
  fi
}

# download_release DIR: the program for this machine into DIR/kanade, checked against SHA256SUMS.
download_release() {
  local dir="$1" pkg="kanade-linux-$ARCH.tar.gz"
  info "下載 $pkg ……"
  fetch "$(release_url "$pkg")" "$dir/$pkg" || die "下載失敗：$(release_url "$pkg")"
  fetch "$(release_url SHA256SUMS)" "$dir/SHA256SUMS" || die "下載校驗檔失敗。"
  (cd "$dir" && grep " $pkg\$" SHA256SUMS | sha256sum -c --status) || die "檔案校驗失敗（SHA-256 不符），已停止。請重試，或換一個代理。"
  tar -xzf "$dir/$pkg" -C "$dir" || die "解壓縮失敗。"
  [ -x "$dir/kanade" ] || die "壓縮檔裡沒有 kanade。"
  "$dir/kanade" version >/dev/null 2>&1 || die "下載的程式無法在這台機器執行。"
}

# install_manager [FILE]: this script as kanade-manager (the release's copy when given one).
install_manager() {
  local src="${1:-}"
  if [ -z "$src" ] || [ ! -s "$src" ]; then
    src="${BASH_SOURCE[0]}"
  fi
  if [ -f "$src" ] && [ "$(readlink -f "$src")" != "$(readlink -f "$MANAGER_PATH" 2>/dev/null)" ]; then
    install -m 755 "$src" "$MANAGER_PATH"
  elif [ ! -x "$MANAGER_PATH" ]; then
    warn "沒有安裝 kanade-manager：請把這個腳本存成檔案再執行（curl … -o kanade.sh && sudo bash kanade.sh）。"
  fi
}

# ---- the service ----

write_service() {
  case "$INIT_TYPE" in
  systemd)
    cat >"/etc/systemd/system/$SERVICE.service" <<EOF
[Unit]
Description=Kanade music server
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$RUN_USER
Group=$RUN_USER
WorkingDirectory=$INSTALL_DIR
ExecStart=$BIN -data $DATA_DIR serve
Restart=on-failure
RestartSec=5
# Kanade stops its aria2 itself, after aria2 has saved what it was doing.
KillMode=mixed
TimeoutStopSec=60
LimitNOFILE=65535
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ReadWritePaths=$DATA_DIR

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable "$SERVICE" >/dev/null 2>&1
    ;;
  openrc)
    cat >"/etc/init.d/$SERVICE" <<EOF
#!/sbin/openrc-run
name="kanade"
description="Kanade music server"
command="$BIN"
command_args="-data $DATA_DIR serve"
command_user="$RUN_USER:$RUN_USER"
command_background=true
pidfile="/run/$SERVICE.pid"
directory="$INSTALL_DIR"
output_log="$DATA_DIR/logs/stdout.log"
error_log="$DATA_DIR/logs/stdout.log"
retry="TERM/60/KILL/5"

depend() {
	need net
	use dns logger
}
EOF
    chmod 755 "/etc/init.d/$SERVICE"
    rc-update add "$SERVICE" default >/dev/null 2>&1
    ;;
  none)
    if command -v crontab >/dev/null 2>&1 && confirm "這台機器沒有 systemd 或 OpenRC。要加一個 @reboot 排程，開機時自動啟動嗎？" y; then
      (crontab -l 2>/dev/null | grep -v "kanade-manager start"; echo "@reboot $MANAGER_PATH start >/dev/null 2>&1") | crontab -
    fi
    ;;
  esac
}

remove_service() {
  case "$INIT_TYPE" in
  systemd)
    systemctl disable "$SERVICE" >/dev/null 2>&1
    rm -f "/etc/systemd/system/$SERVICE.service"
    systemctl daemon-reload
    ;;
  openrc)
    rc-update del "$SERVICE" default >/dev/null 2>&1
    rm -f "/etc/init.d/$SERVICE"
    ;;
  none)
    if command -v crontab >/dev/null 2>&1; then
      crontab -l 2>/dev/null | grep -v "kanade-manager start" | crontab -
    fi
    ;;
  esac
}

pattern() { printf '^%s -data %s serve' "$BIN" "$DATA_DIR"; }

# serving: the program answering is the file at $BIN now (review #91): every running Kanade process
# runs that very file, not one replaced since, which the process still holds (its /proc exe).
serving() {
  local pid ino none=1
  ino=$(stat -L -c %i "$BIN" 2>/dev/null) || return 1
  for pid in $(pgrep -f "$(pattern)"); do
    [ "$(stat -L -c %i "/proc/$pid/exe" 2>/dev/null)" = "$ino" ] || return 1
    none=0
  done
  return $none
}

running() {
  case "$INIT_TYPE" in
  systemd) systemctl is-active --quiet "$SERVICE" ;;
  openrc) rc-service "$SERVICE" status >/dev/null 2>&1 ;;
  none) pgrep -f "$(pattern)" >/dev/null 2>&1 ;;
  esac
}

service_start() {
  case "$INIT_TYPE" in
  systemd) systemctl start "$SERVICE" ;;
  openrc) rc-service "$SERVICE" start >/dev/null ;;
  none)
    running && return 0
    mkdir -p "$DATA_DIR/logs"
    chown "$RUN_USER:$RUN_USER" "$DATA_DIR/logs"
    # Detached, holding none of this script's files open (a pipe it prints to, a terminal).
    (
      cd "$INSTALL_DIR" || exit 1
      if command -v runuser >/dev/null 2>&1; then
        exec nohup runuser -u "$RUN_USER" -- "$BIN" -data "$DATA_DIR" serve
      fi
      exec nohup su -s /bin/sh "$RUN_USER" -c "exec $BIN -data $DATA_DIR serve"
    ) >>"$DATA_DIR/logs/stdout.log" 2>&1 </dev/null &
    ;;
  esac
}

service_stop() {
  case "$INIT_TYPE" in
  systemd) systemctl stop "$SERVICE" ;;
  openrc) rc-service "$SERVICE" stop >/dev/null ;;
  none)
    pkill -TERM -f "$(pattern)" 2>/dev/null || return 0
    local i
    for i in $(seq 1 60); do
      running || return 0
      sleep 1
    done
    warn "60 秒內沒有停止，強制結束。"
    pkill -KILL -f "$(pattern)" 2>/dev/null
    ;;
  esac
}

# stop_service: service_stop, then makes sure it is no longer running.
stop_service() {
  local i
  service_stop || return 1
  for i in 1 2 3 4 5; do
    running || return 0
    sleep 1
  done
  return 1
}

# wait_up: until the service answers, for up to 30 seconds.
wait_up() {
  local url i
  url="$(local_url)/api/v1/passkeys/available"
  for i in $(seq 1 30); do
    if [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$url")" = "200" ]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# ---- commands ----

do_install() {
  check_root
  if installed; then
    warn "Kanade 已安裝在 $INSTALL_DIR（$(installed_version)）。要更新請選「更新」。"
    return 1
  fi
  detect_arch
  detect_init
  need_tools
  line
  info "安裝 Kanade"
  echo "  程式與資料：$INSTALL_DIR（資料在 $DATA_DIR）"
  echo "  服務管理：$INIT_TYPE；以系統帳號 $RUN_USER 執行"
  line
  INSTALL_DIR=$(ask "安裝位置" "$INSTALL_DIR")
  case "$INSTALL_DIR" in
  /*) ;;
  *) die "安裝位置要用絕對路徑。" ;;
  esac
  DATA_DIR="$INSTALL_DIR/data"
  BIN="$INSTALL_DIR/kanade"

  # How it is reached decides the address it listens on and the address it calls itself.
  local listen public port mode
  echo
  info "Kanade 怎麼被連到？"
  echo "  1) 透過網域：Cloudflare Tunnel、Nginx、Caddy 等反向代理（建議）"
  echo "     只在本機監聽，由代理提供 https。passkey 需要 https 網域。"
  echo "  2) 直接用 IP 和連接埠"
  echo "     連 Google Drive 時要多一步（授權後把瀏覽器的網址貼回）；密碼以未加密的 http 傳送，也不能用 passkey。"
  mode=$(ask "請選擇" "1")
  port=$(ask "連接埠" "8080")
  case "$port" in
  '' | *[!0-9]*) die "連接埠要是數字。" ;;
  esac
  if [ "$mode" = "2" ]; then
    local ip
    ip=$(curl -s4 --max-time 5 https://api.ipify.org 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}')
    listen="${KANADE_LISTEN:-0.0.0.0:$port}"
    public="${KANADE_PUBLIC_URL:-http://${ip:-127.0.0.1}:$port}"
  else
    listen="${KANADE_LISTEN:-127.0.0.1:$port}"
    public="${KANADE_PUBLIC_URL:-}"
    while [ -z "$public" ]; do
      public=$(ask "公開網址（例如 https://music.example.com）" "")
      [ -n "${KANADE_YES:-}" ] && [ -z "$public" ] && die "請用 KANADE_PUBLIC_URL 指定公開網址。"
    done
  fi
  if (exec 3<>"/dev/tcp/127.0.0.1/${listen##*:}") 2>/dev/null; then
    die "連接埠 ${listen##*:} 已被其他程式使用，請換一個。"
  fi
  local admin
  admin=$(ask "管理員帳號" "${KANADE_ADMIN:-admin}")

  ask_proxy
  WANT_VERSION="${KANADE_VERSION:-latest}"

  local tmp
  new_temp tmp
  download_release "$tmp"
  fetch "$(release_url kanade.sh)" "$tmp/kanade.sh" >/dev/null 2>&1 || true

  # The program belongs to root; only the data directory to the service user.
  if ! id "$RUN_USER" >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then
      useradd --system --home-dir "$INSTALL_DIR" --no-create-home --shell /usr/sbin/nologin "$RUN_USER" 2>/dev/null ||
        useradd --system --home-dir "$INSTALL_DIR" --no-create-home --shell /sbin/nologin "$RUN_USER"
    else
      addgroup -S "$RUN_USER" 2>/dev/null
      adduser -S -D -H -h "$INSTALL_DIR" -s /sbin/nologin -G "$RUN_USER" "$RUN_USER"
    fi || die "無法建立系統帳號 $RUN_USER。"
  fi
  { mkdir -p "$INSTALL_DIR" "$DATA_DIR" && chmod 755 "$INSTALL_DIR"; } || die "無法建立 $INSTALL_DIR。"
  install -m 755 "$tmp/kanade" "$BIN" || die "無法寫入 $BIN（磁碟空間不足？）。"
  { chown "$RUN_USER:$RUN_USER" "$DATA_DIR" && chmod 700 "$DATA_DIR"; } || die "無法設定 $DATA_DIR 的權限。"
  install_deps

  kanade config set listen "$listen" >/dev/null || die "設定監聽位址失敗。"
  kanade config set public_url "$public" >/dev/null || die "設定公開網址失敗。"
  # Installed again where the data was kept: its accounts stay.
  local password=""
  if [ -s "$DATA_DIR/db.sqlite" ]; then
    info "找到原有的資料（$DATA_DIR），沿用其中的曲庫與帳號。"
  else
    password=$(head -c 64 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 16)
    printf '%s\n' "$password" | kanade user add "$admin" >/dev/null 2>"$tmp/err" || die "建立管理員失敗：$(cat "$tmp/err")"
  fi

  save_state || die "無法寫入 $STATE_FILE。"
  install_manager "$tmp/kanade.sh"
  write_service || die "無法建立服務。"
  info "啟動服務……"
  local ok=1
  { service_start && wait_up; } || ok=0

  echo
  line
  if [ "$ok" = 1 ]; then
    info "Kanade $(installed_version) 安裝完成，已在執行。"
  else
    warn "Kanade 已安裝，但 30 秒內沒有回應；請用 kanade-manager log 查看記錄。"
  fi
  echo "  網址：$public/app/"
  [ "$mode" = "2" ] || echo "  本機：$(local_url)/app/（請把網域的反向代理指到這裡）"
  if [ -n "$password" ]; then
    echo "  帳號：$admin"
    echo "  密碼：$password"
  else
    echo "  帳號：沿用原有的帳號（忘了密碼可以用 kanade-manager password 重設）"
  fi
  echo "  資料：$DATA_DIR"
  line
  echo "下一步："
  echo "  1. 用上面的帳號登入，到「設定」照著說明在 Google Cloud 建立 OAuth 用戶端，再連線 Google Drive"
  echo "     （音樂存在你自己的 Drive）。"
  if [ "$mode" = "2" ]; then
    echo "     用 IP 時，授權後瀏覽器會停在打不開的 localhost 頁面，把那個網址貼回 Kanade 就完成了。"
    echo "  2. 設好 https 網域（kanade-manager config 修改公開網址）後，可以在「設定 → 帳號」改用 passkey。"
  else
    echo "  2. 登入後可以在「設定 → 帳號」改用 passkey。"
  fi
  [ -n "$password" ] && echo "  這組密碼只顯示這一次；忘了可以用 kanade-manager password 重設。"
  echo "  之後輸入 kanade-manager 開啟管理選單。"
  [ "$mode" = "2" ] && warn "防火牆或雲端安全群組要開放連接埠 $port。"
  return 0
}

do_update() {
  check_root
  require_installed
  detect_arch
  detect_init
  need_tools
  ask_proxy
  WANT_VERSION="${KANADE_VERSION:-latest}"
  local current latest
  current=$(installed_version)
  latest="$WANT_VERSION"
  if [ "$WANT_VERSION" = "latest" ]; then
    latest=$(latest_version)
    [ -n "$latest" ] || die "查不到最新版本，請檢查網路或換一個代理。"
  fi
  info "目前版本：$current；可用版本：$latest"
  if [ "$current" = "$latest" ] && ! confirm "已是這個版本，仍要重新安裝嗎？" n; then
    return 0
  fi
  warn "更新會重新啟動服務：播放中的歌會中斷；下載中的任務會在重新啟動後繼續，做種中的任務會結束。"
  confirm "繼續更新嗎？" y || return 0

  local tmp backup got
  new_temp tmp
  download_release "$tmp"
  got=$("$tmp/kanade" version 2>/dev/null | awk '{print $2}')
  [ "$got" = "$latest" ] || die "下載到的程式是 ${got:-未知的版本}，不是 $latest，沒有更新。剛發佈新版時 GitHub 可能還在提供舊的檔案，請過幾分鐘再試。"
  fetch "$(release_url kanade.sh)" "$tmp/kanade.sh" >/dev/null 2>&1 || true

  # Every step is checked (review #69). The new program goes next to the old one first, on the
  # same file system, so switching to it is a rename.
  rm -f "$BIN.new"
  if ! install -m 755 "$tmp/kanade" "$BIN.new"; then
    rm -f "$BIN.new"
    die "無法寫入 $BIN.new（磁碟空間不足？），沒有更新。"
  fi
  { mkdir -p "$DATA_DIR/backups" && chown "$RUN_USER:$RUN_USER" "$DATA_DIR/backups"; } || {
    rm -f "$BIN.new"
    die "無法建立 $DATA_DIR/backups，沒有更新。"
  }
  backup="$DATA_DIR/backups/before-$latest-$(date +%Y%m%d-%H%M%S).sqlite"
  if ! kanade backup "$backup" >/dev/null; then
    rm -f "$BIN.new"
    die "更新前備份資料庫失敗，沒有更新。"
  fi
  info "已備份資料庫：$backup"

  if ! stop_service; then
    rm -f "$BIN.new"
    service_start
    die "無法停止服務，沒有更新。"
  fi
  # The old program stays as kanade.old (a hard link where the file system has them) until the new
  # one answers as the version asked for.
  rm -f "$BIN.old"
  if ! { ln "$BIN" "$BIN.old" 2>/dev/null || cp -p "$BIN" "$BIN.old"; } || ! mv -f "$BIN.new" "$BIN"; then
    rm -f "$BIN.new"
    service_start
    die "無法替換程式（磁碟空間不足？），沒有更新；服務已用 $current 重新啟動。"
  fi
  if service_start && wait_up && serving && [ "$(installed_version)" = "$latest" ]; then
    rm -f "$BIN.old"
    install_manager "$tmp/kanade.sh"
    info "已更新到 $latest。"
    return 0
  fi
  error "新版本沒有正常啟動，退回 $current。"
  # Rolling back is checked like updating (review #91): the new program must have stopped before
  # the old one goes back, and what answers afterwards must be the old one.
  if ! stop_service; then
    error "無法停止服務，沒有退回：$BIN 仍是 $latest，舊的程式在 $BIN.old。請先執行 kanade-manager stop，再執行 mv -f $BIN.old $BIN 並啟動。資料庫更新前的備份在 $backup。"
    return 1
  fi
  if ! mv -f "$BIN.old" "$BIN"; then
    error "無法放回舊的程式（$BIN.old），請手動處理。資料庫更新前的備份在 $backup。"
    return 1
  fi
  if service_start && wait_up && serving && [ "$(installed_version)" = "$current" ]; then
    warn "已退回 $current 並重新啟動。資料庫更新前的備份在 $backup。"
  else
    error "放回 $current 後，無法確認服務正以它運行，請用 kanade-manager log 查看記錄。資料庫更新前的備份在 $backup。"
  fi
  return 1
}

do_uninstall() {
  check_root
  require_installed
  detect_init
  warn "這會停止並移除 Kanade 的程式與服務。存在 Google Drive 的音樂不受影響。"
  confirm "確定要解除安裝嗎？" n || return 0
  service_stop
  remove_service
  rm -f "$BIN" "$BIN.old" "$MANAGER_PATH"
  remove_static
  local keep=1
  if [ -d "$DATA_DIR" ]; then
    warn "資料目錄 $DATA_DIR 有資料庫（帳號、曲庫、Drive 的授權）、設定和下載暫存。"
    if [ "$(ask "要一併刪除嗎？確定刪除請輸入 DELETE" "")" = "DELETE" ]; then
      rm -rf "$DATA_DIR"
      keep=0
    fi
  fi
  rmdir "$INSTALL_DIR" 2>/dev/null
  rm -f "$STATE_FILE"
  rmdir "$(dirname "$STATE_FILE")" 2>/dev/null
  if [ "$keep" = 0 ] && id "$RUN_USER" >/dev/null 2>&1; then
    userdel "$RUN_USER" 2>/dev/null || deluser "$RUN_USER" 2>/dev/null
  fi
  info "已解除安裝。"
  [ "$keep" = 1 ] && [ -d "$DATA_DIR" ] && echo "資料保留在 $DATA_DIR；重新安裝到同一個位置時會繼續使用。"
  return 0
}

do_status() {
  require_installed
  detect_init
  line
  echo "版本：$(installed_version)"
  echo "位置：$INSTALL_DIR（資料 $DATA_DIR，$(du -sh "$DATA_DIR" 2>/dev/null | awk '{print $1}')）"
  echo "公開網址：$(setting public_url)"
  echo "監聽：$(setting listen)"
  echo "服務管理：$INIT_TYPE"
  if running; then
    if [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$(local_url)/api/v1/passkeys/available")" = "200" ]; then
      printf "狀態：${GREEN}執行中${RESET}\n"
    else
      printf "狀態：${YELLOW}執行中，但沒有回應${RESET}\n"
    fi
  else
    printf "狀態：${RED}已停止${RESET}\n"
  fi
  local a f
  a=$(setting aria2)
  f=$(ffmpeg_path)
  echo "aria2：${a:-沒有（BitTorrent 下載停用；可用 kanade-manager tools 安裝）}"
  echo "FFmpeg：${f:-沒有（轉檔與 CUE 切割停用；可用 kanade-manager tools 安裝）}"
  line
}

do_start() {
  check_root
  require_installed
  detect_init
  if running; then
    info "已在執行。"
    return 0
  fi
  service_start
  wait_up && info "已啟動：$(setting public_url)/app/" || warn "啟動後 30 秒內沒有回應，請查看記錄。"
}

do_stop() {
  check_root
  require_installed
  detect_init
  service_stop
  info "已停止。"
}

do_restart() {
  check_root
  require_installed
  detect_init
  service_stop
  service_start
  wait_up && info "已重新啟動。" || warn "重新啟動後 30 秒內沒有回應，請查看記錄。"
}

do_log() {
  require_installed
  local f="$DATA_DIR/logs/kanade.log"
  [ -f "$f" ] || die "還沒有記錄（$f）。"
  if [ "${1:-}" = "-f" ]; then
    tail -n 50 -f "$f"
  else
    tail -n 100 "$f"
    echo
    echo "（持續查看：kanade-manager log -f）"
  fi
}

do_password() {
  check_root
  require_installed
  local name password
  name=$(ask "要重設哪個帳號的密碼" "${1:-admin}")
  if confirm "隨機產生新密碼嗎？（選 n 自己輸入）" y; then
    password=$(head -c 64 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 16)
  else
    read -r -s -p "新密碼（至少 10 個字元）: " password </dev/tty
    echo
  fi
  [ "${#password}" -ge 10 ] || die "密碼至少要 10 個字元。"
  if printf '%s\n' "$password" | kanade user passwd "$name" >/dev/null; then
    info "已重設 $name 的密碼，所有登入都已登出。"
    echo "新密碼：$password"
  else
    die "重設失敗（帳號 $name 存在嗎？）。"
  fi
}

do_backup() {
  check_root
  require_installed
  local dir="$DATA_DIR/backups" file
  mkdir -p "$dir"
  chown "$RUN_USER:$RUN_USER" "$dir"
  file="$dir/kanade-$(date +%Y%m%d-%H%M%S).sqlite"
  kanade backup "$file" >/dev/null || die "備份失敗。"
  [ -f "$DATA_DIR/config.json" ] && cp -p "$DATA_DIR/config.json" "${file%.sqlite}.config.json"
  info "已備份：$file"
  echo "資料庫有帳號、曲庫與 Google Drive 的授權；音樂本身在 Drive。請把備份檔另存到安全的地方。"
}

do_restore() {
  check_root
  require_installed
  detect_init
  local dir="$DATA_DIR/backups" i=0 choice file
  local -a files=()
  local listing
  listing=$(ls -1t "$dir"/*.sqlite 2>/dev/null) # newest first
  [ -n "$listing" ] || die "$dir 裡沒有備份。"
  while IFS= read -r f; do files+=("$f"); done <<<"$listing"
  for f in "${files[@]}"; do
    i=$((i + 1))
    echo "  $i) $(basename "$f")（$(du -h "$f" | awk '{print $1}')）"
  done
  choice=$(ask "要還原哪一個" "1")
  case "$choice" in
  '' | *[!0-9]*) die "沒有這個選項。" ;;
  esac
  file="${files[$((choice - 1))]:-}"
  [ "$choice" -ge 1 ] && [ -n "$file" ] || die "沒有這個選項。"
  [ "$(head -c 15 "$file")" = "SQLite format 3" ] || die "$(basename "$file") 不是 SQLite 資料庫，沒有還原。"
  warn "還原會以 $(basename "$file") 取代目前的資料庫；之後的變更（新匯入、播放記錄等）都會消失。目前的資料庫會先另存一份。"
  confirm "確定還原嗎？" n || return 0
  local now
  now="$dir/before-restore-$(date +%Y%m%d-%H%M%S).sqlite"
  kanade backup "$now" >/dev/null || die "無法先備份目前的資料庫，沒有還原。"
  # Every step is checked, and the current database is left alone until the chosen one is in
  # place: copied next to it first, on the same file system, then renamed over it (review #69).
  local next="$DATA_DIR/db.sqlite.restore"
  rm -f "$next"
  if ! install -m 600 -o "$RUN_USER" -g "$RUN_USER" "$file" "$next" || ! cmp -s "$file" "$next"; then
    rm -f "$next"
    die "無法複製備份（磁碟空間不足？），沒有還原。"
  fi
  if ! stop_service; then
    rm -f "$next"
    service_start
    die "無法停止服務，沒有還原。"
  fi
  if ! mv -f "$next" "$DATA_DIR/db.sqlite"; then
    rm -f "$next"
    service_start
    die "無法替換資料庫，沒有還原；服務已用原本的資料庫重新啟動。"
  fi
  # The write-ahead log belonged to the database just replaced.
  rm -f "$DATA_DIR/db.sqlite-wal" "$DATA_DIR/db.sqlite-shm"
  if service_start && wait_up; then
    info "已還原並重新啟動。原本的資料庫存為 $now。"
  else
    warn "已還原，但服務沒有回應，請用 kanade-manager log 查看記錄。原本的資料庫存為 $now。"
  fi
}

do_config() {
  check_root
  require_installed
  detect_init
  echo "公開網址：$(setting public_url)"
  echo "監聽：$(setting listen)"
  echo
  echo "  1) 修改公開網址"
  echo "  2) 修改監聽位址"
  echo "  0) 返回"
  local choice value key
  choice=$(ask "請選擇" "0")
  case "$choice" in
  1) key=public_url value=$(ask "新的公開網址（例如 https://music.example.com）" "$(setting public_url)") ;;
  2) key=listen value=$(ask "新的監聽位址（只給本機用 127.0.0.1:8080；對外 0.0.0.0:8080）" "$(setting listen)") ;;
  *) return 0 ;;
  esac
  kanade config set "$key" "$value" >/dev/null || return 1
  info "已修改。"
  [ "$key" = "public_url" ] && echo "已連線 Google Drive 的話：重新導向 URI 可能也變了，重新啟動後到「設定 → 更換用戶端」查看，加到 Google Cloud 的 OAuth 用戶端。"
  confirm "現在重新啟動套用嗎？" y && do_restart
  return 0
}

# do_tools: aria2 and FFmpeg after the installation (declined then, or the packages had none).
do_tools() {
  check_root
  require_installed
  detect_arch
  detect_init
  need_tools
  local before
  before=$(missing_tools)
  if [ -z "$before" ]; then
    info "aria2 和 FFmpeg 都有了：$(aria2_path)、$(ffmpeg_path)"
    return 0
  fi
  install_deps
  [ "$(missing_tools)" = "$before" ] && return 0
  save_state # the proxy, when one was given
  install_manager # run from a newer script: a manager that knows the tools (status, uninstall)
  if running; then
    warn "重新啟動後才會用到：播放中的歌會中斷；下載中的任務會在重新啟動後繼續，做種中的任務會結束。"
    confirm "現在重新啟動嗎？" y && do_restart
  else
    info "下次啟動時會用到。"
  fi
  return 0
}

do_version() {
  if [ -f "$STATE_FILE" ] && [ ! -r "$STATE_FILE" ]; then
    echo "已安裝的版本：要用 sudo 才看得到（sudo kanade-manager version）"
  elif installed; then
    echo "已安裝：$(installed_version)（$INSTALL_DIR）"
  else
    echo "尚未安裝"
  fi
  local latest
  latest=$(latest_version)
  echo "最新版本：${latest:-（查詢失敗）}"
}

usage() {
  cat <<EOF
Kanade 管理腳本

用法：kanade-manager [指令]   （不帶指令時顯示選單）

  install     安裝
  update      更新到最新版本
  uninstall   解除安裝
  status      狀態
  start       啟動
  stop        停止
  restart     重新啟動
  log [-f]    查看記錄
  password    重設密碼
  config      修改網址與監聽位址
  backup      備份資料庫
  restore     還原資料庫
  tools       安裝 aria2 與 FFmpeg
  version     版本
  help        說明

專案：https://github.com/$REPO
EOF
}

menu() {
  while true; do
    echo
    line
    printf "${CYAN} Kanade 管理選單${RESET}"
    if installed; then
      printf "   已安裝 %s，" "$(installed_version)"
      detect_init
      if running; then printf "${GREEN}執行中${RESET}\n"; else printf "${RED}已停止${RESET}\n"; fi
    else
      printf "   尚未安裝\n"
    fi
    line
    cat <<'EOF'
   1) 安裝
   2) 更新
   3) 解除安裝
   4) 狀態
   5) 啟動
   6) 停止
   7) 重新啟動
   8) 查看記錄
   9) 重設密碼
  10) 修改網址與監聽位址
  11) 備份資料庫
  12) 還原資料庫
  13) 版本
  14) 安裝 aria2 與 FFmpeg
   0) 離開
EOF
    line
    local choice
    choice=$(ask "請選擇" "0")
    case "$choice" in
    1) (do_install) && load_state ;;
    2) (do_update) ;;
    3) (do_uninstall) && ! installed && return 0 ;;
    4) (do_status) ;;
    5) (do_start) ;;
    6) (do_stop) ;;
    7) (do_restart) ;;
    8) (do_log) ;;
    9) (do_password) ;;
    10) (do_config) ;;
    11) (do_backup) ;;
    12) (do_restore) ;;
    13) (do_version) ;;
    14) (do_tools) ;;
    0 | q | "") return 0 ;;
    *) warn "沒有這個選項。" ;;
    esac
    [ -n "${KANADE_YES:-}" ] && return 0
  done
}

main() {
  check_linux
  load_state
  case "${1:-}" in
  help | -h | --help | version) ;;
  *) check_root "$@" ;; # the data directory belongs to the service account
  esac
  case "${1:-}" in
  install) do_install ;;
  update | upgrade) do_update ;;
  uninstall | remove) do_uninstall ;;
  status) do_status ;;
  start) do_start ;;
  stop) do_stop ;;
  restart) do_restart ;;
  log | logs) do_log "${2:-}" ;;
  password | passwd) do_password "${2:-}" ;;
  backup) do_backup ;;
  restore) do_restore ;;
  config) do_config ;;
  tools) do_tools ;;
  version) do_version ;;
  help | -h | --help) usage ;;
  "") menu ;;
  *)
    usage
    exit 2
    ;;
  esac
}

main "$@"
