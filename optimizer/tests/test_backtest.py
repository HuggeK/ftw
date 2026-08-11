from __future__ import annotations

import copy
import csv
import json

import pytest

import ftw_optimizer.backtest as backtest
from ftw_optimizer.backtest import (
    SnapshotSkip,
    dp_evaluation_reference,
    first_action_counterfactual,
    request_from_diagnostic,
    run_backtest,
    select_causal_first_steps,
    select_summaries,
)


def diagnostic() -> dict:
    return {
        "computed_at_ms": 1000,
        "total_cost_ore": 12.5,
        "params": {
            "mode": "passive_arbitrage",
            "initial_soc_pct": 50,
            "soc_min_pct": 10,
            "soc_max_pct": 95,
            "capacity_wh": 10000,
            "max_charge_w": 5000,
            "max_discharge_w": 5000,
            "charge_efficiency": 0.95,
            "discharge_efficiency": 0.95,
            "terminal_soc_price_ore_kwh": 100,
        },
        "slots": [
            {
                "slot_start_ms": 1000,
                "len_min": 15,
                "price_ore": 100,
                "spot_ore": 50,
                "confidence": 1,
                "pv_w": -500,
                "load_w": 1000,
            }
        ],
    }


def test_select_summaries_preserves_rare_reasons() -> None:
    rows = [{"ts_ms": i, "reason": "scheduled"} for i in range(1, 101)]
    rows.append({"ts_ms": 101, "reason": "missing_plan_retry"})
    selected = select_summaries(rows, 10)
    assert len(selected) == 10
    assert any(row["reason"] == "missing_plan_retry" for row in selected)


def test_request_from_diagnostic_reconstructs_storage_and_limits() -> None:
    request = request_from_diagnostic(
        diagnostic(),
        solver="HIGHS",
        formulation="auto",
        time_limit_s=5,
        max_import_w=11040,
        max_export_w=11040,
        min_arbitrage_spread_ore_kwh=30,
    )
    assert request["storages"][0]["initial_energy_wh"] == 5000
    assert request["slots"][0]["max_import_w"] == 11040
    assert request["settings"]["min_arbitrage_spread_ore_kwh"] == 30


def test_request_from_diagnostic_skips_legacy_loadpoint_without_contract() -> None:
    value = diagnostic()
    value["loadpoint_id"] = "easee"
    with pytest.raises(SnapshotSkip, match="loadpoint contract"):
        request_from_diagnostic(
            value,
            solver="HIGHS",
            formulation="auto",
            time_limit_s=5,
            max_import_w=0,
            max_export_w=0,
            min_arbitrage_spread_ore_kwh=0,
        )


def test_first_action_counterfactual_reprices_both_actions_against_actual_base() -> None:
    value = diagnostic()
    value["slots"][0]["battery_w"] = -200
    response = {"plan": {"actions": [{"battery_w": -400}]}}
    realized = {
        1000: {
            "bucket_end_ms": 901000,
            "pv_w": -500,
            "ev_w": 0,
            "v2x_w": 0,
            "house_load_w": 1000,
            "total_ore_kwh": 100,
            "spot_ore_kwh": 50,
        }
    }
    result = first_action_counterfactual(value, response, realized, 11040, 11040)
    assert result is not None
    assert result["eligible"]
    assert result["reference_grid_w"] == 300
    assert result["candidate_grid_w"] == 100
    assert result["grid_cost_delta_ore"] == pytest.approx(-5)
    assert result["metric_scope"] == "grid_boundary_energy_only"
    assert not result["mode_violation"]


def test_first_action_counterfactual_excludes_full_pv_curtailment_sentinel() -> None:
    value = diagnostic()
    value["slots"][0].update(
        {"pv_w": -5000, "load_w": 0, "battery_w": 0, "grid_w": -5000}
    )
    response = {
        "plan": {
            "actions": [
                {"battery_w": 0, "grid_w": 0, "pv_limit_w": 0}
            ]
        }
    }
    realized = {
        1000: {
            "bucket_end_ms": 901000,
            "pv_w": -5000,
            "ev_w": 0,
            "v2x_w": 0,
            "house_load_w": 0,
            "total_ore_kwh": 100,
            "spot_ore_kwh": -50,
        }
    }
    result = first_action_counterfactual(value, response, realized, 11040, 11040)
    assert result is not None
    assert not result["eligible"]
    assert result["excluded_reason"] == "PV curtailment is not modeled in counterfactual replay"
    assert result["candidate_forecast_balance_residual_w"] == 5000


def test_first_action_counterfactual_marks_nonfinite_realized_interval() -> None:
    value = diagnostic()
    response = {"plan": {"actions": [{"battery_w": 0}]}}
    realized = {
        1000: {
            "bucket_end_ms": 901000,
            "pv_w": float("nan"),
            "ev_w": 0,
            "v2x_w": 0,
            "house_load_w": 1000,
            "total_ore_kwh": 100,
            "spot_ore_kwh": 50,
        }
    }
    result = first_action_counterfactual(value, response, realized, 11040, 11040)
    assert result is not None
    assert not result["eligible"]
    assert result["excluded_reason"] == "non-finite realized interval"


