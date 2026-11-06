#!/usr/bin/env python3
"""Exercise the actual shell sampler against a live mock process."""
import http.server
import multiprocessing
import os
import pathlib
import subprocess
import tempfile
import urllib.request

SCRIPT = pathlib.Path(__file__).with_name('soak-growth.sh')


def serve(pipe):
    class Handler(http.server.BaseHTTPRequestHandler):
        allocations = []
        files = []
        goroutines = 100

        def do_GET(self):
            if self.path == '/leak':
                self.allocations.append(bytearray(64 * 1024 * 1024))
            elif self.path == '/goroutines':
                type(self).goroutines = 106
            elif self.path == '/fds':
                self.files.extend(open('/dev/null', 'rb') for _ in range(1001))
            elif self.path == '/close':
                for f in self.files:
                    f.close()
                self.files.clear()
            elif self.path == '/bad':
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'wrong_metric 1\n')
                return
            body = f'urnet_goroutines {self.goroutines}\n'.encode()
            self.send_response(200)
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
    pipe.send(server.server_port)
    server.serve_forever()


def main():
    reader, writer = multiprocessing.Pipe(False)
    process = multiprocessing.Process(target=serve, args=(writer,))
    process.start()
    try:
        url = f'http://127.0.0.1:{reader.recv()}'
        with tempfile.TemporaryDirectory() as tmp:
            baseline = os.path.join(tmp, 'baseline')

            def sample(mode, expected):
                result = subprocess.run(['sh', str(SCRIPT), mode, str(process.pid), url + '/metrics', baseline], text=True, capture_output=True)
                print(f'{mode} expected={expected} rc={result.returncode}: {result.stdout.strip()} {result.stderr.strip()}', flush=True)
                assert (result.returncode == 0) == expected
                return result

            sample('baseline', True)
            sample('check', True)
            urllib.request.urlopen(url + '/goroutines').read()
            assert 'goroutines grew' in sample('check', False).stderr
            # A new block has its own warmup baseline.
            sample('baseline', True)
            sample('check', True)
            urllib.request.urlopen(url + '/fds').read()
            assert 'FD count exceeds' in sample('check', False).stderr
            urllib.request.urlopen(url + '/close').read()
            bad = subprocess.run(['sh', str(SCRIPT), 'check', str(process.pid), url + '/bad', baseline], capture_output=True)
            assert bad.returncode != 0
            urllib.request.urlopen(url + '/leak').read()
            assert 'RSS grew' in sample('check', False).stderr
    finally:
        process.terminate()
        process.join()


if __name__ == '__main__':
    main()
