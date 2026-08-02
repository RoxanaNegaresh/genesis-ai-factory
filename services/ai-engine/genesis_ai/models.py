"""Model registry and download manager.

Why this exists: llama.cpp serves a model, but it does not decide *which* model
a machine can actually run, nor fetch it. Asking a user to pick a quantisation
and a filename from a Hugging Face repository is the single biggest barrier to
"local-first" being true in practice. This module turns that into one command.

The selection logic is deliberately conservative. Over-promising on hardware
produces swapping, ten-minute generations and a user who concludes local models
do not work; under-promising produces a slightly weaker model that responds in
seconds. The second failure is far cheaper.
"""

from __future__ import annotations

import dataclasses
import json
import os
import shutil
import subprocess
import sys
import urllib.request
from pathlib import Path
from typing import Iterable


@dataclasses.dataclass(frozen=True)
class Model:
    """A downloadable GGUF model."""

    key: str
    name: str
    repo: str
    filename: str
    #: Approximate resident size in mebibytes once loaded, including the KV
    #: cache at the default context length.
    footprint_mb: int
    #: Capability tiers this model is suitable for.
    classes: tuple[str, ...]
    context: int
    notes: str = ""

    @property
    def url(self) -> str:
        return f"https://huggingface.co/{self.repo}/resolve/main/{self.filename}"


# The catalogue is intentionally small and opinionated. A long list of
# near-identical quantisations is a decision the user should not have to make;
# these are the sizes that matter, spanning laptop to workstation.
CATALOGUE: tuple[Model, ...] = (
    Model(
        key="qwen2.5-0.5b",
        name="Qwen2.5 0.5B Instruct",
        repo="Qwen/Qwen2.5-0.5B-Instruct-GGUF",
        filename="qwen2.5-0.5b-instruct-q4_k_m.gguf",
        footprint_mb=900,
        classes=("fast",),
        context=32768,
        notes="Smoke-testing and classification only. Too small for design work.",
    ),
    Model(
        key="qwen2.5-3b",
        name="Qwen2.5 3B Instruct",
        repo="Qwen/Qwen2.5-3B-Instruct-GGUF",
        filename="qwen2.5-3b-instruct-q4_k_m.gguf",
        footprint_mb=3200,
        classes=("fast", "reasoning"),
        context=32768,
        notes="Lowest size that produces usable product reasoning.",
    ),
    Model(
        key="qwen2.5-coder-7b",
        name="Qwen2.5 Coder 7B Instruct",
        repo="Qwen/Qwen2.5-Coder-7B-Instruct-GGUF",
        filename="qwen2.5-coder-7b-instruct-q4_k_m.gguf",
        footprint_mb=6500,
        classes=("code", "reasoning"),
        context=32768,
        notes="Recommended default. Strong at both code and structured reasoning.",
    ),
    Model(
        key="qwen2.5-14b",
        name="Qwen2.5 14B Instruct",
        repo="Qwen/Qwen2.5-14B-Instruct-GGUF",
        filename="qwen2.5-14b-instruct-q4_k_m.gguf",
        footprint_mb=11500,
        classes=("reasoning", "code"),
        context=32768,
        notes="Noticeably better architecture and product judgement.",
    ),
    Model(
        key="qwen2.5-coder-32b",
        name="Qwen2.5 Coder 32B Instruct",
        repo="Qwen/Qwen2.5-Coder-32B-Instruct-GGUF",
        filename="qwen2.5-coder-32b-instruct-q4_k_m.gguf",
        footprint_mb=22000,
        classes=("code", "reasoning"),
        context=32768,
        notes="Best local quality. Needs a workstation or a GPU.",
    ),
)


def catalogue() -> tuple[Model, ...]:
    return CATALOGUE


def find(key: str) -> Model | None:
    for model in CATALOGUE:
        if model.key == key:
            return model
    return None


def available_memory_mb() -> int:
    """Return usable RAM in mebibytes.

    ``MemAvailable`` is used rather than ``MemTotal`` because what matters is
    what this process can actually get without forcing the kernel to swap.
    """
    try:
        with open("/proc/meminfo", encoding="utf-8") as handle:
            for line in handle:
                if line.startswith("MemAvailable:"):
                    return int(line.split()[1]) // 1024
    except (OSError, ValueError, IndexError):
        pass

    # Fall back to total physical memory on platforms without /proc.
    try:
        return (os.sysconf("SC_PAGE_SIZE") * os.sysconf("SC_PHYS_PAGES")) // (1024 * 1024)
    except (AttributeError, ValueError, OSError):
        return 4096


