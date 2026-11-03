#!/usr/bin/env python3
"""A stand-in for MiniFin's relay, for testing the player's seeking.

  GET /v/NAME.ts[?t=S]  live transcode of MEDIA/NAME.ts from S (like the
                        server): no Content-Length, not seekable, and the
                        header X-MiniFin-Start: S.
  GET /d/NAME.ts[?t=S]  offline-style: stream copy from the keyframe at or
                        before S; X-MiniFin-Start gives that keyframe's time.
  ?rate=KBIT            video bit rate for /v (default 1200).
  ?stall=SEC            stop sending after SEC seconds of output (keeps the
                        connection open) to test a stalled network.

  python3 -I tools/test-relay.py MEDIA_DIR [PORT]   (needs ffmpeg, ffprobe)
"""
import http.server, math, os, subprocess, sys, time, urllib.parse

MEDIA = sys.argv[1]
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 8000


def keyframe_before(path, t):
    if t <= 0:
        return 0.0
    out = subprocess.run(
        ["ffprobe", "-v", "error", "-select_streams", "v:0", "-skip_frame", "nokey",
         "-show_entries", "frame=pts_time", "-of", "csv=p=0",
         "-read_intervals", "%f%%%f" % (max(0, t - 12), t + 0.05), path],
        capture_output=True, text=True).stdout
    times = [float(x) for x in out.split() if x.strip()]
    start = float(subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=start_time",
                                  "-of", "csv=p=0", path], capture_output=True, text=True).stdout or 0)
    best = [x - start for x in times if x - start <= t + 0.001]
    return max(best) if best else 0.0


class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        q = urllib.parse.parse_qs(u.query)
        parts = u.path.strip("/").split("/")
        if len(parts) != 2 or parts[0] not in ("v", "d"):
            return self.send_error(404)
        path = os.path.join(MEDIA, os.path.basename(parts[1]))
        if not os.path.exists(path):
            return self.send_error(404)
        try:
            t = float(q.get("t", ["0"])[0])
            if not math.isfinite(t) or t < 0:
                raise ValueError
        except ValueError:
            return self.send_error(400)
        rate = int(q.get("rate", ["1200"])[0])
        stall = float(q.get("stall", ["0"])[0])
        if parts[0] == "v":
            begins = t
            cmd = ["ffmpeg", "-v", "error", "-ss", "%.3f" % t, "-i", path, "-c:v", "libx264", "-preset", "ultrafast",
                   "-tune", "zerolatency", "-profile:v", "main", "-b:v", "%dk" % rate, "-maxrate", "%dk" % rate,
                   "-bufsize", "%dk" % (rate * 2), "-g", "72", "-bf", "0", "-c:a", "aac", "-b:a", "32k", "-ac", "2",
                   "-f", "mpegts", "pipe:1"]
        else:
            begins = keyframe_before(path, t)
            cmd = ["ffmpeg", "-v", "error", "-ss", "%.3f" % begins, "-i", path, "-c", "copy", "-f", "mpegts", "pipe:1"]
        sys.stderr.write("relay: %s t=%.3f begins=%.3f\n" % (u.path, t, begins))
        self.send_response(200)
        self.send_header("Content-Type", "video/mp2t")
        self.send_header("X-MiniFin-Start", "%.3f" % begins)
        self.send_header("Connection", "close")
        self.end_headers()
        p = subprocess.Popen(cmd, stdout=subprocess.PIPE)
        t0 = time.time()
        try:
            while True:
                b = p.stdout.read(16384)
                if not b:
                    break
                self.wfile.write(b)
                if stall and time.time() - t0 > stall:
                    time.sleep(3600)
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            p.kill()
            p.wait()
        self.close_connection = True


http.server.ThreadingHTTPServer(("0.0.0.0", PORT), H).serve_forever()
