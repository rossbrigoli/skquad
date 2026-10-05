"""Unit tests for scripts/sonar-severity-gate.py (S-189, test-only).

All HTTP is mocked — no real SonarQube/Kanbunny traffic, no sleeps.
"""

import sys
import urllib.error

import pytest

from helpers import FakeResponse, load_script

sg = load_script("sonar_severity_gate", "sonar-severity-gate.py")


@pytest.fixture(autouse=True)
def base_env(monkeypatch):
    """Deterministic env: required vars set, optional behaviours cleared."""
    monkeypatch.setenv("SONAR_URL", "http://sonar.test")
    monkeypatch.setenv("SONAR_TOKEN", "tok")
    monkeypatch.setenv("SONAR_PROJECT", "skquad")
    monkeypatch.setenv("GATE_SETTLE_SECONDS", "0")
    for opt in ("SKIP_KANBUNNY", "GATE_TYPES", "KANBUNNY_URL", "KANBUNNY_TOKEN", "KANBUNNY_BOARD"):
        monkeypatch.delenv(opt, raising=False)


# ---------------------------------------------------------------------------
# env()
# ---------------------------------------------------------------------------

def test_env_returns_stripped_value(monkeypatch):
    monkeypatch.setenv("SOME_VAR", "  hello  ")
    assert sg.env("SOME_VAR") == "hello"


def test_env_missing_required_exits_2(monkeypatch):
    monkeypatch.delenv("ABSENT_VAR", raising=False)
    with pytest.raises(SystemExit) as exc:
        sg.env("ABSENT_VAR")
    assert exc.value.code == 2


def test_env_missing_optional_returns_empty(monkeypatch):
    monkeypatch.delenv("ABSENT_VAR", raising=False)
    assert sg.env("ABSENT_VAR", required=False) == ""


# ---------------------------------------------------------------------------
# http_json()
# ---------------------------------------------------------------------------

def test_http_json_success(monkeypatch):
    captured = {}

    def fake_urlopen(req, timeout=None):
        captured["method"] = req.get_method()
        captured["auth"] = req.get_header("Authorization")
        captured["ctype"] = req.get_header("Content-type")
        captured["body"] = req.data
        return FakeResponse({"ok": True})

    monkeypatch.setattr(sg.urllib.request, "urlopen", fake_urlopen)
    out = sg.http_json("http://x/api", "tok123", method="POST", body={"a": 1})
    assert out == {"ok": True}
    assert captured["method"] == "POST"
    assert captured["auth"] == "Bearer tok123"
    assert captured["ctype"] == "application/json"
    assert b'"a": 1' in captured["body"]


def test_http_json_get_has_no_content_type(monkeypatch):
    captured = {}

    def fake_urlopen(req, timeout=None):
        captured["ctype"] = req.get_header("Content-type")
        return FakeResponse({})

    monkeypatch.setattr(sg.urllib.request, "urlopen", fake_urlopen)
    sg.http_json("http://x/api", "tok")
    assert captured["ctype"] is None


def test_http_json_network_error_exits_2(monkeypatch):
    def boom(req, timeout=None):
        raise urllib.error.URLError("connection refused")

    monkeypatch.setattr(sg.urllib.request, "urlopen", boom)
    with pytest.raises(SystemExit) as exc:
        sg.http_json("http://x/api", "tok")
    assert exc.value.code == 2


# ---------------------------------------------------------------------------
# fetch_findings / count_findings
# ---------------------------------------------------------------------------

def test_fetch_findings_paginates_until_total(monkeypatch):
    pages = {
        "1": ({"issues": [{"key": "i1"}, {"key": "i2"}], "paging": {"total": 3}}),
        "2": ({"issues": [{"key": "i3"}], "paging": {"total": 3}}),
    }
    seen = []

    def fake_http(url, token, method="GET", body=None):
        seen.append(url)
        page = url.split("p=")[1]
        return pages[page]

    monkeypatch.setattr(sg, "http_json", fake_http)
    findings = sg.fetch_findings("http://sonar.test", "tok", "proj", "VULNERABILITY,BUG")
    assert [f["key"] for f in findings] == ["i1", "i2", "i3"]
    assert len(seen) == 2
    assert "severities=CRITICAL%2CBLOCKER" in seen[0]
    assert "resolved=false" in seen[0]


