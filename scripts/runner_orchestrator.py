"""
Runner orchestrator adapter for Miles pilot and live acceptance runs.

Versioned, secret-free coordinator that ties together safe telemetry capture,
journal collection, runcommand fallback, benchmark failure sanitization,
failure correlation, and stop-the-line evidence artifact generation.

Enforces zero leakage of secrets, signed URLs, credentials, or private ARNs.
"""
import datetime
import json
import os
from pathlib import Path
import time

from scripts.runner_diagnostics import (
    CHRONY_PAYLOAD_BEGIN,
    CHRONY_PAYLOAD_END,
    build_failed_window_payload,
    collect_node_journals_safe,
    collect_node_telemetry_safe,
    execute_with_runcommand_fallback,
    parse_chrony_response_payload,
    redact_sensitive_payload,
    redact_sensitive_strings,
)


DEFAULT_METRIC_PROGRAM = (
    "import urllib.request,json,pathlib;"
    "c=json.loads(pathlib.Path('/etc/miles/startup.json').read_text());"
    "print(urllib.request.urlopen('http://'+c['health_address']+'/metrics',timeout=5).read().decode())"
)

DEFAULT_CHRONY_PROGRAM = (
    "import datetime,json,subprocess,time;"
    "t0=time.monotonic();"
    "p=subprocess.run(['chronyc','-n','-c','tracking'],capture_output=True,text=True,timeout=5);"
    "t1=time.monotonic();"
    "now=datetime.datetime.now(datetime.timezone.utc);"
    "print('__CHRONY_PAYLOAD_BEGIN__');"
    "print(json.dumps({'version':1,'host_observed_at':now.isoformat(),'host_observed_unix':now.timestamp(),'host_chrony_elapsed_seconds':round(t1-t0,6),'returncode':p.returncode,'stdout':p.stdout,'stderr':p.stderr}));"
    "print('__CHRONY_PAYLOAD_END__')"
)


def build_journal_program(since_timestamp=0):
    return (
        "import json,subprocess\n"
        f"p=subprocess.run(['journalctl','-u','miles.service','--since','@{int(since_timestamp)}','-o','cat','--no-pager'],capture_output=True,text=True,timeout=25)\n"
        "lines=[]\n"
        "for line in p.stdout.splitlines():\n"
        "    line_clean=line.strip()\n"
        "    if not line_clean:continue\n"
        "    if ('s3 server failure' in line_clean or 's3 service unavailable' in line_clean or 'admission_gate' in line_clean):\n"
        "        lines.append(line_clean[:2048])\n"
        "print(json.dumps({'count':len(lines),'lines':lines[-120:]}))\n"
    )


