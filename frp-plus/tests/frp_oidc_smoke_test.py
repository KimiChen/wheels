import base64
from contextlib import closing
import http.client
import json
from pathlib import Path
import tempfile
import unittest
from urllib.parse import urlencode, urlsplit

import frp_oidc_smoke as fixture
from frp_transport_smoke import certificate


class OIDCFixtureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory(prefix="frp-oidc-fixture-")
        cls.root = Path(cls.temporary.name)
        cls.cert, cls.key = certificate(cls.root, "issuer")
    @classmethod
    def tearDownClass(cls):
        cls.temporary.cleanup()

    def request(self, issuer, path, secret=None):
        parsed = urlsplit(issuer)
        with closing(http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=3)) as connection:
            if secret is None:
                connection.request("GET", path)
            else:
                body = urlencode({"grant_type": "client_credentials", "audience": fixture.AUDIENCE})
                basic = base64.b64encode((fixture.CLIENT_ID + ":" + secret).encode()).decode()
                connection.request("POST", path, body, {"Authorization": "Basic " + basic, "Content-Type": "application/x-www-form-urlencoded"})
            response = connection.getresponse()
            return response.status, json.loads(response.read(8192))

    def test_discovery_jwks_and_credentials_produce_verifiable_signature(self):
        with fixture.identity_provider(self.key, self.key, "local-secret", "valid") as (issuer, counts):
            status, discovery = self.request(issuer, "/.well-known/openid-configuration")
            self.assertEqual(status, 200)
            self.assertEqual(discovery["issuer"], issuer)
            status, keys = self.request(issuer, "/jwks")
            self.assertEqual(status, 200)
            self.assertEqual(keys["keys"][0]["alg"], "RS256")
            self.assertEqual(self.request(issuer, "/token", "wrong")[0], 401)
            status, body = self.request(issuer, "/token", "local-secret")
            self.assertEqual(status, 200)
            header, claims, signature = body["access_token"].split(".")
            payload = json.loads(base64.urlsafe_b64decode(claims + "=" * (-len(claims) % 4)))
            self.assertEqual(payload["aud"], fixture.AUDIENCE)
            public = self.root / "public.pem"
            public.write_bytes(fixture.openssl(["pkey", "-in", str(self.key), "-pubout"]))
            sig = self.root / "signature.bin"
            sig.write_bytes(base64.urlsafe_b64decode(signature + "=" * (-len(signature) % 4)))
            result = fixture.openssl(["dgst", "-sha256", "-verify", str(public), "-signature", str(sig)], input=(header + "." + claims).encode())
            self.assertIn(b"Verified OK", result)
            self.assertEqual(counts, {"discovery": 1, "jwks": 1, "token": 1, "rejected": 1})

    def test_expired_token_stays_expired_despite_oauth_response_expiry(self):
        with fixture.identity_provider(self.key, self.key, "local-secret", "expired") as (issuer, _):
            status, body = self.request(issuer, "/token", "local-secret")
            self.assertEqual(status, 200)
            encoded = body["access_token"].split(".")[1]
            payload = json.loads(base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4)))
            self.assertLess(payload["exp"], fixture.time.time())


if __name__ == "__main__":
    unittest.main()