def test_fetch_findings_stops_at_hard_page_cap(monkeypatch):
    calls = []

    def fake_http(url, token, method="GET", body=None):
        calls.append(url)
        return {"issues": [{"key": f"k{len(calls)}"}], "paging": {"total": 9999}}

    monkeypatch.setattr(sg, "http_json", fake_http)
    findings = sg.fetch_findings("http://sonar.test", "tok", "proj", "BUG")
    assert len(findings) == 10  # hard cap: never more than 10 pages
    assert len(calls) == 10


def test_fetch_findings_empty(monkeypatch):
    monkeypatch.setattr(sg, "http_json", lambda *a, **k: {"issues": [], "paging": {"total": 0}})
    assert sg.fetch_findings("http://sonar.test", "tok", "proj", "BUG") == []


def test_count_findings_reads_paging_total(monkeypatch):
    monkeypatch.setattr(sg, "http_json", lambda *a, **k: {"paging": {"total": 7}})
    assert sg.count_findings("http://sonar.test", "tok", "proj", "CODE_SMELL") == 7


def test_count_findings_missing_paging_defaults_zero(monkeypatch):
    monkeypatch.setattr(sg, "http_json", lambda *a, **k: {})
    assert sg.count_findings("http://sonar.test", "tok", "proj", "CODE_SMELL") == 0


# ---------------------------------------------------------------------------
# format_finding
# ---------------------------------------------------------------------------

def test_format_finding_full():
    issue = {
        "severity": "CRITICAL",
        "rule": "go:S1234",
        "component": "skquad:control-plane/pkg/foo.go",
        "line": 42,
        "message": "possible   misuse\nof something",
    }
    out = sg.format_finding(issue)
    assert out == "- **CRITICAL** `go:S1234` — `control-plane/pkg/foo.go:42` — possible   misuse of something"


def test_format_finding_missing_fields_use_question_marks():
    out = sg.format_finding({})
    assert out.startswith("- **?** `?` — `?:?`")


def test_format_finding_truncates_long_message():
    out = sg.format_finding({"message": "x" * 250})
    msg = out.split("— ", 2)[-1]
    assert msg.endswith("...")
    assert len(msg) == 200


def test_format_finding_rule_falls_back_to_rule_name():
    out = sg.format_finding({"ruleName": "ts:S9999"})
    assert "`ts:S9999`" in out


# ---------------------------------------------------------------------------
# sync_kanbunny
# ---------------------------------------------------------------------------

class HttpRecorder:
    def __init__(self, cards):
        self.cards = cards
        self.calls = []

    def __call__(self, url, token, method="GET", body=None):
        self.calls.append((url, method, body))
        if method == "GET":
            return self.cards
        return {"id": "new-card-1"}


def kanbunny_env(monkeypatch):
    monkeypatch.setenv("KANBUNNY_URL", "http://kb.test/")
    monkeypatch.setenv("KANBUNNY_TOKEN", "kbtok")
    monkeypatch.setenv("KANBUNNY_BOARD", "board-1")


def test_sync_creates_card_when_none_exist(monkeypatch):
    kanbunny_env(monkeypatch)
    rec = HttpRecorder([])
    monkeypatch.setattr(sg, "http_json", rec)
    sg.sync_kanbunny([{"key": "i1", "severity": "CRITICAL", "message": "m"}])
    posts = [c for c in rec.calls if c[1] == "POST"]
    assert len(posts) == 1
    url, _, body = posts[0]
    assert url == "http://kb.test/api/boards/board-1/cards"
    assert body["assignee"] == "Sherlock"
    assert body["column"] == "todo"
    assert body["priority"] == 1
    assert sg.MARKER in body["title"]
    assert "(1)" in body["title"]


def test_sync_updates_existing_open_card(monkeypatch):
    kanbunny_env(monkeypatch)
    existing = [{"id": "c1", "title": f"🔴 {sg.MARKER} (1) [sonar:old]", "column": "in-progress"}]
    rec = HttpRecorder(existing)
    monkeypatch.setattr(sg, "http_json", rec)
    sg.sync_kanbunny([{"key": "i1"}, {"key": "i2"}])
    patches = [c for c in rec.calls if c[1] == "PATCH"]
    posts = [c for c in rec.calls if c[1] == "POST"]
    assert len(patches) == 1 and not posts
    assert patches[0][0] == "http://kb.test/api/cards/c1"
    assert "(2)" in patches[0][2]["title"]