def collect_cluster_telemetry(
    nodes,
    ssm_runner=None,
    ssm_runcommand_runner=None,
    ssh_runner=None,
    captured_at=None,
    now_unix=None,
    elapsed_seconds=0.0,
):
    """
    Safely captures cluster-wide telemetry across both cloud (SSM) and edge (SSH) nodes.
    Automatically handles Session Manager Standard_Stream errors via RunCommand fallback.
    """
    def fetcher(node):
        alias = node.get("alias", "unknown")
        profile = node.get("profile", "cloud")
        node_id = node.get("id", alias)
        t_start_node = datetime.datetime.now(datetime.timezone.utc).isoformat()
        t_mono_start = time.monotonic()

        if profile == "cloud":
            if not ssm_runner:
                raise RuntimeError(f"No SSM runner configured for cloud node {alias}")

            def run_session(prog):
                return ssm_runner(node_id, prog)

            def run_rc(prog):
                if not ssm_runcommand_runner:
                    raise RuntimeError(
                        f"SSM Session Manager failed on node '{alias}' and no genuine ssm_runcommand_runner fallback is configured"
                    )
                return ssm_runcommand_runner(node_id, prog)

            def exec_cmd(prog):
                return execute_with_runcommand_fallback(
                    lambda: run_session(prog),
                    lambda: run_rc(prog),
                )

            metrics_raw = exec_cmd(DEFAULT_METRIC_PROGRAM).replace("\r", "")
            raw_chrony_output = ""
            chrony_error = None
            t_chrony_start = time.monotonic()
            try:
                raw_chrony_output = exec_cmd(DEFAULT_CHRONY_PROGRAM).replace("\r", "")
            except Exception as ce:
                chrony_error = redact_sensitive_strings(str(ce))
            t_chrony_finish = time.monotonic()
            transport_latency = round(t_chrony_finish - t_chrony_start, 4)
            t_finish_node = datetime.datetime.now(datetime.timezone.utc).isoformat()
            t_mono_finish = time.monotonic()

            payload = parse_chrony_response_payload(raw_chrony_output)
            chrony_raw = payload["chrony_raw"]
            host_observed_at = payload["host_observed_at"]
            host_observed_unix = payload["host_observed_unix"]
            host_chrony_elapsed = payload["host_chrony_elapsed_seconds"]
            runtime_elapsed_verified = payload["runtime_elapsed_verified"]
            envelope_valid = payload.get("envelope_valid", False)
            envelope_error = payload.get("envelope_error")
            remote_rc = payload.get("returncode")

            if remote_rc is not None and remote_rc != 0:
                chrony_error = chrony_error or f"chronyc_failed_rc_{remote_rc}"
            elif not envelope_valid and envelope_error and envelope_error != "no_envelope_present":
                if not chrony_raw:
                    chrony_error = chrony_error or envelope_error

            return {
                "metrics_raw": metrics_raw,
                "chrony_raw": chrony_raw,
                "chrony_error": chrony_error,
                "readyz_status": None,
                "probe_started_at": t_start_node,
                "probe_finished_at": t_finish_node,
                "probe_duration_seconds": round(t_mono_finish - t_mono_start, 4),
                "transport_latency_seconds": transport_latency,
                "chrony_elapsed_seconds": host_chrony_elapsed,
                "host_chrony_elapsed_seconds": host_chrony_elapsed,
                "host_observed_at": host_observed_at,
                "host_observed_unix": host_observed_unix,
                "runtime_elapsed_verified": runtime_elapsed_verified,
                "envelope_valid": envelope_valid,
                "envelope_error": envelope_error,
                "returncode": remote_rc,
            }

        elif profile == "edge":
            if not ssh_runner:
                raise RuntimeError(f"No SSH runner configured for edge node {alias}")

            r = ssh_runner(["curl", "--silent", "--fail", "http://127.0.0.1:9100/metrics"])
            metrics_raw = r.stdout.decode() if hasattr(r, "stdout") else str(r)
            raw_chrony_output = ""
            chrony_error = None
            t_chrony_start = time.monotonic()
            try:
                rc = ssh_runner(["python3", "-c", DEFAULT_CHRONY_PROGRAM])
                raw_chrony_output = rc.stdout.decode() if hasattr(rc, "stdout") else str(rc)
            except Exception:
                try:
                    rc = ssh_runner(["chronyc", "-n", "-c", "tracking"])
                    raw_chrony_output = rc.stdout.decode() if hasattr(rc, "stdout") else str(rc)
                except Exception as ce:
                    chrony_error = redact_sensitive_strings(str(ce))
            t_chrony_finish = time.monotonic()
            transport_latency = round(t_chrony_finish - t_chrony_start, 4)
            t_finish_node = datetime.datetime.now(datetime.timezone.utc).isoformat()
            t_mono_finish = time.monotonic()

            payload = parse_chrony_response_payload(raw_chrony_output)
            chrony_raw = payload["chrony_raw"]
            host_observed_at = payload["host_observed_at"]
            host_observed_unix = payload["host_observed_unix"]
            host_chrony_elapsed = payload["host_chrony_elapsed_seconds"]
            runtime_elapsed_verified = payload["runtime_elapsed_verified"]
            envelope_valid = payload.get("envelope_valid", False)
            envelope_error = payload.get("envelope_error")
            remote_rc = payload.get("returncode")

            if remote_rc is not None and remote_rc != 0:
                chrony_error = chrony_error or f"chronyc_failed_rc_{remote_rc}"
            elif not envelope_valid and envelope_error and envelope_error != "no_envelope_present":
                if not chrony_raw:
                    chrony_error = chrony_error or envelope_error

            return {
                "metrics_raw": metrics_raw,
                "chrony_raw": chrony_raw,
                "chrony_error": chrony_error,
                "readyz_status": None,
                "probe_started_at": t_start_node,
                "probe_finished_at": t_finish_node,
                "probe_duration_seconds": round(t_mono_finish - t_mono_start, 4),
                "transport_latency_seconds": transport_latency,
                "chrony_elapsed_seconds": host_chrony_elapsed,
                "host_chrony_elapsed_seconds": host_chrony_elapsed,
                "host_observed_at": host_observed_at,
                "host_observed_unix": host_observed_unix,
                "runtime_elapsed_verified": runtime_elapsed_verified,
                "envelope_valid": envelope_valid,
                "envelope_error": envelope_error,
                "returncode": remote_rc,
            }

        else:
            raise ValueError(f"Unknown node profile: {profile}")

    return collect_node_telemetry_safe(
        nodes=nodes,
        fetcher_fn=fetcher,
        captured_at=captured_at,
        now_unix=now_unix,
        elapsed_seconds=elapsed_seconds,
    )


