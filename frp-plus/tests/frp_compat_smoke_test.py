"""Offline provenance rejection and loopback monitoring-session accounting."""
import copy
import io
import json
from pathlib import Path
import socket
import tarfile
import tempfile
import unittest
from unittest import mock

import build_compat_baseline as baseline
import frp_compat_smoke as compat
from smoke import SmokeFailure, echo_server, wait_for


class CompatibilityTests(unittest.TestCase):
    def test_public_observation_rejects_disconnect_reconnect_and_stale_metrics(self):
        node = {"session": "online", "freshness": "fresh", "metrics": {}, "metrics_at": "timestamp",
                "last_seen": "timestamp", "frp": {"control_state": "connected", "proxy_total": 1}}
        compat.require_fresh(node, (1, 0))
        for counts in ((2, 1), (2, 0), (1, 1), (0, 0)):
            with self.subTest(counts=counts), self.assertRaises(SmokeFailure):
                compat.require_fresh(node, counts)
        for key, value in (("session", "offline"), ("freshness", "stale"), ("metrics", None), ("metrics_at", None)):
            changed = {**node, key: value}
            with self.subTest(field=key), self.assertRaises(SmokeFailure):
                compat.require_fresh(changed, (1, 0))

    def test_bridge_counts_real_reconnects_without_parsing_traffic(self):
        with echo_server() as port:
            bridge = compat.CountedBridge(port)
            bridge.start()
            try:
                for expected in (1, 2):
                    with socket.create_connection((compat.LOOPBACK, bridge.port), timeout=2) as connection:
                        connection.sendall(b"private fixture bytes")
                        self.assertEqual(connection.recv(64), b"private fixture bytes")
                        self.assertEqual(bridge.counts(), (expected, expected - 1))
                    wait_for("bridge closure", lambda: bridge.counts() == (expected, expected), (), 3)
            finally:
                bridge.stop()

    def manifest_fixture(self, directory):
        frp = baseline.frp
        lock = {"schema_version": 1, "repository": frp.OFFICIAL_REPOSITORY, "tag": "v1.2.3",
                "tag_object": "1" * 40, "commit": "2" * 40, "license": "Apache-2.0"}
        files = {"upstream.lock": "".join(f"{key} = {json.dumps(value)}\n" for key, value in lock.items()).encode(),
                 "patches/series": b"0001-fixture.patch\n", "patches/0001-fixture.patch": b"archived patch",
                 **{name + "/fixture": name.encode() for name in frp.OVERLAYS}}
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w") as archive:
            for name, data in files.items():
                item = tarfile.TarInfo(name)
                item.size = len(data)
                archive.addfile(item, io.BytesIO(data))
        data = stream.getvalue()
        agent, server, manifest_path = [directory / name for name in ("agent", "server", "BUILD.json")]
        agent.write_bytes(b"old agent")
        server.write_bytes(b"old server")
        manifest = {"plus_baseline": {"commit": baseline.BASELINE_COMMIT, "source_verified_clean": True,
                                      "archive_sha256": frp.digest(data)}, "upstream": lock,
                    "patches": {"0001-fixture.patch": frp.digest(files["patches/0001-fixture.patch"])},
                    "overlays": {name + "/fixture": frp.digest(name.encode()) for name in frp.OVERLAYS},
                    "binaries": {"frp-plus-agent": frp.digest(agent.read_bytes()), "frp-plus-server": frp.digest(server.read_bytes())}}
        manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
        return agent, server, manifest_path, manifest, data

    def test_old_binary_provenance_is_bound_to_archived_patch_and_overlay_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            agent, server, path, manifest, archive = self.manifest_fixture(Path(directory))
            with mock.patch.object(baseline, "baseline_archive", return_value=archive):
                baseline.verify_baseline(agent, server, path)
                for change in (
                    lambda v: v["plus_baseline"].update(commit="0" * 40),
                    lambda v: v["plus_baseline"].update(archive_sha256="0" * 64),
                    lambda v: v["plus_baseline"].update(source_verified_clean=False),
                    lambda v: v["patches"].update({"0005-frp-observation.patch": "0" * 64}),
                    lambda v: v["overlays"].update({"agent/fixture": "0" * 64}),
                    lambda v: v["binaries"].update({"frp-plus-agent": "0" * 64}),
                    lambda v: v["binaries"].update({"frp-plus-server": "0" * 64}),
                ):
                    changed = copy.deepcopy(manifest)
                    change(changed)
                    path.write_text(json.dumps(changed), encoding="utf-8")
                    with self.assertRaises(baseline.frp.PipelineError):
                        baseline.verify_baseline(agent, server, path)

    def test_baseline_archive_uses_fixed_revision_and_commit_time(self):
        result = mock.Mock(returncode=0, stdout=b"archive")
        with mock.patch.object(baseline.frp, "run", side_effect=["/fixture", "12345"]), \
             mock.patch.object(baseline.subprocess, "run", return_value=result) as command:
            self.assertEqual(baseline.baseline_archive(Path("/fixture/frp-plus")), b"archive")
        args = command.call_args.args[0]
        self.assertIn("--mtime=@12345", args)
        self.assertIn(baseline.BASELINE_COMMIT + ":frp-plus", args)
        self.assertEqual(command.call_args.kwargs["cwd"], Path("/fixture"))

    def initializer_fixture(self):
        # The archived initializer uses a schema relative to its own source,
        # just as the real baseline does. It must not import today's local.py.
        files = {
            "scripts/local.py": b"from pathlib import Path\nimport sqlite3\nROOT = Path(__file__).resolve().parents[1]\n"
                                b"def initialize(path):\n    db = sqlite3.connect(path)\n"
                                b"    db.executescript((ROOT / 'monitor/control/schema.sql').read_text())\n"
                                b"    db.close()\n",
            "monitor/control/schema.sql": b"CREATE TABLE old_fixture(value TEXT);\nPRAGMA user_version=6;\n",
        }
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w") as archive:
            for name, value in files.items():
                entry = tarfile.TarInfo(name)
                entry.size = len(value)
                archive.addfile(entry, io.BytesIO(value))
        archive = stream.getvalue()
        manifest = {"plus_baseline": {"commit": baseline.BASELINE_COMMIT, "source_verified_clean": True,
                                      "archive_sha256": baseline.frp.digest(archive)}}
        return archive, manifest

    def test_pair_initializer_uses_verified_archive_schema_and_cleans_source(self):
        archive, manifest = self.initializer_fixture()
        with tempfile.TemporaryDirectory() as temporary, mock.patch.object(compat, "baseline_archive", return_value=archive):
            database = Path(temporary).resolve() / "control.sqlite"
            with compat.baseline_initializer(manifest) as initializer:
                source = initializer.ROOT
                self.assertNotEqual(source, baseline.ROOT)
                initializer.initialize(database)
                self.assertEqual(compat.database_version(database), 6)
                # Nothing modifies the original fixture, even after an upgrade.
                with compat.sqlite3.connect(database) as current:
                    current.execute("PRAGMA user_version=9")
                self.assertEqual(compat.database_version(database), 9)
                self.assertIn("user_version=6", (source / "monitor/control/schema.sql").read_text())
            self.assertFalse(source.exists())

    def test_initializer_refuses_archive_drift_before_loading_code(self):
        archive, manifest = self.initializer_fixture()
        for changes in ({"archive_sha256": "0" * 64}, {"commit": "0" * 40}, {"source_verified_clean": False}):
            changed = copy.deepcopy(manifest)
            changed["plus_baseline"].update(changes)
            with self.subTest(changes=changes), mock.patch.object(compat, "baseline_archive", return_value=archive), \
                 mock.patch.object(compat.importlib.util, "spec_from_file_location") as loader:
                with self.assertRaises(SmokeFailure), compat.baseline_initializer(changed):
                    self.fail("invalid baseline initializer was loaded")
                loader.assert_not_called()


if __name__ == "__main__":
    unittest.main()
