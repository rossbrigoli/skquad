"""Test-suite defaults for S-PROMPT WP3 and BT-RUNTIME.

Pre-WP3 tests exercise the legacy env prompt path (they fake the
control-plane client but not the composed-prompt HTTP layer). WP3 makes
fetch-at-wake the default and loud-failing, so the suite defaults to
``SKQUAD_PROMPT_FETCH_ENABLED=false`` here. The WP3 prompt tests
(``tests/test_prompt_wp3.py``) explicitly enable the flag and mock the
HTTP opener, so they always exercise the composed path.

BT-RUNTIME builtin tools follow the same discipline: the legacy suite
neither runs a control plane with ``/api/v1/agents/me/tools`` nor mocks
that HTTP layer, so the suite defaults
``SKQUAD_BUILTIN_TOOLS_ENABLED=false``. Builtin-tools tests opt in
explicitly and mock the opener. Production default stays enabled.
"""

import os

import pytest


@pytest.fixture(autouse=True)
def _default_legacy_prompt_path(monkeypatch):
    # Only default when the test hasn't chosen a mode itself (WP3 tests
    # set SKQUAD_PROMPT_FETCH_ENABLED=true in their config env).
    if "SKQUAD_PROMPT_FETCH_ENABLED" not in os.environ:
        monkeypatch.setenv("SKQUAD_PROMPT_FETCH_ENABLED", "false")
    # BT-RUNTIME: same pattern for builtin tools (see module docstring).
    if "SKQUAD_BUILTIN_TOOLS_ENABLED" not in os.environ:
        monkeypatch.setenv("SKQUAD_BUILTIN_TOOLS_ENABLED", "false")
    yield