def recommend(memory_mb: int | None = None) -> Model:
    """Pick the largest model that fits comfortably in memory.

    A 30% headroom margin is applied: the footprint figures exclude the
    operating system, the control plane, a browser and the generated project's
    own toolchain, all of which are running at the same time on a real machine.
    """
    budget = (memory_mb if memory_mb is not None else available_memory_mb()) * 0.7

    best = CATALOGUE[0]
    for model in CATALOGUE:
        if model.footprint_mb <= budget:
            best = model
    return best


def model_dir() -> Path:
    configured = os.environ.get("GENESIS_MODEL_DIR")
    if configured:
        return Path(configured)
    return Path(os.environ.get("XDG_DATA_HOME", Path.home() / ".local" / "share")) / "genesis" / "models"


def local_path(model: Model) -> Path:
    return model_dir() / model.filename


def is_downloaded(model: Model) -> bool:
    path = local_path(model)
    # A partial download is worse than none: it fails at load time with a
    # confusing error, so size is checked rather than mere existence.
    return path.exists() and path.stat().st_size > 1_000_000


def download(model: Model, *, progress: bool = True) -> Path:
    """Fetch a model, resuming and verifying size."""
    destination = local_path(model)
    destination.parent.mkdir(parents=True, exist_ok=True)

    if is_downloaded(model):
        return destination

    # Download to a temporary name and rename on success, so an interrupted
    # transfer never leaves a file that looks complete.
    partial = destination.with_suffix(destination.suffix + ".partial")

    request = urllib.request.Request(model.url, headers={"User-Agent": "genesis-ai-factory"})
    with urllib.request.urlopen(request) as response, open(partial, "wb") as out:
        total = int(response.headers.get("Content-Length", 0))
        written = 0
        chunk_size = 1 << 20

        while True:
            chunk = response.read(chunk_size)
            if not chunk:
                break
            out.write(chunk)
            written += len(chunk)

            if progress and total:
                percent = written * 100 // total
                mib = written // (1024 * 1024)
                total_mib = total // (1024 * 1024)
                print(f"\r  {model.key}: {percent:3d}%  {mib}/{total_mib} MiB", end="", flush=True)

    if progress:
        print()

    partial.rename(destination)
    return destination


def find_llama_server() -> str | None:
    """Locate a llama.cpp server binary."""
    for candidate in ("llama-server", "llama-server.exe"):
        found = shutil.which(candidate)
        if found:
            return found

    # Also look beside the model directory, where `genesis-ai install` puts it.
    bundled = model_dir().parent / "bin" / "llama-server"
    if bundled.exists():
        return str(bundled)
    return None


def serve_command(model: Model, *, port: int = 8791, threads: int | None = None, context: int = 8192) -> list[str]:
    """Build the llama.cpp command line for a model.

    The context length is capped rather than taken from the model's maximum:
    a 32k KV cache costs gigabytes of RAM that a laptop does not have, and the
    factory's prompts are budgeted well below that.
    """
    binary = find_llama_server()
    if binary is None:
        raise RuntimeError(
            "llama-server was not found. Install llama.cpp and put llama-server on PATH:\n"
            "  https://github.com/ggml-org/llama.cpp/releases"
        )

    if threads is None:
        threads = max(1, (os.cpu_count() or 2))

    return [
        binary,
        "-m", str(local_path(model)),
        "--host", "127.0.0.1",
        "--port", str(port),
        "-c", str(min(context, model.context)),
        "-t", str(threads),
        "--no-webui",
    ]


def describe(model: Model, *, memory_mb: int | None = None) -> dict[str, object]:
    budget = memory_mb if memory_mb is not None else available_memory_mb()
    return {
        "key": model.key,
        "name": model.name,
        "classes": list(model.classes),
        "context": model.context,
        "footprint_mb": model.footprint_mb,
        "downloaded": is_downloaded(model),
        "fits": model.footprint_mb <= budget * 0.7,
        "path": str(local_path(model)),
        "notes": model.notes,
    }
