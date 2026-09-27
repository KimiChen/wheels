#!/usr/bin/env python3
"""Real TLS binary acceptance for sessions, trusted reconciliation and revocation."""
import argparse
from contextlib import ExitStack
import hashlib
import json
from pathlib import Path
import secrets
import signal
import sys
import tempfile
import time

from p1_smoke import PublicAPI, assert_redacted, generate_certificate
from smoke import (LOOPBACK, SmokeFailure, child, echo_matches, echo_server, interrupted,
                   port_open, positive_timeout, reserve_port, verify, wait_for, write_private)


class AdminAPI(PublicAPI):
    cookie = ''
    csrf = ''

    def request(self, method, path, data=None, *, authenticate=True, csrf=True, origin=None):
        connection = self.connection()
        headers = {'Content-Type': 'application/json'}
        if authenticate:
            headers['Cookie'] = self.cookie
        if csrf:
            headers['X-CSRF-Token'] = self.csrf
        if origin:
            headers['Origin'] = origin
        try:
            connection.request(method, path, body=json.dumps(data) if data is not None else None, headers=headers)
            response = connection.getresponse()
            body = response.read(4 * 1024 * 1024)
            return response.status, dict(response.getheaders()), body
        finally:
            connection.close()

    def json(self, method, path, data=None, expected=200, **kwargs):
        status, _, body = self.request(method, '/api/admin/v1/' + path, data, **kwargs)
        if status != expected:
            raise SmokeFailure(f'admin {method} {path.split("/")[0]} returned {status}, expected {expected}')
        return json.loads(body) if body and 200 <= expected < 300 else None

    def login(self, token):
        status, headers, body = self.request('POST', '/api/admin/v1/login', {'token': token}, authenticate=False, csrf=False)
        if status != 200:
            raise SmokeFailure('administrator login failed')
        cookie = headers.get('Set-Cookie', '')
        if not all(item in cookie for item in ('HttpOnly', 'Secure', 'SameSite=Strict')):
            raise SmokeFailure('admin cookie lacks TLS protection')
        self.cookie = cookie.split(';')[0]
        self.csrf = json.loads(body)['csrf_token']


