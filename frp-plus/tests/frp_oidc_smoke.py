#!/usr/bin/env python3
"""Native OIDC discovery, client credentials, JWKS and signed JWT regression.

The disposable identity service is HTTP on loopback; FRP control uses verified
TLS. No production identity provider or account is contacted.
"""
from __future__ import annotations

import argparse
import base64
from contextlib import contextmanager, ExitStack
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import signal
import subprocess
import sys
import tempfile
import threading
import time
from urllib.parse import parse_qs

from smoke import (LOOPBACK, SmokeFailure, child_environment, interrupted,
                   positive_timeout, port_open, reserve_port, verify, wait_for, write_private)
from frp_transport_smoke import (certificate, config_texts, observed_child,
                                 require_payload, require_rejection, transport_echo_server)

AUDIENCE = "frp-fixture"
CLIENT_ID = "fixture-client"
OIDC_FAILURES = {
    "audience": (b"expected audience",),
    "issuer": (b"id token issued by a different provider",),
    "expired": (b"token is expired",),
    "signature": (b"failed to verify signature",),
    "credentials": (b"invalid_client",),
}


def b64url(value):
    return base64.urlsafe_b64encode(value).decode("ascii").rstrip("=")


def openssl(args, *, input=None):
    result = subprocess.run(["openssl", *args], input=input, stdout=subprocess.PIPE,
                            stderr=subprocess.DEVNULL, timeout=20, env=child_environment())
    if result.returncode:
        raise SmokeFailure("OIDC fixture cryptographic operation failed")
    return result.stdout


