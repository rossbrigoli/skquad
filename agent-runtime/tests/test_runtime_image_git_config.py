"""Regression tests for the runtime image git credential helper config."""

from pathlib import Path


def test_runtime_image_invokes_helper_as_shell_command():
    """Git treats `credential.helper = foo` as `git credential-foo`.

    Our installed console script is named `skquad-git-credential`, not
    `git-credential-skquad-git-credential`, so the image must configure it
    with the shell-command form (`!cmd`) or git will never invoke it.
    """

    dockerfile = Path(__file__).resolve().parents[1] / "Dockerfile"
    text = dockerfile.read_text(encoding="utf-8")

    assert "helper = !skquad-git-credential" in text
    assert "helper = skquad-git-credential" not in text
