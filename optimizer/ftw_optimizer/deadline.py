from __future__ import annotations

import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from .protocol import positive_number, require_dict


class SolveDeadlineExceeded(RuntimeError):
    """The request's one worker-side time budget has been spent."""


@dataclass(frozen=True)
class SolveDeadline:
    expires_at: float
    clock: Callable[[], float] = field(
        default=time.perf_counter,
        repr=False,
        compare=False,
    )

    @classmethod
    def from_payload(
        cls,
        payload: dict[str, Any],
        *,
        started_at: float | None = None,
        clock: Callable[[], float] = time.perf_counter,
    ) -> SolveDeadline:
        settings = require_dict(payload.get("settings", {}), "settings")
        budget_s = positive_number(
            settings.get("time_limit_s", 2.0),
            "settings.time_limit_s",
        )
        if started_at is None:
            started_at = clock()
        return cls(started_at + budget_s, clock)

    def remaining_s(self, phase: str = "optimizer request") -> float:
        remaining = self.expires_at - self.clock()
        if remaining <= 0.0:
            raise SolveDeadlineExceeded(f"{phase} deadline exceeded")
        return remaining

    def check(self, phase: str = "optimizer request") -> None:
        self.remaining_s(phase)
