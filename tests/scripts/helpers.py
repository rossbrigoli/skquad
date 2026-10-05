"""Shared helpers for loading the hyphenated CI scripts as importable modules.

The helpers in ``scripts/`` are standalone CLI files with hyphens in their
names, so they cannot be imported with a plain ``import``. We load them via
``importlib`` which also lets coverage.py instrument them normally.
"""

import importlib.util
from pathlib import Path

SCRIPTS_DIR = Path(__file__).resolve().parents[2] / "scripts"


def load_script(module_name: str, filename: str):
    """Import ``scripts/<filename>`` under a pythonic module name."""
    path = SCRIPTS_DIR / filename
    spec = importlib.util.spec_from_file_location(module_name, path)
    assert spec and spec.loader, f"cannot load {path}"
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class FakeResponse:
    """Minimal stand-in for urllib's HTTPResponse context manager."""

    def __init__(self, payload):
        self._body = payload if isinstance(payload, bytes) else __import__("json").dumps(payload).encode()

    def read(self):
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False
