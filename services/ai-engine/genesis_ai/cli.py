"""genesis-ai — model management for the Genesis AI Factory.

This is a deliberately small tool. Its whole purpose is to remove the four
manual steps between "I installed Genesis" and "my agents can reason": choose a
model that fits, fetch it, start a server, and tell the control plane where it
is. Anything beyond that belongs in the control plane, not here.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys

from . import models


def _print_table(rows: list[list[str]], headers: list[str]) -> None:
    widths = [len(h) for h in headers]
    for row in rows:
        for i, cell in enumerate(row):
            widths[i] = max(widths[i], len(cell))

    print("  " + "  ".join(h.upper().ljust(widths[i]) for i, h in enumerate(headers)))
    for row in rows:
        print("  " + "  ".join(cell.ljust(widths[i]) for i, cell in enumerate(row)))


def cmd_list(args: argparse.Namespace) -> int:
    memory = models.available_memory_mb()
    recommended = models.recommend(memory)

    if args.json:
        print(json.dumps({
            "available_memory_mb": memory,
            "recommended": recommended.key,
            "models": [models.describe(m, memory_mb=memory) for m in models.catalogue()],
        }, indent=2))
        return 0

    print(f"\n  Available memory: {memory} MiB")
    print(f"  Recommended:      {recommended.key} ({recommended.name})\n")

    rows = []
    for model in models.catalogue():
        info = models.describe(model, memory_mb=memory)
        status = "installed" if info["downloaded"] else ("fits" if info["fits"] else "too large")
        marker = "*" if model.key == recommended.key else " "
        rows.append([
            marker + model.key,
            model.name,
            f"{model.footprint_mb} MiB",
            ",".join(model.classes),
            status,
        ])
    _print_table(rows, ["key", "name", "memory", "classes", "status"])
    print("\n  Install with:  genesis-ai pull <key>")
    print("  Then run:      genesis-ai serve\n")
    return 0


def cmd_pull(args: argparse.Namespace) -> int:
    key = args.model or models.recommend().key
    model = models.find(key)
    if model is None:
        print(f"unknown model {key!r}. Run 'genesis-ai list' to see the catalogue.", file=sys.stderr)
        return 1

    memory = models.available_memory_mb()
    if model.footprint_mb > memory * 0.7 and not args.force:
        # Refusing loudly is better than letting the machine swap for an hour.
        print(
            f"{model.key} needs about {model.footprint_mb} MiB but only {memory} MiB is available.\n"
            f"Pick a smaller model, or pass --force if you know the memory will be free.",
            file=sys.stderr,
        )
        return 1

    if models.is_downloaded(model):
        print(f"  {model.key} is already installed at {models.local_path(model)}")
        return 0

    print(f"  Downloading {model.name} ({model.footprint_mb} MiB)…")
    try:
        path = models.download(model)
    except Exception as err:  # noqa: BLE001 — surface any transport failure plainly
        print(f"download failed: {err}", file=sys.stderr)
        return 1

    print(f"  Installed at {path}")
    print(f"\n  Start it with: genesis-ai serve --model {model.key}")
    return 0


def cmd_serve(args: argparse.Namespace) -> int:
    key = args.model
    if key is None:
        # Prefer something already on disk over re-downloading.
        installed = [m for m in models.catalogue() if models.is_downloaded(m)]
        if not installed:
            print("no model is installed. Run 'genesis-ai pull' first.", file=sys.stderr)
            return 1
        key = max(installed, key=lambda m: m.footprint_mb).key

    model = models.find(key)
    if model is None:
        print(f"unknown model {key!r}", file=sys.stderr)
        return 1
    if not models.is_downloaded(model):
        print(f"{model.key} is not installed. Run: genesis-ai pull {model.key}", file=sys.stderr)
        return 1

    try:
        command = models.serve_command(model, port=args.port, threads=args.threads, context=args.context)
    except RuntimeError as err:
        print(str(err), file=sys.stderr)
        return 1

    print(f"  Serving {model.name} on http://127.0.0.1:{args.port}")
    print(f"  Point the control plane at it:\n    export GENESIS_LLM_URL=http://127.0.0.1:{args.port}\n")

    try:
        return subprocess.call(command)
    except KeyboardInterrupt:
        return 0


def cmd_doctor(args: argparse.Namespace) -> int:
    memory = models.available_memory_mb()
    binary = models.find_llama_server()
    installed = [m for m in models.catalogue() if models.is_downloaded(m)]

    print()
    ok = True

    if binary:
        print(f"  ✔ llama-server found at {binary}")
    else:
        print("  ✘ llama-server not found on PATH")
        print("    Install from https://github.com/ggml-org/llama.cpp/releases")
        ok = False

    print(f"  ✔ {memory} MiB memory available")
    print(f"    recommended model: {models.recommend(memory).key}")

    if installed:
        print(f"  ✔ {len(installed)} model(s) installed: {', '.join(m.key for m in installed)}")
    else:
        print("  ✘ no models installed — run: genesis-ai pull")
        ok = False

    print(f"\n  Model directory: {models.model_dir()}")
    print()
    return 0 if ok else 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="genesis-ai",
        description="Model management for the Genesis AI Factory",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    p_list = sub.add_parser("list", help="show the model catalogue and what fits")
    p_list.add_argument("--json", action="store_true", help="machine-readable output")
    p_list.set_defaults(func=cmd_list)

    p_pull = sub.add_parser("pull", help="download a model")
    p_pull.add_argument("model", nargs="?", help="model key (default: the recommended one)")
    p_pull.add_argument("--force", action="store_true", help="download even if it may not fit")
    p_pull.set_defaults(func=cmd_pull)

    p_serve = sub.add_parser("serve", help="run an inference server")
    p_serve.add_argument("--model", help="model key (default: the largest installed)")
    p_serve.add_argument("--port", type=int, default=8791)
    p_serve.add_argument("--threads", type=int, default=None)
    p_serve.add_argument("--context", type=int, default=8192)
    p_serve.set_defaults(func=cmd_serve)

    p_doctor = sub.add_parser("doctor", help="check the local inference setup")
    p_doctor.set_defaults(func=cmd_doctor)

    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    raise SystemExit(main())