def test_select_causal_first_steps_uses_latest_pre_cutoff_decision_once() -> None:
    def record(start_ms: int, decision_ms: int) -> dict:
        value = copy.deepcopy(diagnostic())
        value["computed_at_ms"] = decision_ms
        value["slots"][0]["slot_start_ms"] = start_ms
        return {
            "summary": {"ts_ms": decision_ms, "reason": "scheduled"},
            "diagnostic": value,
        }

    realized = {
        1000: {"bucket_end_ms": 901000},
        901000: {"bucket_end_ms": 1_801_000},
    }
    selected, exclusions = select_causal_first_steps(
        [
            record(1000, 900),
            record(1000, 1000),
            record(1000, 1001),
            record(901000, 901000),
        ],
        realized,
    )
    assert [row["summary"]["ts_ms"] for row in selected] == [1000, 901000]
    assert exclusions["diagnostic after decision cutoff"] == 1
    assert exclusions["superseded before decision cutoff"] == 1


def test_select_causal_first_steps_rejects_overlapping_realized_intervals() -> None:
    first = {"summary": {"ts_ms": 1000}, "diagnostic": diagnostic()}
    second_diagnostic = copy.deepcopy(diagnostic())
    second_diagnostic["computed_at_ms"] = 500000
    second_diagnostic["slots"][0]["slot_start_ms"] = 500000
    second = {"summary": {"ts_ms": 500000}, "diagnostic": second_diagnostic}
    realized = {
        1000: {"bucket_end_ms": 901000},
        500000: {"bucket_end_ms": 1_400_000},
    }
    selected, exclusions = select_causal_first_steps([first, second], realized)
    assert selected == [first]
    assert exclusions["overlapping realized interval"] == 1


def test_run_backtest_reports_non_additive_horizons_and_counterfactual_name(
    tmp_path, monkeypatch: pytest.MonkeyPatch
) -> None:
    dataset = tmp_path / "dataset.jsonl"
    output = tmp_path / "report.json"
    realized_csv = tmp_path / "realized.csv"
    record = {
        "type": "snapshot",
        "summary": {"ts_ms": 1000, "reason": "scheduled"},
        "diagnostic": diagnostic(),
    }
    dataset.write_text(
        json.dumps({"type": "metadata", "schema_version": 1})
        + "\n"
        + json.dumps(record)
        + "\n",
        encoding="utf-8",
    )
    with realized_csv.open("w", encoding="utf-8", newline="") as target:
        writer = csv.DictWriter(
            target,
            fieldnames=[
                "bucket_start_ms",
                "bucket_end_ms",
                "pv_w",
                "ev_w",
                "v2x_w",
                "house_load_w",
                "total_ore_kwh",
                "spot_ore_kwh",
            ],
        )
        writer.writeheader()
        writer.writerow(
            {
                "bucket_start_ms": 1000,
                "bucket_end_ms": 901000,
                "pv_w": -500,
                "ev_w": 0,
                "v2x_w": 0,
                "house_load_w": 1000,
                "total_ore_kwh": 100,
                "spot_ore_kwh": 50,
            }
        )

    monkeypatch.setattr(
        backtest,
        "handle",
        lambda _request: {
            "ok": True,
            "plan": {
                "total_cost_ore": 10,
                "actions": [{"battery_w": -400, "grid_w": 100, "pv_limit_w": 0}],
            },
            "solver": {
                "solve_ms": 2,
                "status": "optimal",
                "formulation": "convex",
                "service_slack": 0,
            },
        },
    )
    report = run_backtest(
        dataset,
        output,
        solver="HIGHS",
        formulation="auto",
        time_limit_s=5,
        max_import_w=11040,
        max_export_w=11040,
        min_arbitrage_spread_ore_kwh=0,
        limit=0,
        realized_csv=realized_csv,
    )
    assert report["schema_version"] == 2
    horizon = report["summary"]["horizon_objective_diagnostics"]
    assert horizon["additive"] is False
    assert "delta_sum" not in horizon
    counterfactual = report["summary"]["first_action_counterfactual"]
    assert counterfactual["scored_intervals"] == 1
    assert counterfactual["metric_scope"] == "grid_boundary_energy_only"
    assert "grid_cost_delta_ore" in counterfactual
    assert "realized_first_slot" not in report["summary"]
    assert "first_action_counterfactual" in report["results"][0]


def test_dp_evaluation_reference_prefers_same_input_shadow() -> None:
    value = diagnostic()
    value["solver"] = {"engine": "cvxpy"}
    value["optimizer_input"] = {"schema_version": 1}
    value["slots"][0]["battery_w"] = -900
    value["dp_evaluation_shadow"] = {
        "total_cost_ore": 8.25,
        "first_action": {"battery_w": -200},
    }
    cost, action = dp_evaluation_reference(value)
    assert cost == 8.25
    assert action["battery_w"] == -200


def test_dp_evaluation_reference_rejects_active_plan_without_shadow() -> None:
    value = diagnostic()
    value["solver"] = {"engine": "cvxpy"}
    value["optimizer_input"] = {"schema_version": 1}
    with pytest.raises(SnapshotSkip, match="same-input DP"):
        dp_evaluation_reference(value)
