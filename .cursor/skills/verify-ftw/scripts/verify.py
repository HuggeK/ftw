"""Run isolated simulators and retain API evidence for the supported journey."""
import json
import math
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import time
import urllib.request

BATTERIES = {"ferroamp-sim", "sungrow-sim"}


def require(ok, message):
    if not ok:
        raise ValueError(message)


def number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def identity(pid):
    result = subprocess.run(["ps", "-p", str(pid), "-o", "lstart=", "-o", "command="], capture_output=True, text=True)
    value = result.stdout.strip()
    require(result.returncode == 0 and value and "<defunct>" not in value, f"process {pid} is gone")
    return value


def listener(port):
    result = subprocess.run(["lsof", "-nP", f"-iTCP:{port}", "-sTCP:LISTEN", "-t"], capture_output=True, text=True)
    require(result.returncode in (0, 1), result.stderr or "lsof failed")
    return {int(pid) for pid in result.stdout.split()}


def available(ports):
    require(len(set(ports)) == len(ports), "each service needs a distinct port")
    sockets = []
    try:
        for port in ports:
            require(0 < port < 65536, f"invalid port: {port}")
            sock = socket.socket()
            sockets.append(sock)
            sock.bind(("127.0.0.1", port))
    finally:
        for sock in sockets:
            sock.close()


def live(status):
    drivers = status.get("drivers") or {}
    require(all((drivers.get(name) or {}).get("status") == "ok" for name in BATTERIES), "both simulator drivers must report ok")
    require(number(status.get("grid_w")), "grid_w must be a live numeric value")


def idle(status, action_ms, now_ms):
    live(status)
    require(status.get("mode") == "idle", "status must report idle")
    targets = status.get("dispatch") or []
    require(len(targets) == len(BATTERIES) and {d["driver"] for d in targets} == BATTERIES, "dispatch must contain both batteries exactly once")
    require(all(number(d.get("target_w")) and d["target_w"] == 0 for d in targets), "idle targets must be 0 W")
    sent = [c for c in status.get("control_feedback") or [] if c.get("kind") == "battery"]
    require(len(sent) == len(BATTERIES) and {c["driver"] for c in sent} == BATTERIES, "feedback must contain both batteries exactly once")
    for command in sent:
        require(number(command.get("sent_w")) and command["sent_w"] == 0, "each battery must receive 0 W")
        require(command.get("evidence") in ("accepted", "measured", "confirmed"), "each driver must accept its command")
        require(command.get("status") in ("waiting", "following", "limited"), "battery control must remain available")
        require(command.get("readings_fresh") is True, "battery readings must be fresh")
        for field in ("command_at_ms", "observed_at_ms"):
            at = command.get(field)
            require(number(at) and action_ms <= at <= now_ms, f"{field} must follow this action")


