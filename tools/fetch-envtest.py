#!/usr/bin/env python3
"""Fetch checksum-pinned local Kubernetes API test binaries, without a cluster."""

import argparse
import hashlib
import os
from pathlib import Path, PurePosixPath
import platform
import shutil
import sys
import tarfile
import tempfile
import urllib.request

URL = "https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-v1.35.0/envtest-v1.35.0-linux-amd64.tar.gz"
# From controller-tools/envtest-releases.yaml, release v1.35.0.
SHA512 = "130369c16f076e724d089189afaede960316f5f5dea6cf57be7a4fc6f09c77342893192509790e4056e116e232dff832ed863f5bd55dcb55d38f3ab834828a11"


def fetch(destination):
    if platform.system() != "Linux" or platform.machine() != "x86_64":
        raise ValueError("this pinned test asset supports Linux amd64")
    if destination.exists():
        raise ValueError("destination already exists")
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="envtest-", dir=destination.parent) as temporary:
        staging = Path(temporary)
        archive = staging / "assets.tar.gz"
        digest = hashlib.sha512()
        print("Downloading Kubernetes 1.35.0 API test assets", file=sys.stderr)
        with urllib.request.urlopen(URL, timeout=60) as response, archive.open("xb") as output:
            while chunk := response.read(1024 * 1024):
                digest.update(chunk)
                output.write(chunk)
        if digest.hexdigest() != SHA512:
            raise ValueError("test asset checksum mismatch")
        binaries = staging / "bin"
        binaries.mkdir()
        expected = {"kube-apiserver", "etcd", "kubectl"}
        with tarfile.open(archive, "r:gz") as source:
            for member in source.getmembers():
                name = PurePosixPath(member.name).name
                if name not in expected:
                    continue
                if not member.isfile() or (binaries / name).exists():
                    raise ValueError("unexpected archive entry")
                with source.extractfile(member) as stream, (binaries / name).open("xb") as output:
                    shutil.copyfileobj(stream, output)
                (binaries / name).chmod(0o755)
        if {p.name for p in binaries.iterdir()} != expected:
            raise ValueError("test asset is incomplete")
        os.rename(binaries, destination)
    print(destination.resolve())


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--destination", type=Path, required=True)
    args = parser.parse_args()
    try:
        fetch(args.destination)
    except (OSError, ValueError, tarfile.TarError) as error:
        parser.exit(1, f"Cannot fetch API test assets: {error}\n")
