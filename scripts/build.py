#!/usr/bin/env python3
"""Reproducible, CGO-free release artifacts from Linux, Windows, or macOS."""
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]
DIST = ROOT / 'dist'
GO = os.environ.get('GO', 'go')
VERSION = '0.2.6'
TARGETS = [('windows', 'amd64'), ('linux', 'arm64'), ('linux', 'amd64')]
DIST.mkdir(exist_ok=True)
artifacts = []
for system, arch in TARGETS:
    folder = DIST / f'ProjectReplay-{VERSION}-{system}-{arch}'
    folder.mkdir(exist_ok=True)
    executable = 'ProjectReplay.exe' if system == 'windows' else 'project-replay'
    env = dict(os.environ, GOOS=system, GOARCH=arch, CGO_ENABLED='0')
    subprocess.run([GO, 'build', '-buildvcs=false', '-trimpath', '-ldflags=-s -w', '-o', str(folder / executable), './cmd/replay'], cwd=ROOT, env=env, check=True)
    source_exe = 'replay-source.exe' if system == 'windows' else 'replay-source'
    subprocess.run([GO, 'build', '-buildvcs=false', '-trimpath', '-ldflags=-s -w', '-o', str(folder / source_exe), './cmd/replay-source'], cwd=ROOT, env=env, check=True)
    shutil.copytree(ROOT / 'third_party_licenses', folder / 'third_party_licenses', dirs_exist_ok=True)
    (folder / 'docs').mkdir(exist_ok=True)
    for name in ['云中继部署与配对.md', 'Windows网页回放交付说明.md', '道具击杀追踪.md', '原生自动追雷交付与验证.md', '道具追踪开源实现调研.md', '本地Demo双路联调.md', '自动录制流程与后台渲染.md', '0.1.1修复与升级说明.md']:
        shutil.copy2(ROOT / 'docs' / name, folder / 'docs' / name)
    shutil.copytree(ROOT / 'configs', folder / 'configs', dirs_exist_ok=True)
    shutil.copy2(ROOT / 'README.md', folder / 'README.md')
    shutil.copy2(ROOT / 'LICENSE', folder / 'LICENSE')
    shutil.copy2(ROOT / 'THIRD_PARTY_NOTICES.md', folder / 'THIRD_PARTY_NOTICES.md')
    if system == 'windows':
        (folder / 'Start-Director.cmd').write_bytes(b'@echo off\r\ncd /d "%~dp0"\r\nProjectReplay.exe -role director\r\npause\r\n')
        archive = DIST / f'{folder.name}-portable.zip'
        with zipfile.ZipFile(archive, 'w', zipfile.ZIP_DEFLATED) as output:
            for path in sorted(folder.rglob('*')):
                if path.is_file() and 'replay-data' not in path.relative_to(folder).parts: output.write(path, str(Path(folder.name) / path.relative_to(folder)))
        shutil.copy2(folder / executable, DIST / 'ProjectReplay-windows-x64-portable.exe')
    else:
        launcher = folder / 'start-agent.sh'
        launcher.write_text('#!/bin/sh\nset -eu\ncd "$(dirname "$0")"\nexec ./project-replay -role agent -listen 0.0.0.0:7788 "$@"\n')
        launcher.chmod(0o755)
        headless = folder / 'start-agent-headless.sh'
        headless.write_text('#!/bin/sh\nset -eu\ncd "$(dirname "$0")"\nexec ./start-agent.sh -no-browser "$@"\n')
        headless.chmod(0o755)
        (folder / executable).chmod(0o755)
        archive = DIST / f'{folder.name}.tar.gz'
        with tarfile.open(archive, 'w:gz') as output:
            output.add(folder, arcname=folder.name, filter=lambda info: None if 'replay-data' in Path(info.name).parts else info)
    artifacts.append(archive)
    if system == 'windows': artifacts.append(DIST / 'ProjectReplay-windows-x64-portable.exe')
    print(f'Built {archive.name}', flush=True)
    if system == 'linux':
        relay_folder = DIST / f'ProjectReplay-Relay-{VERSION}-{system}-{arch}'
        relay_folder.mkdir(exist_ok=True)
        subprocess.run([GO, 'build', '-buildvcs=false', '-trimpath', '-ldflags=-s -w', '-o', str(relay_folder / 'replay-relay'), './cmd/replay-relay'], cwd=ROOT, env=env, check=True)
        shutil.copy2(ROOT / 'docs' / '云中继部署与配对.md', relay_folder / 'README.md')
        shutil.copytree(ROOT / 'configs' / 'relay', relay_folder / 'configs', dirs_exist_ok=True)
        shutil.copy2(ROOT / 'LICENSE', relay_folder / 'LICENSE')
        shutil.copy2(ROOT / 'third_party_licenses' / 'github.com_gorilla_websocket@v1.5.3' / 'LICENSE', relay_folder / 'WEBSOCKET_LICENSE')
        relay_archive = DIST / f'{relay_folder.name}.tar.gz'
        with tarfile.open(relay_archive, 'w:gz') as output:
            output.add(relay_folder, arcname=relay_folder.name)
        artifacts.append(relay_archive)
        print(f'Built {relay_folder.name}.tar.gz', flush=True)
checksums = []
for path in sorted(artifacts):
    if path.is_file() and path.suffix in ('.gz', '.zip', '.exe'):
        checksums.append(f'{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}')
(DIST / 'SHA256SUMS').write_text('\n'.join(checksums) + '\n')