def collect_cluster_journals(
    nodes,
    since_timestamp,
    ssm_runner=None,
    ssm_runcommand_runner=None,
    ssh_runner=None,
):
    """
    Safely captures systemd/journalctl logs across cloud and edge nodes.
    Isolates per-node connection drops and redacts all lines deterministically.
    """
    journal_prog = build_journal_program(since_timestamp)

    def journal_fetcher(node):
        alias = node.get("alias", "unknown")
        profile = node.get("profile", "cloud")
        node_id = node.get("id", alias)

        if profile == "cloud":
            if not ssm_runner:
                raise RuntimeError(f"No SSM runner configured for cloud node {alias}")

            def run_session():
                return ssm_runner(node_id, journal_prog)

            def run_rc():
                if not ssm_runcommand_runner:
                    raise RuntimeError(
                        f"SSM Session Manager failed on node '{alias}' and no genuine ssm_runcommand_runner fallback is configured"
                    )
                return ssm_runcommand_runner(node_id, journal_prog)

            raw = execute_with_runcommand_fallback(run_session, run_rc)
            data = json.loads(raw)
            return {"lines": data.get("lines", [])}

        elif profile == "edge":
            if not ssh_runner:
                raise RuntimeError(f"No SSH runner configured for edge node {alias}")

            cmd = [
                "sudo", "-n", "journalctl", "-u", "miles.service",
                "--since", f"@{int(since_timestamp)}",
                "-o", "cat", "--no-pager"
            ]
            response = ssh_runner(cmd)
            stdout = response.stdout.decode() if hasattr(response, "stdout") else str(response)
            lines = [
                line[:2048] for line in stdout.splitlines()
                if ("s3 server failure" in line or "s3 service unavailable" in line or "admission_gate" in line)
            ]
            return {"lines": lines[-120:]}

        else:
            raise ValueError(f"Unknown node profile: {profile}")

    return collect_node_journals_safe(nodes=nodes, journal_fetcher_fn=journal_fetcher)


