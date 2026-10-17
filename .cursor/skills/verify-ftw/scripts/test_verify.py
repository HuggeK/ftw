import json
import os
import socket
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import verify as v


def status():
    return {"mode": "idle", "grid_w": 1000,
            "drivers": {name: {"status": "ok"} for name in v.BATTERIES},
            "dispatch": [{"driver": name, "target_w": 0} for name in v.BATTERIES],
            "control_feedback": [{"driver": name, "kind": "battery", "sent_w": 0,
                                  "evidence": "accepted", "status": "waiting", "readings_fresh": True,
                                  "command_at_ms": 3000, "observed_at_ms": 3000} for name in v.BATTERIES]}


class Evidence(unittest.TestCase):
    def test_both_batteries_accept_new_commands(self):
        v.idle(status(), 2000, 4000)

    def test_missing_or_bad_evidence_fails(self):
        for name, mutate in [
            ("no feedback", lambda s: s.update(control_feedback=[])),
            ("one battery", lambda s: s["control_feedback"].pop()),
            ("one target", lambda s: s["dispatch"].pop()),
            ("one driver", lambda s: s["drivers"].pop("sungrow-sim")),
            ("stale driver", lambda s: s["drivers"]["sungrow-sim"].update(status="offline")),
            ("unknown grid", lambda s: s.update(grid_w=None)),
            ("old command", lambda s: s["control_feedback"][0].update(command_at_ms=1000)),
            ("old observation", lambda s: s["control_feedback"][0].update(observed_at_ms=1000)),
            ("failed command", lambda s: s["control_feedback"][0].update(status="no_contact")),
            ("unaccepted command", lambda s: s["control_feedback"][0].update(evidence="none")),
            ("stale reading", lambda s: s["control_feedback"][0].update(readings_fresh=False)),
            ("unknown sent", lambda s: s["control_feedback"][0].update(sent_w=None)),
            ("nonzero target", lambda s: s["dispatch"][0].update(target_w=1000)),
        ]:
            with self.subTest(name=name):
                data = status()
                mutate(data)
                with self.assertRaises(ValueError):
                    v.idle(data, 2000, 4000)


class Ownership(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.env = patch.dict(os.environ, {"FTW_VERIFY_DIR": self.temp.name,
                                          "FTW_VERIFY_EVIDENCE": self.temp.name + "-evidence"})
        self.env.start()
        self.addCleanup(self.env.stop)
        self.instance = v.Instance()
        self.data = {"run": str(self.instance.run), "ports": self.instance.ports,
                     "processes": [{"pid": n, "identity": str(n)} for n in [100, 200, 300]]}
        (self.instance.run / "instance.json").write_text(json.dumps(self.data))

    def test_wrong_listener_prevents_post(self):
        with patch.object(v, "identity", side_effect=lambda pid: str(pid)), patch.object(v, "listener", return_value={999}), patch.object(v.urllib.request, "urlopen") as send:
            with self.assertRaisesRegex(ValueError, "not owned"):
                self.instance.request("/api/mode", {"mode": "idle"})
            send.assert_not_called()

    def test_dead_process_prevents_post(self):
        with patch.object(v, "identity", side_effect=ValueError("process is gone")), patch.object(v.urllib.request, "urlopen") as send:
            with self.assertRaises(ValueError):
                self.instance.request("/api/mode", {"mode": "idle"})
            send.assert_not_called()

    def test_reused_pid_prevents_cleanup(self):
        with patch.object(v, "identity", return_value="a different process"), patch.object(v.os, "kill") as kill:
            with self.assertRaisesRegex(ValueError, "reused pid"):
                self.instance.cleanup()
            kill.assert_not_called()
            self.assertTrue(self.instance.run.exists())

    def test_changed_port_prevents_post(self):
        self.instance.ports[0] += 1
        with patch.object(v.urllib.request, "urlopen") as send:
            with self.assertRaisesRegex(ValueError, "ports differ"):
                self.instance.request("/api/mode", {"mode": "idle"})
            send.assert_not_called()

    def test_occupied_port_prevents_launch(self):
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            sock.listen()
            self.instance.ports[0] = sock.getsockname()[1]
            with patch.object(v.shutil, "which", return_value="lsof"), patch.object(v.subprocess, "Popen") as start:
                with self.assertRaises(OSError):
                    self.instance.launch()
                start.assert_not_called()

    def test_listener_matches_real_process(self):
        # Exercise ps/lsof on both macOS and Linux, with an isolated listener.
        code = "import socket,time; s=socket.socket(); s.bind(('127.0.0.1',0)); s.listen(); print(s.getsockname()[1],flush=True); time.sleep(30)"
        with subprocess.Popen([sys.executable, "-u", "-c", code], stdout=subprocess.PIPE, text=True) as process:
            try:
                port = int(process.stdout.readline())
                self.assertEqual(v.listener(port), {process.pid})
                self.assertIn("python", v.identity(process.pid).lower())
            finally:
                process.terminate()
                process.wait(timeout=5)


if __name__ == "__main__":
    unittest.main()
