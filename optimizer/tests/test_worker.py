from __future__ import annotations

import io
import json
import threading

from ftw_optimizer import worker


def test_health_stays_responsive_without_cleaning_memory_during_solve(
    monkeypatch,
) -> None:
    solve_started = threading.Event()
    finish_solve = threading.Event()
    cleanup_calls: list[bool] = []
    thread_errors: list[BaseException] = []
    monkeypatch.setattr(worker, "SOLVE_LOCK", threading.Lock())

    def fake_handle(_raw: object, **_kwargs: object) -> dict[str, object]:
        solve_started.set()
        if not finish_solve.wait(timeout=2):
            raise TimeoutError("test did not release solve")
        return {"ok": True}

    def fake_cleanup() -> None:
        cleanup_calls.append(worker.SOLVE_LOCK.locked())

    monkeypatch.setattr(worker, "handle", fake_handle)
    monkeypatch.setattr(worker, "release_unused_memory", fake_cleanup)

    solve_output = io.StringIO()

    def run_solve() -> None:
        try:
            worker.process_stream(
                io.StringIO(
                    '{"schema_version":1,"request_id":"test","slots":[{}]}\n'
                ),
                solve_output,
            )
        except BaseException as exc:
            thread_errors.append(exc)

    solve_thread = threading.Thread(target=run_solve)
    solve_thread.start()
    assert solve_started.wait(timeout=1)

    health_output = io.StringIO()
    health_thread = threading.Thread(
        target=worker.process_stream,
        args=(
            io.StringIO('{"type":"handshake","protocol_version":1}\n'),
            health_output,
        ),
    )
    health_thread.start()
    health_thread.join(timeout=1)
    assert not health_thread.is_alive()
    assert '"name":"ftw-optimizer"' in health_output.getvalue()
    assert cleanup_calls == []

    finish_solve.set()
    solve_thread.join(timeout=1)
    assert not solve_thread.is_alive()
    assert thread_errors == []
    assert cleanup_calls == [True]


class FakeClock:
    def __init__(self) -> None:
        self.value = 0.0
        self.condition = threading.Condition()

    def __call__(self) -> float:
        with self.condition:
            return self.value

    def advance(self, seconds: float) -> None:
        with self.condition:
            self.value += seconds
            self.condition.notify_all()


class FakeSolveLock:
    def __init__(self, clock: FakeClock) -> None:
        self.clock = clock
        self.held = False
        self.queued = threading.Event()

    def acquire(self, blocking: bool = True, timeout: float = -1) -> bool:
        with self.clock.condition:
            if not self.held:
                self.held = True
                return True
            if not blocking:
                return False
            self.queued.set()
            expires_at = self.clock.value + timeout
            while self.held:
                if timeout >= 0 and self.clock.value >= expires_at:
                    return False
                self.clock.condition.wait()
            self.held = True
            return True

    def release(self) -> None:
        with self.clock.condition:
            self.held = False
            self.clock.condition.notify_all()

    def locked(self) -> bool:
        with self.clock.condition:
            return self.held


def test_expired_request_leaves_solve_queue_without_running(
    monkeypatch,
) -> None:
    clock = FakeClock()
    solve_lock = FakeSolveLock(clock)
    first_started = threading.Event()
    finish_first = threading.Event()
    solve_calls: list[str] = []
    thread_errors: list[BaseException] = []
    monkeypatch.setattr(worker, "SOLVE_LOCK", solve_lock)
    monkeypatch.setattr(worker, "release_unused_memory", lambda: None)

    def fake_solve(payload: dict, **_kwargs: object) -> dict[str, object]:
        request_id = str(payload["request_id"])
        solve_calls.append(request_id)
        if request_id == "first":
            first_started.set()
            if not finish_first.wait(timeout=2):
                raise TimeoutError("test did not release first solve")
        return {"ok": True, "request_id": request_id}

    monkeypatch.setattr(worker, "solve", fake_solve)

    def request(request_id: str, budget_s: float) -> io.StringIO:
        return io.StringIO(
            json.dumps(
                {
                    "schema_version": 1,
                    "request_id": request_id,
                    "settings": {"time_limit_s": budget_s},
                    "slots": [{}],
                }
            )
            + "\n"
        )

    def run(request_id: str, budget_s: float, output: io.StringIO) -> None:
        try:
            worker.process_stream(
                request(request_id, budget_s),
                output,
                clock=clock,
            )
        except BaseException as exc:
            thread_errors.append(exc)

    first_output = io.StringIO()
    first = threading.Thread(target=run, args=("first", 10.0, first_output))
    first.start()
    assert first_started.wait(timeout=1)

    expired_output = io.StringIO()
    expired = threading.Thread(target=run, args=("expired", 1.0, expired_output))
    expired.start()
    assert solve_lock.queued.wait(timeout=1)
    clock.advance(2.0)
    expired.join(timeout=1)
    assert not expired.is_alive()
    assert solve_lock.locked()
    assert json.loads(expired_output.getvalue())["error"]["code"] == "deadline_exceeded"
    assert solve_calls == ["first"]

    finish_first.set()
    first.join(timeout=1)
    assert not first.is_alive()

    fresh_output = io.StringIO()
    fresh = threading.Thread(target=run, args=("fresh", 1.0, fresh_output))
    fresh.start()
    fresh.join(timeout=1)
    assert not fresh.is_alive()
    assert json.loads(fresh_output.getvalue())["ok"] is True
    assert solve_calls == ["first", "fresh"]
    assert thread_errors == []


def test_handle_rejects_a_result_that_finishes_after_its_deadline(
    monkeypatch,
) -> None:
    clock = FakeClock()
    solve_calls = 0

    def fake_solve(_payload: dict, **_kwargs: object) -> dict[str, object]:
        nonlocal solve_calls
        solve_calls += 1
        clock.advance(2.0)
        return {"ok": True}

    monkeypatch.setattr(worker, "solve", fake_solve)
    response = worker.handle(
        {
            "schema_version": 1,
            "request_id": "late",
            "settings": {"time_limit_s": 1.0},
            "slots": [{}],
        },
        clock=clock,
    )

    assert solve_calls == 1
    assert response["error"]["code"] == "deadline_exceeded"
