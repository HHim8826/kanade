#!/usr/bin/env python3
"""Upload a folder to Kanade and import it, like the phone app will (decision D4).

usage: upload-folder.py FOLDER [--server URL] [--token TOKEN | --token-file PATH] [--stop-after-chunks N]

Files keep their paths relative to FOLDER, so disc folders and cover images still group
correctly. Chunks are 32 MB; an interrupted run resumes when started again with the same folder.
"""
import argparse
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request

AUDIO = {".flac", ".mp3", ".m4a", ".mp4", ".aac", ".ogg", ".oga", ".opus", ".wav", ".aif", ".aiff",
         ".ape", ".tak", ".wv", ".tta", ".dsf", ".dff", ".wma"}
IMAGES = {".jpg", ".jpeg", ".png"}


def call(server, token, method, path, body=None, raw=None):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    req = urllib.request.Request(server + path, data=data, method=method)
    req.add_header("Authorization", "Bearer " + token)
    # Cloudflare's bot protection on the domain blocks Python's default User-Agent.
    req.add_header("User-Agent", "kanade-uploader/1.0")
    if body is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            return r.status, parse(r.read())
    except urllib.error.HTTPError as e:
        return e.code, parse(e.read())


def parse(raw):
    try:
        return json.loads(raw or b"null")
    except ValueError:  # an HTML error page from a proxy, for example
        return {"error": raw[:200].decode("utf-8", "replace")}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("folder")
    ap.add_argument("--server", default=os.environ.get("KANADE_SERVER", "http://localhost:8080"))
    ap.add_argument("--token")
    ap.add_argument("--token-file")
    ap.add_argument("--stop-after-chunks", type=int, default=0, help="simulate a dropped connection")
    a = ap.parse_args()
    token = a.token or open(a.token_file).read().strip()
    root = os.path.abspath(a.folder)
    # One group per folder: rerunning on the same folder resumes the same uploads.
    group = "cli-" + hashlib.sha256(root.encode()).hexdigest()[:24]

    files = []
    for dirpath, _, names in os.walk(root):
        for n in sorted(names):
            if os.path.splitext(n)[1].lower() in AUDIO | IMAGES:
                files.append(os.path.join(dirpath, n))
    chunks_sent = 0
    t0, sent_bytes = time.time(), 0
    for path in files:
        rel = os.path.relpath(path, root).replace(os.sep, "/")
        size = os.path.getsize(path)
        h = hashlib.sha256()
        with open(path, "rb") as f:
            for block in iter(lambda: f.read(1 << 20), b""):
                h.update(block)
        status, r = call(a.server, token, "POST", "/api/v1/uploads",
                         {"group": group, "path": rel, "size": size, "sha256": h.hexdigest()})
        if status != 201:
            sys.exit(f"{rel}: {status} {r}")
        up, chunk = r["upload"], r["chunk_size"]
        if up["state"] != "receiving":
            print(f"  {rel}: already uploaded")
            continue
        offset = up["received"]
        if offset:
            print(f"  {rel}: resuming at {offset} of {size}")
        with open(path, "rb") as f:
            while offset < size:
                f.seek(offset)
                data = f.read(chunk)
                status, r = call(a.server, token, "PUT", f"/api/v1/uploads/{up['id']}?offset={offset}", raw=data)
                if status == 409 and "received" in r:  # server has a different count: continue from it
                    offset = r["received"]
                    continue
                if status != 200:
                    sys.exit(f"{rel}: chunk at {offset}: {status} {r}")
                offset = r["received"]
                sent_bytes += len(data)
                chunks_sent += 1
                if a.stop_after_chunks and chunks_sent >= a.stop_after_chunks:
                    print(f"  stopping after {chunks_sent} chunks (simulated disconnect) in {rel} at {offset}")
                    return
        status, r = call(a.server, token, "POST", f"/api/v1/uploads/{up['id']}/complete")
        if status != 200:
            sys.exit(f"{rel}: complete: {status} {r}")
        print(f"  {rel}: done ({size} bytes)")
    secs = time.time() - t0
    print(f"uploaded {sent_bytes / 2**20:.1f} MiB in {secs:.1f}s ({sent_bytes / 2**20 / max(secs, 0.001):.1f} MiB/s)")
    status, r = call(a.server, token, "POST", "/api/v1/imports", {"upload_group": group})
    if status != 201:
        sys.exit(f"import: {status} {r}")
    print(f"import batch {r['id']} with {r['files']} audio file(s)")


if __name__ == "__main__":
    main()
