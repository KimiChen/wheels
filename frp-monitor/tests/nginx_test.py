"""Exercise the shipped nginx fragment against an isolated HTTPS proxy."""
import base64
import hashlib
import http.client
import http.server
from pathlib import Path
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import unittest


ROOT = Path(__file__).resolve().parents[1]
NGINX = shutil.which('nginx') or ('/opt/homebrew/bin/nginx' if Path('/opt/homebrew/bin/nginx').is_file() else None)
OPENSSL = shutil.which('openssl')


class Backend(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *_args):
        pass

    def do_GET(self):
        if self.path.startswith('/events/'):
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.send_header('Cache-Control', 'no-cache')
            self.end_headers()
            try:
                self.wfile.write(b': ready\n\n')
                self.wfile.flush()
                while not self.server.stopping.wait(0.1):
                    self.wfile.write(b': keepalive\n\n')
                    self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                pass
            self.close_connection = True
            return
        if self.path == '/agent/v1/ws':
            if self.headers.get('Upgrade', '').lower() != 'websocket':
                self.send_error(400)
                return
            key = self.headers['Sec-WebSocket-Key']
            accept = base64.b64encode(hashlib.sha1((key + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest()).decode()
            self.send_response(101)
            self.send_header('Upgrade', 'websocket')
            self.send_header('Connection', 'Upgrade')
            self.send_header('Sec-WebSocket-Accept', accept)
            self.send_header('X-Observed-Host', self.headers.get('Host', ''))
            self.end_headers()
            self.close_connection = True
            return
        body = b'proxied\n'
        self.send_response(200)
        self.send_header('Content-Type', 'text/plain')
        self.send_header('Content-Length', str(len(body)))
        self.send_header('X-Observed-Host', self.headers.get('Host', ''))
        self.end_headers()
        self.wfile.write(body)


@unittest.skipUnless(NGINX and OPENSSL, 'nginx and openssl are required for the isolated proxy test')
class NginxTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='frp-monitor-nginx-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.backend = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Backend)
        self.backend.stopping = threading.Event()
        self.backend_thread = threading.Thread(target=self.backend.serve_forever, daemon=True)
        self.backend_thread.start()
        self.addCleanup(self.close_backend)
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            self.port = listener.getsockname()[1]
        self.authority = f'monitor.example.invalid:{self.port}'
        certificate, key = self.root / 'certificate.pem', self.root / 'key.pem'
        subprocess.run([OPENSSL, 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
                        '-subj', '/CN=monitor.example.invalid', '-out', str(certificate), '-keyout', str(key)],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=15)
        fragment = (ROOT / 'packaging/nginx.conf.example').read_text(encoding='utf-8')
        fragment = fragment.replace('listen 443 ssl;', f'listen 127.0.0.1:{self.port} ssl;')
        fragment = fragment.replace('/etc/nginx/tls/monitor.example.invalid/fullchain.pem', str(certificate))
        fragment = fragment.replace('/etc/nginx/tls/monitor.example.invalid/privkey.pem', str(key))
        fragment = fragment.replace('127.0.0.1:17401', f'127.0.0.1:{self.backend.server_port}')
        (self.root / 'monitor.conf').write_text(fragment, encoding='utf-8')
        self.config = self.root / 'nginx.conf'
        self.config.write_text(f'''worker_processes 1;
pid "{self.root / 'nginx.pid'}";
error_log "{self.root / 'error.log'}";
events {{ worker_connections 256; }}
http {{
    access_log off;
    client_body_temp_path "{self.root / 'client_body'}";
    proxy_temp_path "{self.root / 'proxy'}";
    fastcgi_temp_path "{self.root / 'fastcgi'}";
    uwsgi_temp_path "{self.root / 'uwsgi'}";
    scgi_temp_path "{self.root / 'scgi'}";
    include "{self.root / 'monitor.conf'}";
}}
''', encoding='utf-8')
        self.command = [NGINX, '-p', str(self.root) + '/', '-c', str(self.config)]
        checked = subprocess.run(self.command + ['-t'], capture_output=True, text=True, timeout=10)
        self.assertEqual(checked.returncode, 0, checked.stderr)
        self.process = subprocess.Popen(self.command + ['-g', 'daemon off;'], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        self.addCleanup(self.close_nginx)
        # Only this isolated listener uses a self-signed certificate.
        self.tls = ssl._create_unverified_context()
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                self.fail(self.process.stderr.read().decode())
            try:
                status, _, _ = self.request('/')
                if status == 200:
                    return
            except (OSError, http.client.HTTPException):
                pass
            time.sleep(0.02)
        self.fail('isolated nginx did not become ready')

    def close_nginx(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        self.process.stderr.close()

    def close_backend(self):
        self.backend.stopping.set()
        self.backend.shutdown()
        self.backend.server_close()
        self.backend_thread.join(timeout=5)

    def connect(self):
        return http.client.HTTPSConnection('127.0.0.1', self.port, timeout=3, context=self.tls)

    def request(self, path, headers=None):
        connection = self.connect()
        try:
            connection.request('GET', path, headers={'Host': self.authority, **(headers or {})})
            response = connection.getresponse()
            return response.status, dict(response.getheaders()), response.read()
        finally:
            connection.close()

    def stream(self, path):
        connection = self.connect()
        self.addCleanup(connection.close)
        connection.request('GET', path, headers={'Host': self.authority})
        response = connection.getresponse()
        self.addCleanup(response.close)
        self.assertEqual(response.status, 200)
        self.assertEqual(response.readline(), b': ready\n')
        return response

    def test_oauth_source_limit_cannot_be_bypassed_with_forwarded_header(self):
        path = '/api/admin/v1/auth/github'
        statuses = [self.request(path, {'X-Forwarded-For': f'198.51.100.{i}'})[0] for i in range(1, 4)]
        self.assertEqual(statuses, [200, 200, 429])
        status, headers, body = self.request(path + '/callback?code=fixture&state=fixture')
        self.assertEqual(status, 200)
        self.assertEqual(body, b'proxied\n')
        self.assertEqual(headers['X-Observed-Host'], self.authority)

    def test_public_sse_fifth_connection_is_rejected_without_blocking_admin(self):
        for _ in range(4):
            self.stream('/events/public')
        self.assertEqual(self.request('/events/public')[0], 429)
        for _ in range(8):
            self.stream('/events/admin')
        self.assertEqual(self.request('/events/admin')[0], 429)

    def test_public_api_requests_are_rate_limited(self):
        statuses = [self.request('/api/public/v1/nodes')[0] for _ in range(40)]
        self.assertEqual(statuses[0], 200)
        self.assertIn(429, statuses)
        self.assertTrue(set(statuses) <= {200, 429})

    def test_wss_upgrade_and_host_are_forwarded(self):
        status, headers, _ = self.request('/agent/v1/ws', {
            'Upgrade': 'websocket', 'Connection': 'Upgrade',
            'Sec-WebSocket-Version': '13', 'Sec-WebSocket-Key': 'dGhlIHNhbXBsZSBub25jZQ==',
        })
        self.assertEqual(status, 101)
        self.assertEqual(headers['Sec-WebSocket-Accept'], 's3pPLMBiTxaQ9kYGzzhZRbK+xOo=')
        self.assertEqual(headers['X-Observed-Host'], self.authority)


if __name__ == '__main__':
    unittest.main()
