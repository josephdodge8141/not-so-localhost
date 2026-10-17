"""Exercise the real config generator without enrollment or live credentials."""

import json
import os
import pathlib
import secrets
import shutil
import subprocess  # nosec B404: fixed Docker argv, no shell execution
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
DOCKER = shutil.which("docker")
if DOCKER is None:
    raise RuntimeError("Docker is required")
CHECK = unittest.TestCase()


def docker(arguments, **kwargs):
    """Run only Docker commands assembled by this integration test."""
    return subprocess.run(  # nosec B603: fixed executable and local test argv
        [DOCKER, *arguments], cwd=ROOT, check=True, **kwargs
    )


configuration = docker(["compose", "config", "--format", "json"], capture_output=True)
command = json.loads(configuration.stdout)["services"]["config-gen"]["command"]
# Compose's config output retains escaped dollars; Docker's runtime sees one.
command = [value.replace("$$", "$") for value in command]
docker(["compose", "build", "config-gen"], stdout=subprocess.DEVNULL)

with tempfile.TemporaryDirectory() as output_dir:

    def render(url, authorization):
        arguments = [
            "run",
            "--rm",
            "--user",
            f"{os.getuid()}:{os.getgid()}",
            "-v",
            f"{ROOT}/traefik/dynamic/infra.yml.tmpl:/templates/infra.yml.tmpl:ro",
            "-v",
            f"{output_dir}:/output",
        ]
        environment = {
            "DOMAIN": "test.invalid",
            "NODE_NAME": "windows-pc",
            "REGISTRY_PROXY_TOKEN": secrets.token_hex(16),
            "NSL_TERMINAL_URL": url,
            "NSL_TERMINAL_AUTHORIZATION": authorization,
        }
        for name, value in environment.items():
            arguments.extend(["-e", f"{name}={value}"])
        try:
            docker(
                arguments + ["not-so-localhost-config-gen"] + command,
                capture_output=True,
            )
        except subprocess.CalledProcessError:
            return False
        return True

    for target, header in [
        ("http://host.docker.internal:7681", ""),
        ("http://172.22.21.38:7681", "Basic dGVzdDp0ZXN0"),
    ]:
        CHECK.assertTrue(render(target, header))
        text = (pathlib.Path(output_dir) / "infra.yml").read_text()
        CHECK.assertIn(target, text)
        CHECK.assertIn(
            "        - auth\n        - authz-check\n"
            "        - terminal-backend\n      service: ttyd",
            text,
        )
        CHECK.assertNotIn("${", text)

    for target, header in [
        ("http://user:pass@host:7681", ""),
        ("http://host:7681", "bad\nheader"),
        ("http://host:7681", "Basic dGVzdA==\nBasic dGVzdA=="),
    ]:
        CHECK.assertFalse(render(target, header))

print("Terminal config default/WSL rendering and invalid-input checks passed")