def run(agent, monitor, timeout):
    with tempfile.TemporaryDirectory(prefix='frp-monitor-p3-') as temporary, ExitStack() as stack:
        folder = Path(temporary).resolve()
        folder.chmod(0o700)
        sockets = [stack.enter_context(reserve_port()) for _ in range(3)]
        control_port, remote_port, monitor_port = [s.getsockname()[1] for s in sockets]
        echo_port = stack.enter_context(echo_server())
        certificate, key = generate_certificate(folder)
        admin_token, agent_token, frp_token = (secrets.token_urlsafe(32) for _ in range(3))
        identifier = 'p3-smoke-node'
        binding = {'server_id': 'p3-smoke', 'user': 'private-tenant', 'raw_client_id': 'private-p3-client'}
        write_private(folder / 'admin.json', json.dumps({'token_sha256': hashlib.sha256(admin_token.encode()).hexdigest()}))
        write_private(folder / 'agent.token', agent_token + '\n')
        write_private(folder / 'credentials.json', json.dumps([{'agent_id': identifier, 'name': 'P3 public node',
            'token_sha256': hashlib.sha256(agent_token.encode()).hexdigest(), 'frp_binding': binding}]))
        write_private(folder / 'probes.json', '{"version":1,"nodes":[]}')
        common = f'auth.method = "token"\nauth.token = "{frp_token}"\nlog.to = "console"\nlog.level = "info"\nlog.disablePrintColor = true\n'
        q = lambda name: json.dumps(str(folder / name))
        server_config, client_config = folder / 'server.toml', folder / 'agent.toml'
        write_private(server_config, f'bindAddr = "{LOOPBACK}"\nproxyBindAddr = "{LOOPBACK}"\nbindPort = {control_port}\n' + common +
            f'\n[monitor]\nenabled = true\nbindAddr = "{LOOPBACK}"\nbindPort = {monitor_port}\nserverID = "p3-smoke"\n'
            f'certFile = {json.dumps(str(certificate))}\nkeyFile = {json.dumps(str(key))}\ncredentialsFile = {q("credentials.json")}\n'
            f'adminCredentialsFile = {q("admin.json")}\nprobeTasksFile = {q("probes.json")}\ndatabaseFile = {q("history.sqlite")}\n')
        write_private(client_config, f'serverAddr = "{LOOPBACK}"\nserverPort = {control_port}\nloginFailExit = false\n'
            f'clientID = "{binding["raw_client_id"]}"\nuser = "{binding["user"]}"\n' + common +
            f'\n[telemetry]\nenabled = true\nserverID = "p3-smoke"\nendpoint = "wss://{LOOPBACK}:{monitor_port}/agent/v1/ws"\n'
            f'tokenFile = {q("agent.token")}\ncaFile = {json.dumps(str(certificate))}\n'
            f'\n[[proxies]]\nname = "private-p3-tunnel"\ntype = "tcp"\nlocalIP = "{LOOPBACK}"\nlocalPort = {echo_port}\nremotePort = {remote_port}\n')
        verify(monitor, server_config, 'P3 server config', timeout)
        verify(agent, client_config, 'P3 agent config', timeout)
        for sock in sockets:
            sock.close()
        server = stack.enter_context(child('P3 monitor', [str(monitor), '-c', str(server_config)], folder))
        wait_for('P3 listener', lambda: port_open(monitor_port) and port_open(control_port), (server,), timeout)
        api = AdminAPI(monitor_port, certificate)
        wait_for('P3 ready', lambda: api.node()['session'] == 'waiting', (server,), timeout)
        api.json('GET', 'nodes', expected=401, authenticate=False)
        api.json('POST', 'login', {'token': agent_token}, expected=401, authenticate=False, csrf=False)
        api.login(admin_token)
        api.json('POST', 'nodes', {'name': 'no CSRF'}, expected=403, csrf=False)
        api.json('POST', 'nodes', {'name': 'wrong origin'}, expected=403, origin='https://outside.example.invalid')
        status, headers, page = api.get('/admin/')
        if status != 200 or b'Content-Security-Policy' in page or 'script-src' not in headers.get('Content-Security-Policy', ''):
            raise SmokeFailure('admin shell/CSP unavailable')
        client = stack.enter_context(child('P3 agent', [str(agent), '-c', str(client_config)], folder))
        wait_for('P3 tunnel', lambda: echo_matches(remote_port), (server, client), timeout)
        wait_for('P3 monitoring', lambda: api.node()['session'] == 'online' and api.node()['freshness'] == 'fresh', (server, client), timeout)
        def matched():
            result = api.json('GET', 'nodes')
            rows = result['frp']['nodes']
            return rows and rows[0]['state'] == 'matched' and rows[0]['proxies'] and rows[0]['proxies'][0]['server_state'] == 'registered'
        wait_for('trusted server registry/proxy reconciliation', matched, (server, client), timeout)
        private = api.json('GET', 'nodes')
        if not private['nodes'][0]['facts'] or not private['nodes'][0]['frp_binding'] or not private['frp']['proxies']:
            raise SmokeFailure('private admin detail missing')
        hidden = (admin_token, agent_token, frp_token, binding['raw_client_id'], binding['user'], 'private-p3-tunnel', LOOPBACK)
        assert_redacted(api.snapshot(), hidden)
        assert_redacted(api.event(), hidden)
        encoded = json.dumps(private)
        if any(value in encoded for value in (admin_token, agent_token, frp_token, hashlib.sha256(agent_token.encode()).hexdigest(), '"token_sha256"', '"token"')):
            raise SmokeFailure('private read API exposed a credential')
        print('PASS P3: TLS sessions/CSRF/origin protection, trusted tunnel reconciliation and public/private field separation', flush=True)

        api.json('DELETE', f'nodes/{identifier}/binding', expected=204)
        if api.json('GET', 'nodes')['frp']['nodes'][0]['state'] != 'unbound':
            raise SmokeFailure('agent claim incorrectly auto-rebound after trusted binding removal')
        api.json('PUT', f'nodes/{identifier}/binding', binding, expected=204)
        new = api.json('POST', 'nodes', {'name': 'New waiting node'}, expected=201)
        if len(new['token']) != 43:
            raise SmokeFailure('created node token invalid')
        api.json('DELETE', f'nodes/{new["id"]}', expected=204)
        book = api.json('GET', 'probes')
        book['version'] += 1
        book['nodes'] = [{'agent_id': identifier, 'tasks': [{'id': 'disabled-agent-probe', 'name': 'Public TCP label', 'target': f'{LOOPBACK}:{control_port}', 'interval': 5}]}]
        api.json('PUT', 'probes', book)
        api.json('PUT', 'probes', book, expected=409)
        rotated = api.json('POST', f'nodes/{identifier}/rotate')
        wait_for('rotation disconnects old monitoring session', lambda: api.node()['session'] != 'online', (server, client), timeout)
        if not echo_matches(remote_port):
            raise SmokeFailure('credential rotation affected native FRP forwarding')
        # Restart agent with newly delivered token; no need to wait for its 401 retry backoff.
        client.stop()
        temporary_token = folder / 'agent.next'
        write_private(temporary_token, rotated['token'] + '\n')
        temporary_token.replace(folder / 'agent.token')
        client = stack.enter_context(child('P3 rotated agent', [str(agent), '-c', str(client_config)], folder))
        wait_for('rotated agent monitoring', lambda: api.node()['session'] == 'online', (server, client), timeout)
        wait_for('rotated agent forwarding', lambda: echo_matches(remote_port), (server, client), timeout)
        api.json('DELETE', f'nodes/{identifier}', expected=204)
        def empty():
            status, _, body = api.get('/api/public/v1/nodes')
            return status == 200 and json.loads(body)['nodes'] == []
        wait_for('revoked node disappears', empty, (server, client), timeout)
        if api.get(f'/api/public/v1/nodes/{identifier}/history?window=1h')[0] != 404:
            raise SmokeFailure('revoked node history still public')
        if not echo_matches(remote_port):
            raise SmokeFailure('node revocation affected native FRP forwarding')
        if json.loads((folder / 'credentials.json').read_text()) != []:
            raise SmokeFailure('last-node revocation did not persist an empty credential list')
        api.json('POST', 'logout', expected=204)
        api.json('GET', 'nodes', expected=401)
        print('PASS P3: explicit binding, one-time enrollment/rotation, versioned tasks, last-node revocation and independent FRP forwarding', flush=True)
        client.stop()
        server.stop()
        if client.forced_stop or server.forced_stop:
            raise SmokeFailure('P3 processes required forced shutdown')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--agent', type=Path, required=True)
    parser.add_argument('--server', type=Path, required=True)
    parser.add_argument('--timeout', type=positive_timeout, default=30)
    args = parser.parse_args()
    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, interrupted)
    try:
        run(args.agent.resolve(), args.server.resolve(), args.timeout)
    except (SmokeFailure, OSError, ValueError) as exc:
        print(f'FAIL P3 acceptance: {exc}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
