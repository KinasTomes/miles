import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("local_test", Path(__file__).parents[1] / "local_test.py")
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class LocalFixtureBoundary(unittest.TestCase):
    def test_remote_wildcard_and_ambiguous_endpoints_are_rejected(self):
        for value in ("https://dynamodb.ap-southeast-1.amazonaws.com", "http://10.0.0.1:8000",
                      "http://0.0.0.0:8000", "http://localhost:8000", "http://127.0.0.1:8000/path",
                      "http://user:pass@127.0.0.1:8000", "http://127.0.0.1", "http://127.0.0.1:8000?q=x"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                runner.local_endpoint(value)
        self.assertEqual(runner.local_endpoint("http://127.0.0.1:18000/"), "http://127.0.0.1:18000")

    def test_port_open_does_not_replace_api_readiness(self):
        with patch.object(runner, "aws", side_effect=[RuntimeError("port open but no API response"), '{"TableNames":[]}']) as api, patch.object(runner.time, "sleep"):
            runner.wait_backend({}, "http://127.0.0.1:18000", "dynamodb")
            self.assertEqual(api.call_count, 2)

    def test_real_credentials_and_profiles_cannot_leak_into_fixture_cli(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {
            "AWS_PROFILE": "real", "AWS_SESSION_TOKEN": "sensitive", "AWS_ROLE_ARN": "real-role",
            "AWS_ACCESS_KEY_ID": "real-key", "AWS_ENDPOINT_URL": "https://remote.example",
        }):
            env = runner.fixture_environment(Path(tmp))
            self.assertEqual(env["AWS_ACCESS_KEY_ID"], "mileslocal")
            for name in ("AWS_PROFILE", "AWS_SESSION_TOKEN", "AWS_ROLE_ARN", "AWS_ENDPOINT_URL"):
                self.assertNotIn(name, env)
            self.assertEqual(env["AWS_EC2_METADATA_DISABLED"], "true")
            self.assertEqual(env["AWS_SHARED_CREDENTIALS_FILE"], os.devnull)


if __name__ == "__main__":
    unittest.main()