class Instance:
    def __init__(self):
        self.repo = Path(__file__).resolve().parents[4]
        self.run = Path(os.environ.get("FTW_VERIFY_DIR", str(Path(os.environ.get("TMPDIR", "/tmp")) / "ftw-verify"))).resolve()
        self.evidence = Path(os.environ.get("FTW_VERIFY_EVIDENCE", str(self.repo / ".verify-evidence"))).resolve()
        self.ports = [int(os.environ.get(f"FTW_VERIFY_{key}_PORT", default)) for key, default in
                      [("API", "18080"), ("MQTT", "21883"), ("MODBUS", "25502"), ("OCPP", "18887"), ("OCPP201", "18888")]]
        self.base = f"http://127.0.0.1:{self.ports[0]}"

    def manifest(self):
        data = json.loads((self.run / "instance.json").read_text())
        require(data["run"] == str(self.run) and data["ports"] == self.ports, "run directory or ports differ from the recorded instance")
        require(len(data["processes"]) == 3, "instance must record all three processes")
        return data

    def check(self):
        data = self.manifest()
        for process in data["processes"]:
            require(identity(process["pid"]) == process["identity"], f"process {process['pid']} changed identity")
        require(listener(self.ports[0]) == {data["processes"][2]["pid"]}, "API port is not owned by our ftw process")

    def request(self, path, body=None):
        # Every request, including the POST, checks the recorded process and listener.
        self.check()
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(self.base + path, data=data, headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(request, timeout=5) as response:
            return json.load(response)

    def launch(self):
        require(shutil.which("lsof"), "lsof is required on macOS and Linux")
        available(self.ports)
        require(self.run != self.evidence and self.run not in self.evidence.parents, "evidence must live outside the scratch directory")
        self.run.mkdir(parents=True)  # Refuse existing state, including a second launch.
        (self.run / "bin").mkdir()
        (self.run / "data").mkdir()
        self.evidence.mkdir(parents=True, exist_ok=True)
        processes = []
        try:
            if not all((self.repo / f"drivers/{name}.lua").exists() for name in ("ferroamp", "sungrow")):
                subprocess.run(["bash", "scripts/sync-bundled-drivers.sh"], cwd=self.repo, check=True)
            subprocess.run(["go", "build", "-o", str(self.run / "bin") + "/", "./cmd/ftw", "./cmd/sim-ferroamp", "./cmd/sim-sungrow"], cwd=self.repo / "go", check=True)
            config = (self.repo / "config.local.example.yaml").read_text()
            for old, new in [("port: 1883", f"port: {self.ports[1]}"), ("port: 5502", f"port: {self.ports[2]}"),
                             ("  port: 8080", f"  port: {self.ports[0]}"), ("port: 8887", f"port: {self.ports[3]}"),
                             ("port_v201: 8888", f"port_v201: {self.ports[4]}"),
                             ("path: ../dev-data/state.db", f"path: {self.run}/data/state.db"),
                             ("lua: drivers/", f"lua: {self.repo}/drivers/")]:
                require(old in config, f"simulator config is missing {old!r}")
                config = config.replace(old, new)
            (self.run / "config.yaml").write_text(config)
            commands = [("sim-ferroamp", ["-addr", f"127.0.0.1:{self.ports[1]}"]),
                        ("sim-sungrow", ["-addr", f"tcp://127.0.0.1:{self.ports[2]}"]),
                        ("ftw", ["-config", str(self.run / "config.yaml"), "-drivers", str(self.repo / "drivers"), "-web", str(self.repo / "web")])]
            records = []
            for name, args in commands:
                with (self.run / f"{name}.log").open("w") as log:
                    process = subprocess.Popen([str(self.run / "bin" / name), *args], cwd=self.repo, stdout=log, stderr=log)
                processes.append(process)
                records.append({"pid": process.pid, "identity": identity(process.pid)})
                if name == "sim-sungrow":
                    time.sleep(1)
            (self.run / "pids").write_text("".join(f"{p.pid}\n" for p in processes))
            (self.run / "instance.json").write_text(json.dumps({"run": str(self.run), "ports": self.ports, "processes": records}))
            deadline = time.monotonic() + 90
            while time.monotonic() < deadline:
                require(all(p.poll() is None for p in processes), "a simulator or Core exited; inspect the run logs")
                try:
                    status = self.request("/api/status")
                    live(status)
                except (OSError, ValueError):
                    time.sleep(1)
                    continue
                (self.run / "status.json").write_text(json.dumps(status, indent=2))
                print(f"ready: {self.base} (state and logs in {self.run})")
                return
            raise ValueError("not ready after 90 s; inspect the run logs")
        except BaseException:
            # These are still our child handles. A failed launch cannot leave them running.
            for process in processes:
                if process.poll() is None:
                    process.terminate()
            for process in processes:
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            raise

    def doctor(self):
        status = self.request("/api/status")
        live(status)
        for name in sorted(BATTERIES):
            print("driver", name, status["drivers"][name]["status"])
        print("grid_w", status["grid_w"], "mode", status.get("mode"))

    def drive(self, mode):
        require(mode == "idle", "only the idle journey has dispatch assertions")
        self.check()
        out = self.evidence / f"mode-idle-{time.time_ns()}"
        out.mkdir(parents=True)
        def save(name, data):
            (out / name).write_text(json.dumps(data, indent=2))
        save("before.json", self.request("/api/status"))
        action_ms = time.time_ns() // 1_000_000
        save("request.json", {"mode": mode, "action_at_ms": action_ms})
        response = self.request("/api/mode", {"mode": mode})
        save("response.json", response)
        require(response.get("status") == "ok" and response.get("mode") == mode, "mode request was not accepted")
        time.sleep(6)
        after = self.request("/api/status")
        save("after.json", after)
        shutil.copyfile(self.run / "ftw.log", out / "ftw.log")
        idle(after, action_ms, time.time_ns() // 1_000_000)
        print(f"idle confirmed for both batteries; evidence in {out}")

    def cleanup(self):
        if not self.run.exists():
            print(f"nothing to clean in {self.run}")
            return
        data = self.manifest()
        # Check every live PID before signalling any; never trust a reused PID.
        alive = []
        for process in data["processes"]:
            try:
                current = identity(process["pid"])
            except ValueError:
                continue
            require(current == process["identity"], f"refusing to stop reused pid {process['pid']}")
            alive.append(process)
        for process in alive:
            os.kill(process["pid"], signal.SIGTERM)
        time.sleep(2)
        for process in alive:
            try:
                current = identity(process["pid"])
            except ValueError:
                continue
            require(current == process["identity"], f"refusing to kill reused pid {process['pid']}")
            os.kill(process["pid"], signal.SIGKILL)
        shutil.rmtree(self.run)
        print(f"stopped; evidence kept in {self.evidence}")


if __name__ == "__main__":
    try:
        instance = Instance()
        command = sys.argv[1]
        if command == "drive-mode":
            instance.drive(sys.argv[2] if len(sys.argv) > 2 else "idle")
        else:
            {"launch": instance.launch, "doctor": instance.doctor, "cleanup": instance.cleanup}[command]()
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print(f"verify-ftw: {error}", file=sys.stderr)
        sys.exit(1)