def capture_and_assemble_failed_window(
    name,
    nodes,
    client_result=None,
    before_metrics=None,
    after_metrics=None,
    since_timestamp=0,
    ssm_runner=None,
    ssm_runcommand_runner=None,
    ssh_runner=None,
    alb_evidence=None,
    out_dir=None,
    release_hash=None,
    save_private_trace=False,
    private_dir=None,
):
    """
    Coordinates end-to-end evidence capture and assembly upon trial failure.
    Ensures that diagnostics are safely persisted to disk before teardown or exit.

    Returns (public_payload, artifact_path).
    """
    captured_at = datetime.datetime.now(datetime.timezone.utc).isoformat()

    # 1. Capture after-window telemetry if not already provided
    if after_metrics is None:
        try:
            after_metrics = collect_cluster_telemetry(
                nodes=nodes,
                ssm_runner=ssm_runner,
                ssm_runcommand_runner=ssm_runcommand_runner,
                ssh_runner=ssh_runner,
                captured_at=captured_at,
            )
        except Exception as e:
            after_metrics = {
                "status": "error",
                "missing_reason": f"telemetry_collection_exception: {redact_sensitive_strings(str(e))}",
                "captured_at": captured_at,
                "nodes": {},
            }

    # 2. Capture cluster journals safely
    try:
        node_journals, node_status = collect_cluster_journals(
            nodes=nodes,
            since_timestamp=since_timestamp,
            ssm_runner=ssm_runner,
            ssm_runcommand_runner=ssm_runcommand_runner,
            ssh_runner=ssh_runner,
        )
    except Exception as e:
        node_journals = []
        node_status = {n.get("alias", "unknown"): "error" for n in nodes}

    # 3. Build sanitized payload with failure correlation
    public_payload = build_failed_window_payload(
        name=name,
        captured_at=captured_at,
        client_result=client_result,
        node_journals=node_journals,
        node_status=node_status,
        before_metrics=before_metrics,
        after_metrics=after_metrics,
        release_hash=release_hash,
        alb_evidence=alb_evidence,
        schema_version=1,
    )

    # 4. Save public failed-window artifact if out_dir specified
    artifact_path = None
    if out_dir:
        out_path = Path(out_dir)
        out_path.mkdir(parents=True, exist_ok=True)
        artifact_path = out_path / f"{name}-failed-window.json"
        tmp_path = out_path / f".{name}-failed-window.json.tmp.{os.getpid()}"
        tmp_path.write_text(json.dumps(public_payload, indent=2))
        try:
            tmp_path.chmod(0o644)
        except OSError:
            pass
        os.replace(tmp_path, artifact_path)

    # 5. Optionally write private raw trace if requested
    if save_private_trace and (private_dir or out_dir):
        p_dir = Path(private_dir or out_dir)
        p_dir.mkdir(parents=True, exist_ok=True)
        private_path = p_dir / f"{name}-private-raw-trace.json"
        tmp_path = p_dir / f".{name}-private-raw-trace.json.tmp.{os.getpid()}"
        raw_trace = {
            "name": name,
            "captured_at": captured_at,
            "raw_client_result": client_result,
            "raw_node_journals": node_journals,
            "before_metrics": before_metrics,
            "after_metrics": after_metrics,
        }
        fd = os.open(str(tmp_path), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            json.dump(raw_trace, f, indent=2)
        os.replace(tmp_path, private_path)

    return public_payload, artifact_path


def create_aws_cli_runcommand_runner(region="ap-southeast-1", env=None, timeout=90):
    """
    Builds a bounded, secret-free SSM RunCommand runner using AWS CLI.
    Executes python snippets via AWS-RunShellScript with bounded invocation polling.
    """
    import base64
    import re
    import secrets
    import subprocess
    import time

    def run_cmd(node_id, program):
        nonce = secrets.token_hex(8)
        begin = f"__MILES_RC_BEGIN_{nonce}__"
        end = f"__MILES_RC_END_{nonce}__"
        wrapper = (
            "import base64,traceback\n"
            f"print({repr(begin)},flush=True)\n"
            "rc=0\n"
            "try:\n"
            f" exec(compile(base64.b64decode({repr(base64.b64encode(program.encode()).decode())}),'<runcommand>','exec'))\n"
            "except BaseException:\n"
            " rc=1;traceback.print_exc()\n"
            f"print({repr(end)},rc,flush=True)\n"
        )
        shell_cmd = f"sudo -n python3 -c \"import base64;exec(base64.b64decode('{base64.b64encode(wrapper.encode()).decode()}'))\"\n"

        send = subprocess.run(
            [
                "aws", "--region", region, "ssm", "send-command",
                "--instance-ids", node_id,
                "--document-name", "AWS-RunShellScript",
                "--parameters", json.dumps({"commands": [shell_cmd]}),
                "--timeout-seconds", str(min(120, timeout)),
            ],
            capture_output=True,
            env=env,
            text=True,
            check=False,
            timeout=min(60, timeout),
        )
        if send.returncode != 0:
            raise RuntimeError(f"SSM send-command failed on {node_id}: {send.stderr[-1000:]}")
        cmd_data = json.loads(send.stdout)
        cmd_id = cmd_data["Command"]["CommandId"]

        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            time.sleep(2)
            inv = subprocess.run(
                [
                    "aws", "--region", region, "ssm", "get-command-invocation",
                    "--command-id", cmd_id,
                    "--instance-id", node_id,
                ],
                capture_output=True,
                env=env,
                text=True,
                check=False,
                timeout=min(30, timeout),
            )
            if inv.returncode != 0:
                continue
            inv_data = json.loads(inv.stdout)
            status = inv_data.get("Status")
            if status in ("Success", "Failed", "TimedOut", "Cancelled"):
                out = inv_data.get("StandardOutputContent", "")
                if status != "Success":
                    raise RuntimeError(f"RunCommand status {status}: {inv_data.get('StandardErrorContent', '')[-1000:]}")
                if begin not in out or end not in out:
                    raise RuntimeError(
                        f"Incomplete RunCommand output on {node_id}: missing begin/end sentinels (truncated or corrupted)"
                    )
                start = out.index(begin) + len(begin)
                match = re.search(re.escape(end) + r" (\d+)", out)
                if not match:
                    raise RuntimeError(
                        f"Incomplete RunCommand output on {node_id}: missing returncode after end sentinel"
                    )
                body = out[start:match.start()].strip()
                if match.group(1) != "0":
                    raise RuntimeError(f"Remote RunCommand failed (rc={match.group(1)}): {body[-2000:]}")
                return body
        raise RuntimeError(f"SSM RunCommand timed out waiting for command {cmd_id} on {node_id}")

    return run_cmd


def execute_workload_and_capture_on_failure(
    workload_fn,
    name,
    nodes,
    before_metrics=None,
    since_timestamp=0,
    ssm_runner=None,
    ssm_runcommand_runner=None,
    ssh_runner=None,
    alb_evidence=None,
    out_dir=None,
    release_hash=None,
    save_private_trace=False,
    private_dir=None,
    fetch_result_fn=None,
):
    """
    Executes workload_fn with guaranteed failure evidence capture.
    If workload_fn raises ANY Exception (e.g. TimeoutExpired, ConnectionError)
    or returns an object with non-zero returncode, it guarantees that
    telemetry, journals, and correlated diagnostics are collected and
    written to disk BEFORE propagating the exception.
    """
    try:
        res = workload_fn()
        rc = getattr(res, "returncode", 0)
        if rc != 0:
            raise RuntimeError(f"Workload execution failed with non-zero return code: {rc}")
        return res
    except BaseException as exc:
        client_result = None
        if fetch_result_fn:
            try:
                client_result = fetch_result_fn()
            except Exception:
                pass

        capture_err = None
        try:
            capture_and_assemble_failed_window(
                name=name,
                nodes=nodes,
                client_result=client_result,
                before_metrics=before_metrics,
                since_timestamp=since_timestamp,
                ssm_runner=ssm_runner,
                ssm_runcommand_runner=ssm_runcommand_runner,
                ssh_runner=ssh_runner,
                alb_evidence=alb_evidence,
                out_dir=out_dir,
                release_hash=release_hash,
                save_private_trace=save_private_trace,
                private_dir=private_dir,
            )
        except Exception as ce:
            capture_err = ce
            try:
                spool_dir = Path("/tmp/miles-failure-spool")
                spool_dir.mkdir(parents=True, exist_ok=True)
                try:
                    spool_dir.chmod(0o700)
                except OSError:
                    pass
                capture_and_assemble_failed_window(
                    name=name,
                    nodes=nodes,
                    client_result=client_result,
                    before_metrics=before_metrics,
                    since_timestamp=since_timestamp,
                    ssm_runner=ssm_runner,
                    ssm_runcommand_runner=ssm_runcommand_runner,
                    ssh_runner=ssh_runner,
                    alb_evidence=alb_evidence,
                    out_dir=spool_dir,
                    release_hash=release_hash,
                    save_private_trace=save_private_trace,
                    private_dir=spool_dir,
                )
            except Exception:
                pass

        if capture_err:
            raise RuntimeError(
                f"Workload failed with {type(exc).__name__} ('{exc}'), AND failure evidence persistence failed: {type(capture_err).__name__} ('{capture_err}')"
            ) from exc
        raise exc
