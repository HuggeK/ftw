from __future__ import annotations

import cvxpy as cp
import highspy
import pytest

from ftw_optimizer import shared_highs
from ftw_optimizer.deadline import SolveDeadline, SolveDeadlineExceeded
from ftw_optimizer.direct_highs import (
    DirectHighsError,
    _remaining_time_s,
    _run_optimal,
)
from ftw_optimizer.model import _solver_options, solve


class FakeClock:
    def __init__(self, now: float = 0.0) -> None:
        self.now = now

    def __call__(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += seconds


def test_one_deadline_shrinks_across_cvxpy_and_direct_highs_phases() -> None:
    clock = FakeClock(10.0)
    deadline = SolveDeadline.from_payload(
        {"settings": {"time_limit_s": 1.0}},
        started_at=clock(),
        clock=clock,
    )
    settings = {"time_limit_s": 1.0}

    assert _solver_options(settings, cp.HIGHS, deadline)["time_limit"] == pytest.approx(1.0)
    clock.advance(0.6)
    assert _solver_options(settings, cp.CLARABEL, deadline)["time_limit"] == pytest.approx(0.4)
    assert _remaining_time_s(deadline) == pytest.approx(0.4)

    # The old per-solve 50 ms floor must not extend the request deadline.
    clock.advance(0.39)
    assert _solver_options(settings, cp.HIGHS, deadline)["time_limit"] == pytest.approx(0.01)
    clock.advance(0.02)
    with pytest.raises(SolveDeadlineExceeded, match="deadline exceeded"):
        _solver_options(settings, cp.HIGHS, deadline)
    with pytest.raises(SolveDeadlineExceeded, match="deadline exceeded"):
        _remaining_time_s(deadline)


def test_deadline_error_bypasses_shared_backend_fallback(monkeypatch) -> None:
    deadline = SolveDeadline(1.0, FakeClock())
    direct_calls: list[SolveDeadline] = []

    def fail_direct(
        _payload: dict,
        _started: float,
        received_deadline: SolveDeadline,
    ) -> dict:
        direct_calls.append(received_deadline)
        raise SolveDeadlineExceeded("direct HiGHS solve deadline exceeded")

    monkeypatch.setattr(shared_highs, "solve_shared_highs", fail_direct)

    with pytest.raises(SolveDeadlineExceeded, match="deadline exceeded"):
        solve(
            {
                "settings": {
                    "shared_backend": "auto",
                    "time_limit_s": 10.0,
                },
                "commercial_constraints": {},
                "slots": [{}],
                "storages": [],
            },
            deadline=deadline,
        )

    assert direct_calls == [deadline]


class FakeHighs:
    def __init__(
        self,
        status: highspy.HighsModelStatus,
        run_status: highspy.HighsStatus = highspy.HighsStatus.kOk,
    ) -> None:
        self.status = status
        self.run_status = run_status

    def run(self) -> highspy.HighsStatus:
        return self.run_status

    def getModelStatus(self) -> highspy.HighsModelStatus:
        return self.status


def test_direct_highs_time_limit_is_a_deadline_not_a_fallback_error() -> None:
    deadline = SolveDeadline(1.0, FakeClock())

    with pytest.raises(SolveDeadlineExceeded, match="service solve deadline exceeded"):
        _run_optimal(
            FakeHighs(
                highspy.HighsModelStatus.kTimeLimit,
                highspy.HighsStatus.kWarning,
            ),
            "service",
            deadline,
        )

    with pytest.raises(DirectHighsError, match="failed with status"):
        _run_optimal(
            FakeHighs(highspy.HighsModelStatus.kInfeasible),
            "service",
            deadline,
        )
