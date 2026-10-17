"""Run real config-gen with isolated dummy fixtures, without operator env files."""

import os
import pathlib
import secrets
import shutil
import subprocess  # nosec B404
import tempfile
import textwrap
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
DOCKER = shutil.which("docker")
if DOCKER is None:
    raise RuntimeError("Docker is required")


def docker(arguments, timeout=30, check=True, **kwargs):
    """Run fixed Docker argv without a shell or production configuration."""
    return subprocess.run(  # nosec B603
        [DOCKER, *arguments], check=check, timeout=timeout, **kwargs
    )


class TerminalConfiguration(unittest.TestCase):
    """Check actual rendered headers and atomic rejection of bad settings."""

    @classmethod
    def setUpClass(cls):
        cls.image = "nsl-config-test:" + secrets.token_hex(6)
        cls.addClassCleanup(
            docker,
            ["image", "rm", cls.image],
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        compose = (ROOT / "docker-compose.yml").read_text()
        block = compose.split("  config-gen:\n", 1)[1]
        block = block.split("      - |\n", 1)[1].split("    environment:\n", 1)[0]
        cls.command = ["sh", "-ec", textwrap.dedent(block).replace("$$", "$")]
        docker(
            ["build", "-t", cls.image, str(ROOT / "config-gen")],
            timeout=300,
            stdout=subprocess.DEVNULL,
        )

    def setUp(self):
        self.output = tempfile.TemporaryDirectory()
        self.addCleanup(self.output.cleanup)
        self.rendered = pathlib.Path(self.output.name) / "infra.yml"

    def render(self, url, header):
        container = "nsl-config-test-" + secrets.token_hex(6)
        arguments = [
            "run",
            "--rm",
            "--name",
            container,
            "--network",
            "none",
            "--user",
            f"{os.getuid()}:{os.getgid()}",
            "-v",
            f"{ROOT}/traefik/dynamic/infra.yml.tmpl:/templates/infra.yml.tmpl:ro",
            "-v",
            f"{self.output.name}:/output",
        ]
        values = {
            "DOMAIN": "test.invalid",
            "NODE_NAME": "windows-pc",
            "REGISTRY_PROXY_TOKEN": secrets.token_hex(16),
            "NSL_TERMINAL_URL": url,
            "NSL_TERMINAL_AUTHORIZATION": header,
        }
        for name, value in values.items():
            arguments.extend(["-e", f"{name}={value}"])
        try:
            docker(arguments + [self.image] + self.command, capture_output=True)
        except subprocess.CalledProcessError:
            return False
        finally:
            docker(
                ["rm", "-f", container],
                check=False,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        return True

    def test_default_and_protected_target(self):
        for target, header in [
            ("http://host.docker.internal:7681", ""),
            ("http://172.22.21.38:7681", "Basic dGVzdDp0ZXN0"),
        ]:
            with self.subTest(target=target):
                self.assertTrue(self.render(target, header))
                text = self.rendered.read_text()
                self.assertIn(f'          - url: "{target}"', text)
                self.assertIn(f'          Authorization: "{header}"', text)
                self.assertIn(
                    "        - auth\n        - authz-check\n"
                    "        - terminal-backend\n      service: ttyd",
                    text,
                )
                self.assertNotIn("${", text)

    def test_invalid_inputs_preserve_previous_output(self):
        self.assertTrue(self.render("http://host:7681", ""))
        previous = self.rendered.read_bytes()
        for target, header in [
            ("http://user:pass@host:7681", ""),
            ("http://host:7681/path", ""),
            ("http://host:0", ""),
            ("http://host:65536", ""),
            ("http://host:999999999999999999999", ""),
            ("http://host:7681\nhttp://other:7681", ""),
            ("http://host:7681", "bad\nheader"),
            ("http://host:7681", "Basic dGVzdA==\r"),
            ("http://host:7681", "Basic dGVzdA==\nBasic dGVzdA=="),
            ("http://host:7681", "Basic "),
        ]:
            with self.subTest(target=target, header=header):
                self.assertFalse(self.render(target, header))
                self.assertEqual(self.rendered.read_bytes(), previous)


if __name__ == "__main__":
    unittest.main()
