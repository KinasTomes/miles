#!/usr/bin/env python3
"""Approved W3 single-node AWS trial; no provisioning and no static access keys."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import signal
import subprocess
import time
from urllib.request import build_opener, ProxyHandler, Request


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--config', type=Path, required=True)
    ap.add_argument('--instance-id', required=True)
    args = ap.parse_args()
    c = json.loads(args.config.read_text())
    work = Path('/opt/miles-benchmark/run')
    work.mkdir(mode=0o700)
    http = build_opener(ProxyHandler({}))
    token = http.open(Request('http://169.254.169.254/latest/api/token', method='PUT', headers={'X-aws-ec2-metadata-token-ttl-seconds': '60'}), timeout=5).read().decode()
    document = json.loads(http.open(Request('http://169.254.169.254/latest/dynamic/instance-identity/document', headers={'X-aws-ec2-metadata-token': token}), timeout=5).read())
    assert document['instanceId'] == args.instance_id
    assert document['instanceType'] == c['instance_type'] and document['region'] == c['region'] and document['accountId'] == c['account']
    env = {k: v for k, v in os.environ.items() if not k.startswith('AWS_')}
    env.update(AWS_DEFAULT_REGION=c['region'], AWS_REGION=c['region'], AWS_CONFIG_FILE='/dev/null', AWS_SHARED_CREDENTIALS_FILE='/dev/null', AWS_PAGER='', AWS_MAX_ATTEMPTS='1')
    finish_by = time.monotonic() + 80 * 60

    def save(path, value):
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, 'w') as f:
            json.dump(value, f, indent=2)
            f.write('\n')

    def run(argv, selected_env=None, seconds=120):
        left = int(finish_by - time.monotonic())
        if left < 1:
            raise RuntimeError('approved workload time window exhausted')
        r = subprocess.run(argv, env=selected_env or env, text=True, capture_output=True, timeout=min(seconds, left))
        if r.returncode:
            name = argv[0].split('/')[-1]
            (work / (name + '-error.log')).write_text(r.stderr)
            raise RuntimeError(name + ' failed; diagnostic retained privately')
        return r.stdout

    def aws(*argv, selected_env=None):
        return run(['aws', '--region', c['region'], '--output', 'json', '--no-cli-pager', *argv], selected_env)

    def inventory(bucket):
        data = json.loads(aws('s3api', 'list-objects-v2', '--bucket', bucket))
        objects = data.get('Contents', [])
        return {'objects': len(objects), 'bytes': sum(x['Size'] for x in objects), 'listing': data}

    def upload(path, key):
        aws('s3', 'cp', str(path), 's3://' + c['buckets']['direct'] + '/evidence/' + key, '--sse', 'aws:kms', '--sse-kms-key-id', 'alias/aws/s3', '--only-show-errors')

    identity = json.loads(aws('sts', 'get-caller-identity'))
    assert identity['Account'] == c['account'] and ':assumed-role/' + c['name'] + '/' in identity['Arn']
    exported = json.loads(aws('configure', 'export-credentials', '--format', 'process'))
    credentials = work / 'aws-session.json'
    assert exported['SessionToken']
    save(credentials, {'access_key_id': exported['AccessKeyId'], 'secret_access_key': exported['SecretAccessKey'], 'session_token': exported['SessionToken']})
    binary = Path('/opt/miles-benchmark/bin')
    for name, expected in c['binary_hashes'].items():
        assert hashlib.sha256((binary / name).read_bytes()).hexdigest() == expected
    run([str(binary / 'datasetgen'), '--profile', 'backup30', '--seed', 'miles-w02-v1', '--out', str(work / 'dataset')])
    manifest = work / 'dataset' / 'manifest.json'
    dataset = json.loads(manifest.read_text())
    cpu = next(x.split(':', 1)[1].strip() for x in Path('/proc/cpuinfo').read_text().splitlines() if x.startswith('model name'))
    memory = next(x for x in Path('/proc/meminfo').read_text().splitlines() if x.startswith('MemTotal:'))
    summary = {'status': 'running', 'kind': 'preliminary-real-s3-badger-warm', 'instance_identity': document, 'caller_identity': identity,
               'memory': memory, 'cpu': cpu, 'kernel': platform.release(), 'aws_cli': run(['aws', '--version']).strip(),
               'manifest_sha256': hashlib.sha256(manifest.read_bytes()).hexdigest(), 'dataset_bytes': dataset['total_bytes'],
               'sample_files': len(dataset['files']), 'runs': [], 'limitations': [
                   'Client and Miles share this one EC2. Direct client uses HTTPS; Miles client uses loopback HTTP and backend HTTPS.',
                   'Real-clock local admission only; no distributed R1/R2 or UTC uncertainty proof.', 'm7i-flex CPU baseline is 40%, with opportunistic burst; record observed CPU and do not apply fixed-performance targets.',
                   'Outbox/payload retained in Badger, exporter/seal/checkpoint absent. Logical bytes are not billed/backend wire bytes.',
                   'No final performance target or savings claim; S3 internal cache is uncontrolled.']}
    save(work / 'summary.json', summary)
    endpoint = 'https://s3.' + c['region'] + '.amazonaws.com'
    common = {'host': 'EC2 ' + c['instance_type'] + '; client and gateway on the same 2-vCPU/8-GiB host, Flex CPU baseline 40%', 'cpu': cpu,
              'disk': run(['stat', '-f', '-c', '%T', str(work)]).strip() + '; encrypted gp3 20 GiB, default throughput/IOPS',
              'encryption': 'SSE-KMS alias/aws/s3, bucket keys disabled, Standard',
              'topology': 'W3 single node EC2 + real S3 in ap-southeast-1; no ALB; not final Cloud/Edge topology',
              'network': 'Direct client HTTPS to S3; Miles client loopback HTTP, backend HTTPS to same-Region S3',
              'async_work': 'producer/outbox retained locally; no exporter/seal/checkpoint; async debt not drained'}
    try:
        for concurrency in (1, 4):
            for trial in (1, 2, 3):
                label = 'c' + str(concurrency) + '-t' + str(trial)
                folder = work / label
                folder.mkdir(mode=0o700)
                start_inventory = {'direct': inventory(c['buckets']['direct']), 'packs': inventory(c['buckets']['packs'])}
                assert start_inventory['packs']['objects'] == 0
                save(folder / 'start-inventory.json', start_inventory)

                def workload(target, origin, bucket, secret_file):
                    declaration = dict(common, metadata='badger' if target == 'miles' else 'none', gateway_commit=c['source_commit'] if target == 'miles' else '',
                                       policy_revision='1' if target == 'miles' else '', cache_state='fresh process/LRU, no write admission; read passes separate' if target == 'miles' else 'uncontrolled S3 service internals',
                                       isolation='fresh Badger/domain/cache and verified-empty physical bucket' if target == 'miles' else 'fresh random prefix; all previous trial data deleted')
                    config = {'target': 'miles-warm' if target == 'miles' else 'direct-s3', 'endpoint': origin, 'bucket': bucket,
                              'region': c['region'], 'credentials_file': str(secret_file), 'kms_key_id': 'alias/aws/s3', 'environment': declaration}
                    file = folder / (target + '-config.json')
                    save(file, config)
                    run([str(binary / 'warmbench'), '--config', str(file), '--manifest', str(manifest), '--out', str(folder / target), '--concurrency', str(concurrency), '--read-repeats', '3'], seconds=900)
                    raw = (folder / target / 'result.json').read_bytes()
                    r = json.loads(raw)
                    gets = [s for s in r['samples'] if s['operation'] == 'GetObject']
                    assert r['status'] == 'passed' and r['client']['vcs_revision'] == c['source_commit'] and r['client']['vcs.modified'] == 'false'
                    assert len(gets) == len(dataset['files']) * 4 and all(not s.get('error') and len(s.get('sha256', '')) == 64 for s in gets)
                    assert r['manifest_sha256'] == summary['manifest_sha256']
                    summary['runs'].append({'trial': label, 'target': target, 'concurrency': concurrency, 'artifact_sha256': hashlib.sha256(raw).hexdigest(),
                                            'samples': len(r['samples']), 'verified_gets': len(gets), 'phases': r['phases']})
                    upload(folder / target / 'result.json', label + '/' + target + '-result.json')
                    save(work / 'summary.json', summary)
                    print('PASS', label, target, len(gets), 'full-file GET hashes', flush=True)

                workload('direct', endpoint, c['buckets']['direct'], credentials)
                startup = {'profile': 'cloud', 'mode': 'benchmark', 'region': c['region'], 'local_data_path': str(folder / 'runtime'),
                           'pack_endpoint': endpoint, 'pack_bucket': c['buckets']['packs'], 'pack_credentials_file': str(credentials), 'pack_kms_key_id': 'alias/aws/s3',
                           'health_address': '127.0.0.1:0', 's3_address': '127.0.0.1:0', 'shutdown_seconds': 60}
                startup_file = folder / 'startup.json'
                save(startup_file, startup)
                log = folder / 'gateway.log'
                with log.open('w') as output:
                    process = subprocess.Popen([str(binary / 'miles'), '-config', str(startup_file)], env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=output)
                    try:
                        ready_by = time.monotonic() + 30
                        gateway = None
                        while time.monotonic() < ready_by:
                            if process.poll() is not None:
                                raise RuntimeError('gateway startup failed; private log retained')
                            addresses = {}
                            for line in log.read_text().splitlines(keepends=True):
                                if line.endswith('\n'):
                                    x = json.loads(line)
                                    addresses[x.get('msg')] = x.get('address')
                            if addresses.get('health server listening') and addresses.get('s3 server listening'):
                                try:
                                    status = http.open('http://' + addresses['health server listening'] + '/readyz', timeout=3).status
                                except Exception:
                                    status = 503
                                if status == 200:
                                    gateway = 'http://' + addresses['s3 server listening']
                                    break
                            time.sleep(.1)
                        assert gateway, 'gateway never ready'
                        credential_file = folder / 'runtime' / 'credentials.json'
                        cred = json.loads(credential_file.read_text())
                        gateway_env = dict(env, AWS_ACCESS_KEY_ID=cred['access_key_id'], AWS_SECRET_ACCESS_KEY=cred['secret_access_key'], AWS_EC2_METADATA_DISABLED='true')
                        aws('--endpoint-url', gateway, 's3api', 'create-bucket', '--bucket', 'warm-benchmark', selected_env=gateway_env)
                        workload('miles', gateway, 'warm-benchmark', credential_file)
                        process.send_signal(signal.SIGTERM)
                        assert process.wait(timeout=65) == 0
                        assert '"msg":"shutdown complete"' in log.read_text()
                    finally:
                        if process.poll() is None:
                            process.terminate()
                            try:
                                process.wait(timeout=65)
                            except subprocess.TimeoutExpired:
                                process.kill()
                                process.wait(timeout=5)
                files = [x.stat() for x in (folder / 'runtime' / 'metadata').rglob('*') if x.is_file()]
                end_inventory = {'direct': inventory(c['buckets']['direct']), 'packs': inventory(c['buckets']['packs']),
                                 'badger_file_bytes': sum(x.st_size for x in files), 'badger_allocated_bytes': sum(x.st_blocks * 512 for x in files),
                                 'gateway_shutdown_complete': True, 'async_debt': 'retained producer payload/outbox; no export/seal performed'}
                save(folder / 'end-inventory.json', end_inventory)
                upload(folder / 'start-inventory.json', label + '/start-inventory.json')
                upload(folder / 'end-inventory.json', label + '/end-inventory.json')
                # Explicit whole synthetic-installation teardown AFTER process
                # quiescence; this is not application GC or retention bypass.
                aws('s3', 'rm', 's3://' + c['buckets']['packs'] + '/', '--recursive', '--only-show-errors')
                assert inventory(c['buckets']['packs'])['objects'] == 0
                upload(work / 'summary.json', 'summary.json')
        summary['status'] = 'passed'
    except Exception as error:
        summary['status'] = 'failed'
        summary['failure_type'] = type(error).__name__
        summary['failure'] = str(error)[:500]
        raise
    finally:
        save(work / 'summary.json', summary)
        try:
            upload(work / 'summary.json', 'summary.json')
        except Exception:
            print('Final evidence upload failed; retrieve private EBS evidence before teardown.', flush=True)
    print('PASS real AWS paired preliminary Warm trial suite', flush=True)


if __name__ == '__main__':
    main()
