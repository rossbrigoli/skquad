"""Test-suite defaults for BT-RUNTIME and WP6.

S-147 (WP6): the SKQUAD_PROMPT_FETCH_ENABLED flag and the legacy env
prompt path are removed — the composed-prompt fetch is mandatory. Tests
that don't drive the real fetcher stub it themselves (see
``tests/test_runtime.py``); WP3/WP5 restore the real one.

BT-RUNTIME builtin tools follow the same discipline: the legacy suite
neither runs a control plane with ``/api/v1/agents/me/tools`` nor mocks
that HTTP layer, so the suite defaults
``SKQUAD_BUILTIN_TOOLS_ENABLED=false``. Builtin-tools tests opt in
explicitly and mock the opener. Production default stays enabled.
"""

import os

import pytest


@pytest.fixture(autouse=True)
def _default_builtin_tools_off(monkeypatch):
    # BT-RUNTIME: same pattern as the old prompt flag (see module
    # docstring). Builtin-tools tests opt in explicitly.
    if "SKQUAD_BUILTIN_TOOLS_ENABLED" not in os.environ:
        monkeypatch.setenv("SKQUAD_BUILTIN_TOOLS_ENABLED", "false")
    yield
