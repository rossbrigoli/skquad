"""Test-suite defaults for S-PROMPT WP3.

Pre-WP3 tests exercise the legacy env prompt path (they fake the
control-plane client but not the composed-prompt HTTP layer). WP3 makes
fetch-at-wake the default and loud-failing, so the suite defaults to
``SKQUAD_PROMPT_FETCH_ENABLED=false`` here. The WP3 prompt tests
(``tests/test_prompt_wp3.py``) explicitly enable the flag and mock the
HTTP opener, so they always exercise the composed path.
"""

import os

import pytest


@pytest.fixture(autouse=True)
def _default_legacy_prompt_path(monkeypatch):
    # Only default when the test hasn't chosen a mode itself (WP3 tests
    # set SKQUAD_PROMPT_FETCH_ENABLED=true in their config env).
    if "SKQUAD_PROMPT_FETCH_ENABLED" not in os.environ:
        monkeypatch.setenv("SKQUAD_PROMPT_FETCH_ENABLED", "false")
    yield
