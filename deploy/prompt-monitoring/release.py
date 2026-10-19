#!/usr/bin/env python3
"""Bounded binary replacement with an independent systemd recovery timer.

Never changes config, other services, or existing archives. Backups contain
private data and must stay in a root-owned 0700 release directory.
"""
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

SERVICE = 'workbuddy2api.service'
BINARY = 'workbuddy2api'


def sha(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for block in iter(lambda: f.read(1 << 20), b''):
            h.update(block)
    return h.hexdigest()


def run(*args, timeout=90):
    return subprocess.check_output(args, text=True, timeout=timeout).strip()


def save(path, value):
    # Exclusive files prevent a repeat command from replacing recovery evidence.
    with open(path, 'x', encoding='utf8') as f:
        os.chmod(path, 0o600)
        json.dump(value, f, ensure_ascii=False, indent=2)
        f.write('\n')
        f.flush()
        os.fsync(f.fileno())


def config(base):
    return json.loads((base / 'config.json').read_text())


def http(base, path, data=None, key=None):
    cfg = config(base)
    addr = cfg['listen']
    if addr.startswith(':'):
        addr = '127.0.0.1' + addr
    if key is None:
        key = cfg.get('panel', {}).get('api_key') or cfg.get('api_key', '')
    headers = {'Authorization': 'Bearer ' + key}
    if data is not None:
        headers['Content-Type'] = 'application/json'
        data = json.dumps(data, ensure_ascii=False).encode()
    req = urllib.request.Request('http://' + addr + path, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=20) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as e:
        # Do not put response bodies or auth data into deployment logs.
        return e.code, None


def probe(base, expected_version):
    if run('systemctl', 'is-active', SERVICE) != 'active':
        raise RuntimeError('service is not active')
    status, overview = http(base, '/panel/api/overview')
    if status != 200 or overview.get('version') != expected_version:
        raise RuntimeError('overview health/version mismatch')
    status, keys = http(base, '/panel/api/keys')
    if status != 200 or not isinstance(keys.get('keys'), list):
        raise RuntimeError('key counts unavailable')
    for key in keys['keys']:
        status, view = http(base, '/panel/api/keys/' + key['id'] + '/prompts?limit=1&role=fresh')
        if status != 200 or not isinstance(view.get('entries'), list):
            raise RuntimeError('archive view unavailable')
    status, _ = http(base, '/panel/api/keys/' + keys['keys'][0]['id'] + '/prompts', key='') if keys['keys'] else (401, None)
    if status != 401:
        raise RuntimeError('archive authentication check failed')
    return {'version': expected_version, 'keys_checked': len(keys['keys']), 'accounts': overview.get('total')}


def wait_probe(base, version):
    deadline = time.monotonic() + 60
    while True:
        try:
            return probe(base, version)
        except Exception:
            if time.monotonic() >= deadline:
                raise
            time.sleep(1)


def archive_copy(base, dest):
    src = base / 'data' / 'prompt-logs'
    cfg = config(base)
    state = Path(cfg.get('state_file', './data/state.json'))
    if not state.is_absolute():
        state = base / state
    src = state.parent / 'prompt-logs'
    dest.mkdir(mode=0o700)
    rows = total = 0
    manifest = []
    for path in sorted(src.glob('prompts-*.jsonl')):
        if path.is_symlink() or not path.is_file():
            raise RuntimeError('archive path is not a regular file')
        with path.open('rb') as f, (dest / path.name).open('xb') as out:
            size = os.fstat(f.fileno()).st_size
            os.chmod(dest / path.name, 0o600)
            remaining = size
            while remaining:
                block = f.read(min(1 << 20, remaining))
                if not block:
                    raise RuntimeError('archive changed during snapshot')
                out.write(block)
                remaining -= len(block)
            out.flush()
            os.fsync(out.fileno())
        count = 0
        with (dest / path.name).open('rb') as f:
            for line in f:
                json.loads(line)
                count += 1
        manifest.append({'file': path.name, 'bytes': size, 'rows': count, 'sha256': sha(dest / path.name)})
        rows += count
        total += size
    save(dest / 'manifest.json', manifest)
    return {'files': len(manifest), 'rows': rows, 'bytes': total}


def install(source, target, mode, uid, gid):
    # Copy to a fresh inode: never truncate a running executable.
    fd, temporary = tempfile.mkstemp(prefix='.prompt-release-', dir=target.parent)
    try:
        with os.fdopen(fd, 'wb') as out, source.open('rb') as src:
            shutil.copyfileobj(src, out)
            os.fchmod(out.fileno(), mode)
            os.fchown(out.fileno(), uid, gid)
            out.flush()
            os.fsync(out.fileno())
        os.replace(temporary, target)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def prepare(args):
    base, release = args.base, args.release
    if any((release / name).exists() for name in ['manifest.json', 'workbuddy2api.before', 'config.before.json', 'archives-before']):
        raise RuntimeError('release preparation exists; use a fresh release directory')
    if shutil.disk_usage(base).free < 10 << 30:
        raise RuntimeError('insufficient disk headroom (10 GiB minimum)')
    if sha(release / 'workbuddy2api.next') != args.sha256:
        raise RuntimeError('staged binary checksum mismatch')
    pid = run('systemctl', 'show', SERVICE, '-p', 'MainPID', '--value')
    target = base / BINARY
    old_hash = sha(target)
    if sha(Path('/proc') / pid / 'exe') != old_hash:
        raise RuntimeError('on-disk binary differs from running process')
    status, overview = http(base, '/panel/api/overview')
    if status != 200:
        raise RuntimeError('baseline authentication/health failed')
    baseline = probe(base, overview['version'])
    info = target.stat()
    cfg_hash = sha(base / 'config.json')
    shutil.copy2(target, release / 'workbuddy2api.before')
    shutil.copy2(base / 'config.json', release / 'config.before.json')
    os.chmod(release / 'config.before.json', 0o600)
    archives = archive_copy(base, release / 'archives-before')
    if sha(base / 'config.json') != cfg_hash or sha(target) != old_hash:
        raise RuntimeError('configuration or binary changed during preparation')
    manifest = dict(version=args.version, new_sha256=args.sha256, old_sha256=old_hash,
                    config_sha256=cfg_hash, old_version=overview['version'], pid=pid,
                    mode=info.st_mode & 0o777, uid=info.st_uid, gid=info.st_gid,
                    recovery_unit=args.recovery_unit, deploy_unit=args.deploy_unit,
                    baseline=baseline, backup=archives, utc=dt.datetime.now(dt.timezone.utc).isoformat())
    save(release / 'manifest.json', manifest)
    print(json.dumps({'prepared': True, 'baseline': baseline, 'backup': archives}))


def verify_backup(args, manifest):
    if sha(args.release / 'workbuddy2api.before') != manifest['old_sha256']:
        raise RuntimeError('recovery binary integrity check failed')
    if sha(args.release / 'config.before.json') != manifest['config_sha256']:
        raise RuntimeError('configuration backup integrity check failed')
    for entry in json.loads((args.release / 'archives-before' / 'manifest.json').read_text()):
        if sha(args.release / 'archives-before' / entry['file']) != entry['sha256']:
            raise RuntimeError('archive backup integrity check failed')


def recover(args, manifest, stop_job=True):
    if (args.release / 'success.json').exists():
        print('release already healthy; recovery is a no-op')
        return
    if not (args.release / 'started.json').exists():
        print('activation never started; prior service remains untouched')
        return
    verify_backup(args, manifest)
    if stop_job:
        run('systemctl', 'stop', manifest['deploy_unit'] + '.service', timeout=30)
    install(args.release / 'workbuddy2api.before', args.base / BINARY, manifest['mode'], manifest['uid'], manifest['gid'])
    run('systemctl', 'restart', SERVICE)
    health = wait_probe(args.base, manifest['old_version'])
    if not (args.release / 'recovered.json').exists():
        save(args.release / 'recovered.json', {'health': health, 'configuration_preserved': True, 'archives_preserved': True})
    print(json.dumps({'recovered': True, 'health': health}))


def verify_capture(base, release):
    cfg = config(base)
    candidates = [k for k in cfg.get('api_keys', []) if k.get('capture_prompts') and not k.get('disabled') and k.get('models') and not k.get('daily_tokens')]
    if not candidates:
        raise RuntimeError('no restricted capture key available for safe local verification')
    _, key_data = http(base, '/panel/api/keys')
    counts = {k['id']: k.get('prompt_count', 0) for k in key_data['keys']}
    key = min(candidates, key=lambda k: counts.get(k['id'], 0))
    model = '__prompt_monitor_selftest__'
    if model in key['models']:
        raise RuntimeError('self-test model unexpectedly permitted')
    user = {'role': 'user', 'content': '提示词监控上线自检（合成数据）'}
    messages = [user]
    for i in range(2):
        if i:
            messages.append({'role': 'user', 'content': '继续'})
        status, _ = http(base, '/v1/chat/completions', {'model': model, 'prompt_cache_key': release.name, 'messages': messages}, key=key['key'])
        if status != 403:
            raise RuntimeError('self-test did not stop at model whitelist')
    status, data = http(base, '/panel/api/keys/' + key['id'] + '/prompts?limit=50&role=fresh')
    digest = hashlib.sha256(release.name.encode()).hexdigest()[:32]
    entries = [e for e in (data or {}).get('entries', []) if e.get('session_hash') == digest]
    if status != 200 or len(entries) != 2:
        raise RuntimeError('self-test captures unavailable')
    entries.sort(key=lambda e: (e['time'], e['request_id']))
    first, last = entries
    if first.get('fresh_state') != 'unknown' or last.get('fresh_state') != 'new' or last.get('messages') != [{'role':'user', 'content':'继续'}]:
        raise RuntimeError('self-test turn classification failed')
    if any(e.get('version') != 2 or not e.get('inputs') for e in entries):
        raise RuntimeError('self-test metadata missing')
    return {'requests': len(entries), 'statuses': [e['status'] for e in entries], 'fresh_states': [e['fresh_state'] for e in entries], 'upstream_calls': 0}


def activate(args, manifest):
    verify_backup(args, manifest)
    if sha(args.base / 'config.json') != manifest['config_sha256'] or sha(args.base / BINARY) != manifest['old_sha256']:
        raise RuntimeError('live config/binary changed; refresh preparation before activation')
    if run('systemctl', 'is-active', manifest['recovery_unit'] + '.timer') != 'active':
        raise RuntimeError('independent recovery timer is not armed')
    if sha(args.release / 'workbuddy2api.next') != manifest['new_sha256']:
        raise RuntimeError('staged binary changed after preparation')
    save(args.release / 'started.json', {'utc': dt.datetime.now(dt.timezone.utc).isoformat()})
    try:
        run('systemctl', 'stop', SERVICE)
        archive_copy(args.base, args.release / 'archives-at-stop')
        install(args.release / 'workbuddy2api.next', args.base / BINARY, manifest['mode'], manifest['uid'], manifest['gid'])
        run('systemctl', 'start', SERVICE)
        health = wait_probe(args.base, manifest['version'])
        captured = verify_capture(args.base, args.release)
        save(args.release / 'success.json', {'health': health, 'capture': captured, 'binary_sha256': sha(args.base / BINARY), 'config_sha256': sha(args.base / 'config.json')})
    except Exception:
        recover(args, manifest, stop_job=False)
        raise
    run('systemctl', 'stop', manifest['recovery_unit'] + '.timer', timeout=10)
    print(json.dumps({'activated': True, 'health': health, 'capture': captured}))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('action', choices=['prepare', 'check-recovery', 'activate', 'recover', 'probe'])
    parser.add_argument('--base', type=Path, required=True)
    parser.add_argument('--release', type=Path, required=True)
    parser.add_argument('--sha256')
    parser.add_argument('--version', default='1.13.1-panel')
    parser.add_argument('--recovery-unit')
    parser.add_argument('--deploy-unit')
    args = parser.parse_args()
    args.base, args.release = args.base.resolve(), args.release.resolve()
    if not args.release.is_relative_to(args.base / 'backups') or not args.release.is_dir():
        raise RuntimeError('release directory must exist inside service backups')
    os.chmod(args.release, 0o700)
    if args.action == 'prepare':
        return prepare(args)
    manifest = json.loads((args.release / 'manifest.json').read_text())
    if args.action == 'probe':
        print(json.dumps(probe(args.base, args.version)))
    elif args.action == 'check-recovery':
        verify_backup(args, manifest)
        print(json.dumps({'recovery_ready': True, 'baseline': probe(args.base, manifest['old_version'])}))
    elif args.action == 'activate':
        activate(args, manifest)
    else:
        recover(args, manifest)


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        print(type(exc).__name__ + ': ' + str(exc), file=sys.stderr)
        sys.exit(1)