def jwk(key):
    public = openssl(["pkey", "-in", str(key), "-pubout"])
    details = openssl(["pkey", "-pubin", "-text", "-noout"], input=public)
    if b"Exponent: 65537 " not in details:
        raise SmokeFailure("OIDC fixture RSA exponent was unexpected")
    modulus = openssl(["rsa", "-pubin", "-modulus", "-noout"], input=public).strip()
    if not modulus.startswith(b"Modulus="):
        raise SmokeFailure("OIDC fixture public modulus was unavailable")
    try:
        n = bytes.fromhex(modulus.split(b"=", 1)[1].decode("ascii"))
    except (ValueError, UnicodeError) as error:
        raise SmokeFailure("OIDC fixture public modulus was invalid") from error
    if len(n) != 256:
        raise SmokeFailure("OIDC fixture requires a 2048-bit RSA key")
    return {"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "fixture-key", "n": b64url(n), "e": "AQAB"}


def signed_token(key, issuer, mode):
    now = int(time.time())
    claims = {"iss": issuer + "/wrong" if mode == "issuer" else issuer,
              "sub": CLIENT_ID, "aud": "wrong-audience" if mode == "audience" else AUDIENCE,
              "iat": now - 3600, "exp": now - 300 if mode == "expired" else now + 3600}
    header = {"alg": "RS256", "typ": "JWT", "kid": "fixture-key"}
    data = (b64url(json.dumps(header, separators=(",", ":")).encode()) + "." +
            b64url(json.dumps(claims, separators=(",", ":")).encode())).encode("ascii")
    return data.decode("ascii") + "." + b64url(openssl(["dgst", "-sha256", "-sign", str(key)], input=data))


@contextmanager
def identity_provider(key, signing_key, secret, mode):
    counts = {"discovery": 0, "jwks": 0, "token": 0, "rejected": 0}
    public_key = jwk(key)
    lock = threading.Lock()
    class Handler(BaseHTTPRequestHandler):
        def setup(self):
            super().setup()
            self.connection.settimeout(3)
        def log_message(self, *_):
            pass
        def respond(self, status, body):
            data = json.dumps(body, separators=(",", ":")).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(data)
        def do_GET(self):
            if self.path == "/.well-known/openid-configuration":
                with lock: counts["discovery"] += 1
                self.respond(200, {"issuer": issuer, "authorization_endpoint": issuer + "/unused",
                                   "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks",
                                   "response_types_supported": ["code"], "subject_types_supported": ["public"],
                                   "id_token_signing_alg_values_supported": ["RS256"]})
            elif self.path == "/jwks":
                with lock: counts["jwks"] += 1
                self.respond(200, {"keys": [public_key]})
            else:
                self.respond(404, {"error": "not_found"})
        def do_POST(self):
            try:
                size = int(self.headers.get("Content-Length", "0"))
                if self.path != "/token" or not 0 < size <= 8192:
                    raise ValueError()
                body = parse_qs(self.rfile.read(size).decode("utf-8"), strict_parsing=True)
                basic = "Basic " + base64.b64encode((CLIENT_ID + ":" + secret).encode()).decode()
                valid = (self.headers.get("Authorization") == basic or
                         body.get("client_id") == [CLIENT_ID] and body.get("client_secret") == [secret])
                valid = valid and body.get("grant_type") == ["client_credentials"] and body.get("audience") == [AUDIENCE]
            except (ValueError, UnicodeError):
                valid = False
            if not valid:
                with lock: counts["rejected"] += 1
                self.respond(401, {"error": "invalid_client"})
                return
            with lock: counts["token"] += 1
            self.respond(200, {"access_token": token, "token_type": "Bearer", "expires_in": 3600})
    with ThreadingHTTPServer((LOOPBACK, 0), Handler) as server:
        issuer = f"http://{LOOPBACK}:{server.server_port}"
        token = signed_token(signing_key, issuer, mode)
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": .05}, daemon=True)
        thread.start()
        try:
            yield issuer, counts
        finally:
            server.shutdown()
            thread.join(timeout=2)
            if thread.is_alive():
                raise SmokeFailure("OIDC fixture did not stop")


def run_case(agent, server, wire, mode, timeout):
    with tempfile.TemporaryDirectory(prefix="frp-oidc-") as temporary, ExitStack() as stack:
        root = Path(temporary).resolve()
        root.chmod(0o700)
        control, remote = stack.enter_context(reserve_port()), stack.enter_context(reserve_port())
        control_port, remote_port = control.getsockname()[1], remote.getsockname()[1]
        target_port = stack.enter_context(transport_echo_server())
        cert, key = certificate(root, "oidc")
        signing_key = certificate(root, "other-signing")[1] if mode == "signature" else key
        secret = secrets.token_hex(24)
        issuer, counts = stack.enter_context(identity_provider(key, signing_key, secret, mode))
        server_text, client_text = config_texts("tcp", wire, control_port, 0, remote_port,
                                               target_port, "unused-fixture-token", cert, key, mux=False)
        server_auth = f'auth.method = "oidc"\nauth.additionalScopes = ["HeartBeats", "NewWorkConns"]\nauth.oidc.issuer = {json.dumps(issuer)}\nauth.oidc.audience = "{AUDIENCE}"\n'
        client_auth = (f'auth.method = "oidc"\nauth.additionalScopes = ["HeartBeats", "NewWorkConns"]\n'
                       f'auth.oidc.clientID = "{CLIENT_ID}"\nauth.oidc.clientSecret = {json.dumps(secret + "-wrong" if mode == "credentials" else secret)}\n'
                       f'auth.oidc.audience = "{AUDIENCE}"\nauth.oidc.tokenEndpointURL = {json.dumps(issuer + "/token")}\n')
        old_auth = 'auth.method = "token"\nauth.token = "unused-fixture-token"\n'
        assert server_text.count(old_auth) == client_text.count(old_auth) == 1
        server_config, client_config = root / "server.toml", root / "client.toml"
        write_private(server_config, server_text.replace(old_auth, server_auth))
        write_private(client_config, client_text.replace(old_auth, client_auth))
        verify(server, server_config, "OIDC server configuration", timeout)
        verify(agent, client_config, "OIDC client configuration", timeout)
        control.close()
        service = stack.enter_context(observed_child("OIDC server", server, server_config, root, OIDC_FAILURES))
        wait_for("OIDC server startup", lambda: port_open(control_port), (service,), timeout)
        remote.close()
        client = stack.enter_context(observed_child("OIDC client", agent, client_config, root, OIDC_FAILURES))
        if mode == "valid":
            wait_for("OIDC login and registration", lambda: client.logged_in.is_set() and client.registered.is_set(), (service, client), timeout)
            for _ in range(4):
                require_payload(remote_port)
        else:
            require_rejection(client, service, remote_port, mode, timeout)
        if not counts["discovery"] or (not counts["token"] if mode != "credentials" else not counts["rejected"]):
            raise SmokeFailure("OIDC case did not exercise discovery and token authentication")
        if mode in ("valid", "signature") and not counts["jwks"]:
            raise SmokeFailure("OIDC case did not fetch real signing keys")
    print(f"PASS oidc-{wire}-{mode}", flush=True)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent", type=Path, required=True)
    parser.add_argument("--server", type=Path, required=True)
    parser.add_argument("--wire", choices=("v1", "v2"), action="append")
    parser.add_argument("--timeout", type=positive_timeout, default=20)
    args = parser.parse_args(argv)
    for binary in (args.agent, args.server):
        if not binary.is_file() or not os.access(binary, os.X_OK):
            parser.error("each binary must be an existing native executable")
    agent, server = args.agent.resolve(), args.server.resolve()
    print(json.dumps({"agent_sha256": hashlib.sha256(agent.read_bytes()).hexdigest(),
                      "server_sha256": hashlib.sha256(server.read_bytes()).hexdigest()}), flush=True)
    for wire in dict.fromkeys(args.wire or ("v1", "v2")):
        for mode in ("valid", *OIDC_FAILURES):
            run_case(agent, server, wire, mode, args.timeout)
    return 0


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        print("FAIL: interrupted; isolated processes cleaned up", file=sys.stderr)
        raise SystemExit(130)
    except (SmokeFailure, subprocess.TimeoutExpired) as error:
        print("FAIL: " + str(error), file=sys.stderr)
        raise SystemExit(1)
