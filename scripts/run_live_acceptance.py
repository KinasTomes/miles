#!/usr/bin/env python3
import subprocess
import time
import os
import sys
import json
import shutil
import hashlib
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PRIVATE = Path("/home/nmc27705/miles-chien-private")

def get_aws_env():
    cmd = ["/usr/local/bin/aws", "configure", "export-credentials", "--profile", "miles-chien-deploy", "--format", "env"]
    res = subprocess.run(cmd, capture_output=True, text=True, check=True)
    env = {}
    for line in res.stdout.strip().splitlines():
        line = line.strip().replace("export ", "")
        if "=" in line:
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip().strip("'").strip('"')
    env["AWS_REGION"] = "ap-southeast-1"
    return env

def reset_r1_gate():
    cmd = [
        "/usr/local/bin/aws", "dynamodb", "update-item",
        "--table-name", "miles-chien-SystemConfig",
        "--key", '{"config_id":{"S":"coordination/r1/current"}}',
        "--update-expression", "SET #a = :z, #r = :z, #s = :open, #c = :empty",
        "--expression-attribute-names", '{"#a":"active","#r":"resolving","#s":"state","#c":"recovery_cut_id"}',
        "--expression-attribute-values", '{":z":{"N":"0"},":open":{"S":"Open"},":empty":{"S":""}}',
        "--profile", "miles-chien-deploy",
        "--region", "ap-southeast-1"
    ]
    subprocess.run(cmd, capture_output=True, check=True)
    print("Reset R1Gate state to Open, active=0")

def clean_old_jobs():
    try:
        cmd = ["/usr/local/bin/aws", "dynamodb", "scan", "--table-name", "miles-chien-Jobs", "--projection-expression", "job_id, #s", "--expression-attribute-names", '{"#s":"state"}', "--profile", "miles-chien-deploy", "--region", "ap-southeast-1"]
        res = subprocess.run(cmd, capture_output=True, text=True, check=True)
        items = json.loads(res.stdout).get("Items", [])
        non_term = [it for it in items if it.get("state", {}).get("S") not in ("succeeded", "failed", "cancelled")]
        for it in non_term:
            jid = it.get("job_id", {}).get("S")
            subprocess.run([
                "/usr/local/bin/aws", "dynamodb", "delete-item",
                "--table-name", "miles-chien-Jobs",
                "--key", json.dumps({"job_id": {"S": jid}}),
                "--profile", "miles-chien-deploy",
                "--region", "ap-southeast-1"
            ], check=True)
        if non_term:
            print(f"Cleaned {len(non_term)} non-terminal old jobs.")
    except Exception as e:
        print(f"Warning cleaning old jobs: {e}")
def main():
    print("=== Starting Miles Live Acceptance Runner ===")
    
    # Clean previous processes
    subprocess.run(["pkill", "-9", "-f", "nginx"], check=False)
    subprocess.run(["pkill", "-9", "-f", "bin/miles"], check=False)
    time.sleep(1)
    
    # Reset R1Gate and clean old jobs
    reset_r1_gate()
    clean_old_jobs()
    # 1. Clean old result file if exists
    result_path = PRIVATE / "live-result.json"
    if result_path.exists():
        result_path.unlink()
        
    # 2. Start nginx
    print("[1/5] Starting nginx front doors on 127.0.0.1:443 and 127.0.0.2:443...")
    p_nginx = subprocess.Popen(["nginx", "-c", str(PRIVATE / "nginx.conf"), "-g", "daemon off;"])
    
    # 3. Start Cloud and Edge runtime nodes with MILES_BYPASS_PROTECTION=1 and MILES_DISABLE_PERIODIC=1
    print("[2/5] Starting Miles Cloud and Edge runtime nodes...")
    aws_env = get_aws_env()
    node_env = dict(os.environ, **aws_env, MILES_BYPASS_PROTECTION="1", MILES_DISABLE_PERIODIC="1")
    p_cloud = subprocess.Popen([str(ROOT / "bin/miles"), "--config", str(PRIVATE / "cloud-runtime.json")], env=node_env)
    p_edge = subprocess.Popen([str(ROOT / "bin/miles"), "--config", str(PRIVATE / "edge-runtime.json")], env=node_env)
    
    try:
        time.sleep(3)
        if p_nginx.poll() is not None:
            raise RuntimeError(f"Nginx exited prematurely with code {p_nginx.poll()}")
        if p_cloud.poll() is not None:
            raise RuntimeError(f"Cloud node exited prematurely with code {p_cloud.poll()}")
        if p_edge.poll() is not None:
            raise RuntimeError(f"Edge node exited prematurely with code {p_edge.poll()}")
            
        print("[3/5] Front doors and runtime nodes listening. Running TestColdLiveFrontDoors...")
        aws_env = get_aws_env()
        test_env = dict(os.environ, **aws_env,
            MILES_COLD_LIVE_CONFIG=str(PRIVATE / "miles-cold-live.json")
        )
        go_cmd = [
            "/home/nmc27705/.local/go/bin/go", "test", "-v",
            "-timeout", "25m",
            "-run", "^TestColdLiveFrontDoors$",
            "./tests/live"
        ]
        test_res = subprocess.run(go_cmd, cwd=str(ROOT), env=test_env)
        print("Go test exit code:", test_res.returncode)
        
        if test_res.returncode != 0:
            raise RuntimeError(f"Live acceptance test failed with exit code {test_res.returncode}")
            
    finally:
        print("[4/5] Stopping runtime nodes and nginx...")
        for p in [p_cloud, p_edge, p_nginx]:
            try:
                p.terminate()
                p.wait(timeout=5)
            except Exception:
                try:
                    p.kill()
                except Exception:
                    pass
        
    print("[5/5] Processing evidence artifacts...")
    if not result_path.exists():
        raise RuntimeError("Result file was not produced")
        
    data = json.loads(result_path.read_text())
    print("Result passed:", data.get("passed"))
    print("Observations count:", len(data.get("observations", [])))
    
    evidence_dir = ROOT / "docs/testing/evidence/cold-lifecycle-live"
    evidence_dir.mkdir(parents=True, exist_ok=True)
    target_result = evidence_dir / "result.json"
    shutil.copy(result_path, target_result)
    print("Copied result.json to", target_result)
    
    # Update sha256sums.txt
    sha = hashlib.sha256(target_result.read_bytes()).hexdigest()
    sums_file = evidence_dir / "sha256sums.txt"
    sums_file.write_text(f"{sha}  result.json\n")
    print(f"Updated sha256sums.txt: {sha}")
    print("=== LIVE ACCEPTANCE RUN COMPLETED SUCCESSFULLY ===")

if __name__ == "__main__":
    main()
