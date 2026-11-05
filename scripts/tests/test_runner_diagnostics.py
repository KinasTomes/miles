import json
import re
import unittest

def parse_gateway_journal_lines(stdout_text):
    """
    Parses journalctl lines matching both structured JSON slog output and text slog output.
    Returns normalized list of dicts with request_id, node_id, stage, failure_class, msg.
    """
    out = []
    text_pattern = re.compile(
        r'(?:ERROR|WARN)\s+(?P<msg>s3\s+server\s+failure|s3\s+service\s+unavailable)\s+'
        r'.*?request_id=(?P<request_id>[A-Za-z0-9_-]+)'
        r'.*?node_id=(?P<node_id>[^\s]*)'
        r'.*?stage=(?P<stage>[^\s]+)'
        r'.*?failure_class=(?P<failure_class>[^\s]+)'
    )
    for line in stdout_text.splitlines():
        line = line.strip()
        if not line:
            continue
        # 1. Try structured JSON
        if line.startswith("{") and line.endswith("}"):
            try:
                d = json.loads(line)
                if d.get("msg") in ("s3 server failure", "s3 service unavailable"):
                    out.append({
                        "msg": d.get("msg"),
                        "request_id": d.get("request_id", ""),
                        "node_id": d.get("node_id", ""),
                        "stage": d.get("stage", ""),
                        "failure_class": d.get("failure_class", ""),
                    })
                    continue
            except Exception:
                pass
        # 2. Try text format
        m = text_pattern.search(line)
        if m:
            out.append({
                "msg": m.group("msg"),
                "request_id": m.group("request_id"),
                "node_id": m.group("node_id"),
                "stage": m.group("stage"),
                "failure_class": m.group("failure_class"),
            })
    return out


def execute_with_runcommand_fallback(session_runner, runcommand_runner):
    """
    Simulates audited execution with Session Manager first, falling back to RunCommand
    if Session Manager encounters Standard_Stream or pty failure.
    """
    try:
        return session_runner()
    except Exception as e:
        if "Standard_Stream" in str(e) or "session" in str(e).lower() or isinstance(e, RuntimeError):
            # Fallback cleanly to RunCommand
            return runcommand_runner()
        raise


class RunnerDiagnosticsTests(unittest.TestCase):
    def test_json_and_text_journal_parsing(self):
        json_line = json.dumps({
            "time": "2026-11-05T14:31:54Z",
            "level": "WARN",
            "msg": "s3 service unavailable",
            "request_id": "F4D42F0CD2CD743E",
            "node_id": "cloud-b",
            "method": "PUT",
            "operation": "PutObject",
            "stage": "prepare",
            "failure_class": "backend_unavailable"
        })
        text_line = "2026/11/05 14:32:00 ERROR s3 server failure request_id=3EE76450EAC2C164 node_id=cloud-a method=PUT operation=PutObject stage=credential_lookup failure_class=backend_unavailable"

        raw_journal = f"{json_line}\nsome random noise line\n{text_line}\n"
        parsed = parse_gateway_journal_lines(raw_journal)

        self.assertEqual(len(parsed), 2)
        self.assertEqual(parsed[0]["request_id"], "F4D42F0CD2CD743E")
        self.assertEqual(parsed[0]["node_id"], "cloud-b")
        self.assertEqual(parsed[0]["stage"], "prepare")
        self.assertEqual(parsed[0]["failure_class"], "backend_unavailable")
        self.assertEqual(parsed[0]["msg"], "s3 service unavailable")

        self.assertEqual(parsed[1]["request_id"], "3EE76450EAC2C164")
        self.assertEqual(parsed[1]["node_id"], "cloud-a")
        self.assertEqual(parsed[1]["stage"], "credential_lookup")
        self.assertEqual(parsed[1]["failure_class"], "backend_unavailable")
        self.assertEqual(parsed[1]["msg"], "s3 server failure")

    def test_ssm_session_manager_fallback_to_runcommand(self):
        def failing_session():
            raise RuntimeError("SSM Session Manager error: Standard_Stream not found")

        def successful_runcommand():
            return {"exit_code": 0, "stdout": "metrics_captured", "status": "Success"}

        result = execute_with_runcommand_fallback(failing_session, successful_runcommand)
        self.assertEqual(result["status"], "Success")
        self.assertEqual(result["stdout"], "metrics_captured")

    def test_classify_upstream_versus_gateway_failures(self):
        # Client observed 2 errors
        client_samples = [
            {"request_id": "F4D42F0CD2CD743E", "http_status": 503},
            {"request_id": "ALB-TIMEOUT-9999", "http_status": 503},
        ]
        gateway_records = {
            "F4D42F0CD2CD743E": {"node_id": "cloud-b", "stage": "prepare", "failure_class": "backend_unavailable"}
        }

        classified = []
        for s in client_samples:
            rid = s["request_id"]
            if rid in gateway_records:
                rec = gateway_records[rid]
                classified.append({
                    "request_id": rid,
                    "source": "gateway",
                    "node_id": rec["node_id"],
                    "stage": rec["stage"],
                    "failure_class": rec["failure_class"]
                })
            else:
                classified.append({
                    "request_id": rid,
                    "source": "upstream_no_gateway_request",
                    "http_status": s["http_status"]
                })

        self.assertEqual(len(classified), 2)
        self.assertEqual(classified[0]["source"], "gateway")
        self.assertEqual(classified[0]["stage"], "prepare")
        self.assertEqual(classified[1]["source"], "upstream_no_gateway_request")


if __name__ == "__main__":
    unittest.main()