def test_sync_ignores_closed_columns_and_creates_new(monkeypatch):
    kanbunny_env(monkeypatch)
    done = [{"id": "c1", "title": sg.MARKER, "column": "done"}]
    rec = HttpRecorder(done)
    monkeypatch.setattr(sg, "http_json", rec)
    sg.sync_kanbunny([{"key": "i1"}])
    assert any(c[1] == "POST" for c in rec.calls)
    assert not any(c[1] == "PATCH" for c in rec.calls)


def test_sync_marker_is_stable_for_same_key_set_and_changes_when_it_changes(monkeypatch):
    kanbunny_env(monkeypatch)
    titles = []
    for findings in ([{"key": "a"}, {"key": "b"}], [{"key": "a"}, {"key": "b"}], [{"key": "c"}]):
        rec = HttpRecorder([])
        monkeypatch.setattr(sg, "http_json", rec)
        sg.sync_kanbunny(findings)
        titles.append([c for c in rec.calls if c[1] == "POST"][0][2]["title"])
    assert titles[0] == titles[1]  # same set → same marker
    assert titles[0].split("[sonar:")[1] != titles[2].split("[sonar:")[1]


def test_sync_truncates_listing_beyond_max_listed(monkeypatch):
    kanbunny_env(monkeypatch)
    rec = HttpRecorder([])
    monkeypatch.setattr(sg, "http_json", rec)
    findings = [{"key": f"k{n}", "severity": "CRITICAL", "message": f"m{n}"} for n in range(53)]
    sg.sync_kanbunny(findings)
    desc = [c for c in rec.calls if c[1] == "POST"][0][2]["description"]
    assert "and 3 more" in desc
    assert "- **CRITICAL**" in desc


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

def patch_gate(monkeypatch, first, second, smells=0):
    calls = {"fetch": [], "sync": 0, "sleeps": []}

    # Deterministic sequence: first fetch → `first`, every later fetch → `second`.
    state = {"n": 0}

    def fake_fetch2(*a, **k):
        state["n"] += 1
        return first if state["n"] == 1 else second

    monkeypatch.setattr(sg, "fetch_findings", fake_fetch2)
    monkeypatch.setattr(sg, "count_findings", lambda *a, **k: smells)
    monkeypatch.setattr(sg, "sync_kanbunny", lambda f: calls.__setitem__("sync", calls["sync"] + 1))
    monkeypatch.setattr(sg.time, "sleep", lambda s: calls["sleeps"].append(s))
    return calls


def test_main_clean_returns_zero_without_sleep(monkeypatch):
    calls = patch_gate(monkeypatch, [], [])
    assert sg.main() == 0
    assert calls["sleeps"] == []
    assert calls["sync"] == 0


def test_main_reports_non_blocking_code_smells(monkeypatch, capsys):
    patch_gate(monkeypatch, [], [], smells=42)
    assert sg.main() == 0
    assert "42 open Critical/Blocker CODE_SMELL(s)" in capsys.readouterr().out


def test_main_recheck_clean_avoids_false_failure(monkeypatch):
    # First read sees a finding, re-check after settle sees none → pass.
    calls = patch_gate(monkeypatch, [{"key": "ghost"}], [])
    assert sg.main() == 0
    assert calls["sleeps"] == [0]  # GATE_SETTLE_SECONDS=0
    assert calls["sync"] == 0


def test_main_persistent_findings_fail_and_skip_kanbunny(monkeypatch, capsys):
    monkeypatch.setenv("SKIP_KANBUNNY", "1")
    finding = {"key": "i1", "severity": "BLOCKER", "message": "bad"}
    calls = patch_gate(monkeypatch, [finding], [finding])
    assert sg.main() == 1
    assert calls["sync"] == 0
    assert "skipped (SKIP_KANBUNNY=1)" in capsys.readouterr().out


def test_main_persistent_findings_fail_and_sync_kanbunny(monkeypatch):
    finding = {"key": "i1", "severity": "CRITICAL", "message": "bad"}
    calls = patch_gate(monkeypatch, [finding], [finding])
    assert sg.main() == 1
    assert calls["sync"] == 1


def test_main_missing_required_env_exits_2(monkeypatch):
    monkeypatch.delenv("SONAR_TOKEN", raising=False)
    with pytest.raises(SystemExit) as exc:
        sg.main()
    assert exc.value.code == 2
