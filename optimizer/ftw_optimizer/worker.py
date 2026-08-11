from __future__ import annotations

import argparse
import ctypes
import gc
import importlib.metadata
import json
import os
import socket
import sys
import threading
import time
import traceback
from collections.abc import Callable
from pathlib import Path
from typing import Any

import cvxpy as cp

from .deadline import SolveDeadline, SolveDeadlineExceeded
from .model import solve
from .protocol import ParsedRequest, ProtocolError, error_response, parse_request


# PROTOCOL_VERSION is what this worker speaks; MIN_PROTOCOL_VERSION is the
# oldest Core it still works with. Core accepts any overlap with its own window,
# so widening this range — rather than moving it — keeps an updated optimizer
# usable by a Core that has not been updated yet.
#
# Prefer adding to FEATURES over bumping the protocol. A feature an old Core
# does not know about costs it nothing; a protocol bump makes every Core outside
# the window stop using this optimizer at once.
MIN_PROTOCOL_VERSION = 1
PROTOCOL_VERSION = 1
FEATURES = ["champion", "recourse", "multistage", "commercial_constraints_v1"]
SOLVE_LOCK = threading.Lock()


def release_unused_memory() -> None:
    """Return solver heap pages when the platform allocator supports it."""
    gc.collect()
    try:
        malloc_trim = ctypes.CDLL(None).malloc_trim
    except (AttributeError, OSError):
        return
    malloc_trim.argtypes = [ctypes.c_size_t]
    malloc_trim.restype = ctypes.c_int
    malloc_trim(0)


def handle(
    raw: Any,
    *,
    received_at: float | None = None,
    clock: Callable[[], float] = time.perf_counter,
    parsed: ParsedRequest | None = None,
    deadline: SolveDeadline | None = None,
) -> dict[str, Any]:
    if received_at is None:
        received_at = clock()
    request_id = "unknown"
    try:
        if parsed is None:
            parsed = parse_request(raw)
        request_id = parsed.request_id
        if deadline is None:
            deadline = SolveDeadline.from_payload(
                parsed.payload,
                started_at=received_at,
                clock=clock,
            )
        deadline.check("optimizer queue")
        response = solve(parsed.payload, deadline=deadline)
        deadline.check("optimizer response")
        return response
    except ProtocolError as exc:
        return error_response(request_id, "invalid_request", str(exc))
    except SolveDeadlineExceeded as exc:
        return error_response(request_id, "deadline_exceeded", str(exc))
    except cp.error.SolverError as exc:
        return error_response(request_id, "solver_error", str(exc))
    except Exception as exc:  # worker boundary: one bad request must not kill the process
        traceback.print_exc(file=sys.stderr)
        return error_response(request_id, "internal_error", str(exc))


def handshake(raw: Any) -> dict[str, Any] | None:
    if not isinstance(raw, dict) or raw.get("type") != "handshake":
        return None
    try:
        version = importlib.metadata.version("ftw-optimizer")
    except importlib.metadata.PackageNotFoundError:
        version = "dev"
    return {
        "name": "ftw-optimizer",
        "version": os.environ.get("FTW_OPTIMIZER_VERSION", version),
        "protocol_version": PROTOCOL_VERSION,
        "protocol_min": MIN_PROTOCOL_VERSION,
        "protocol_max": PROTOCOL_VERSION,
        "features": FEATURES,
        "build_sha": os.environ.get("FTW_OPTIMIZER_BUILD_SHA", ""),
    }


def process_stream(
    reader: Any,
    writer: Any,
    *,
    clock: Callable[[], float] = time.perf_counter,
) -> None:
    for line in reader:
        if not line.strip():
            continue
        received_at = clock()
        try:
            raw = json.loads(line)
        except json.JSONDecodeError as exc:
            response = error_response("unknown", "invalid_json", str(exc))
        else:
            response = handshake(raw)
            if response is None:
                # Handshakes stay responsive while a solve is in progress,
                # but solver state and its memory cleanup remain serialized.
                request_id = "unknown"
                try:
                    parsed = parse_request(raw)
                    request_id = parsed.request_id
                    deadline = SolveDeadline.from_payload(
                        parsed.payload,
                        started_at=received_at,
                        clock=clock,
                    )
                    wait_s = min(
                        deadline.remaining_s("optimizer queue"),
                        threading.TIMEOUT_MAX,
                    )
                except ProtocolError as exc:
                    response = error_response(request_id, "invalid_request", str(exc))
                except SolveDeadlineExceeded as exc:
                    response = error_response(
                        request_id,
                        "deadline_exceeded",
                        str(exc),
                    )
                else:
                    if not SOLVE_LOCK.acquire(timeout=wait_s):
                        response = error_response(
                            parsed.request_id,
                            "deadline_exceeded",
                            "optimizer queue deadline exceeded",
                        )
                        writer.write(
                            json.dumps(
                                response,
                                separators=(",", ":"),
                                allow_nan=False,
                            )
                            + "\n"
                        )
                        writer.flush()
                        continue
                    try:
                        response = handle(
                            raw,
                            received_at=received_at,
                            clock=clock,
                            parsed=parsed,
                            deadline=deadline,
                        )
                        try:
                            writer.write(
                                json.dumps(
                                    response,
                                    separators=(",", ":"),
                                    allow_nan=False,
                                )
                                + "\n"
                            )
                            writer.flush()
                        finally:
                            response = None
                            release_unused_memory()
                    finally:
                        SOLVE_LOCK.release()
                    continue
        writer.write(json.dumps(response, separators=(",", ":"), allow_nan=False) + "\n")
        writer.flush()


def serve_unix(socket_path: str) -> None:
    path = Path(socket_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        path.unlink()
    except FileNotFoundError:
        pass
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
            server.bind(str(path))
            os.chmod(path, 0o660)
            server.listen(16)
            def serve_connection(conn: socket.socket) -> None:
                try:
                    with conn:
                        with conn.makefile("r", encoding="utf-8") as reader:
                            with conn.makefile("w", encoding="utf-8") as writer:
                                process_stream(reader, writer)
                except (BrokenPipeError, ConnectionResetError):
                    # Core timed out/cancelled. The worker stays alive and the
                    # next replan can use it (or core's fallback) normally.
                    return

            while True:
                conn, _ = server.accept()
                threading.Thread(target=serve_connection, args=(conn,), daemon=True).start()
    finally:
        try:
            path.unlink()
        except FileNotFoundError:
            pass


def main() -> None:
    parser = argparse.ArgumentParser(description="FTW mathematical optimizer worker")
    parser.add_argument("--socket", default=os.environ.get("FTW_OPTIMIZER_SOCKET", ""))
    args = parser.parse_args()
    if args.socket:
        serve_unix(args.socket)
        return
    process_stream(sys.stdin, sys.stdout)


if __name__ == "__main__":
    main()
