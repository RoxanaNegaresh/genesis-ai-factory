"""Tests for model selection and the download manager.

The selection logic is the part that must not be wrong: recommending a model
that does not fit produces a machine that swaps for ten minutes and a user who
concludes local inference does not work.
"""

import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from genesis_ai import models  # noqa: E402


def test_catalogue_is_well_formed():
    assert models.catalogue(), "catalogue must not be empty"

    keys = set()
    for model in models.catalogue():
        assert model.key not in keys, f"duplicate key {model.key}"
        keys.add(model.key)

        assert model.repo and model.filename
        assert model.filename.endswith(".gguf")
        assert model.footprint_mb > 0
        assert model.context >= 4096
        assert model.classes, f"{model.key} declares no capability class"
        for cls in model.classes:
            assert cls in {"fast", "reasoning", "code"}, f"unknown class {cls}"
        assert model.url.startswith("https://huggingface.co/")


def test_catalogue_is_ordered_by_size():
    # recommend() relies on ascending order to pick the largest that fits.
    sizes = [m.footprint_mb for m in models.catalogue()]
    assert sizes == sorted(sizes), "catalogue must be ordered smallest to largest"


def test_recommend_picks_largest_that_fits_with_headroom():
    # 8 GiB machine: 70% of 8192 is 5734, so the 3B (3200) fits but the 7B
    # (6500) does not.
    chosen = models.recommend(memory_mb=8192)
    assert chosen.key == "qwen2.5-3b", chosen.key

    # A workstation should get the best model available.
    assert models.recommend(memory_mb=64000).key == "qwen2.5-coder-32b"

    # A constrained machine must still get something runnable rather than an
    # error: degrading is the whole point.
    assert models.recommend(memory_mb=1500).key == "qwen2.5-0.5b"


def test_recommend_never_exceeds_budget():
    for memory in (1024, 2048, 4096, 8192, 16384, 32768, 65536):
        chosen = models.recommend(memory_mb=memory)
        smallest = models.catalogue()[0]
        if chosen.key != smallest.key:
            assert chosen.footprint_mb <= memory * 0.7, (
                f"recommended {chosen.key} ({chosen.footprint_mb} MiB) for {memory} MiB"
            )


def test_available_memory_is_plausible():
    memory = models.available_memory_mb()
    assert memory > 0
    assert memory < 10_000_000  # sanity bound, not a real limit


def test_find_returns_none_for_unknown():
    assert models.find("qwen2.5-3b") is not None
    assert models.find("does-not-exist") is None


def test_local_path_respects_environment(tmp_path, monkeypatch):
    monkeypatch.setenv("GENESIS_MODEL_DIR", str(tmp_path))
    model = models.find("qwen2.5-3b")
    assert models.local_path(model).parent == tmp_path


def test_is_downloaded_rejects_partial_files(tmp_path, monkeypatch):
    monkeypatch.setenv("GENESIS_MODEL_DIR", str(tmp_path))
    model = models.find("qwen2.5-3b")

    assert not models.is_downloaded(model)

    # A truncated download must not be mistaken for a usable model; that
    # failure surfaces much later as an unreadable-tensor error.
    path = models.local_path(model)
    path.write_bytes(b"partial")
    assert not models.is_downloaded(model)

    path.write_bytes(b"x" * 2_000_000)
    assert models.is_downloaded(model)


def test_serve_command_caps_context(tmp_path, monkeypatch):
    monkeypatch.setenv("GENESIS_MODEL_DIR", str(tmp_path))
    monkeypatch.setattr(models, "find_llama_server", lambda: "/usr/bin/llama-server")

    model = models.find("qwen2.5-3b")
    command = models.serve_command(model, port=9999, threads=4, context=4096)

    assert "--port" in command and "9999" in command
    assert "-t" in command and "4" in command
    # The requested context must be honoured rather than the model's 32k
    # maximum, which would allocate gigabytes of KV cache.
    context_index = command.index("-c")
    assert command[context_index + 1] == "4096"


def test_serve_command_reports_missing_binary(monkeypatch):
    monkeypatch.setattr(models, "find_llama_server", lambda: None)
    model = models.find("qwen2.5-3b")
    try:
        models.serve_command(model)
    except RuntimeError as err:
        assert "llama-server" in str(err)
        assert "github.com" in str(err), "the error must tell the user where to get it"
    else:
        raise AssertionError("expected a RuntimeError when the binary is missing")


def test_describe_reports_fit(tmp_path, monkeypatch):
    monkeypatch.setenv("GENESIS_MODEL_DIR", str(tmp_path))
    info = models.describe(models.find("qwen2.5-coder-32b"), memory_mb=4096)
    assert info["fits"] is False
    assert info["downloaded"] is False
    assert info["key"] == "qwen2.5-coder-32b"
