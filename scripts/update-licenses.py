#!/usr/bin/env python3
# Copyright 2026 Кислов Роман Сергеевич
# SPDX-License-Identifier: Apache-2.0
"""Record pinned development dependencies and preserve their license notices."""
import json
import shutil
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
OUTPUT = ROOT / "third_party" / "licenses"
OUTPUT.mkdir(parents=True, exist_ok=True)


def preserve(directory, target):
    names = []
    for source in Path(directory).iterdir():
        if source.is_file() and source.name.upper().startswith(
            ("LICENSE", "COPYING", "NOTICE", "PATENTS")
        ):
            target.mkdir(exist_ok=True)
            shutil.copyfile(source, target / source.name)
            names.append(source.name)
    return sorted(names)


records = []
remaining = subprocess.check_output(
    ["go", "list", "-m", "-json", "all"], cwd=ROOT, text=True
)
decoder = json.JSONDecoder()
while remaining.strip():
    remaining = remaining.lstrip()
    module, end = decoder.raw_decode(remaining)
    remaining = remaining[end:]
    if module.get("Main") or not module.get("Dir"):
        continue
    name = module["Path"]
    version = module.get("Version", "")
    target = OUTPUT / ("go_" + name.replace("/", "_") + "_" + version)
    records.append(
        {"ecosystem": "go", "name": name, "version": version,
         "license_files": preserve(module["Dir"], target)}
    )

modules = ROOT / "node_modules"
packages = sorted(list(modules.glob("*/package.json")) + list(modules.glob("@*/*/package.json")))
for package in packages:
    metadata = json.loads(package.read_text())
    target = OUTPUT / ("npm_" + metadata["name"].replace("/", "_") + "_" + metadata["version"])
    records.append(
        {"ecosystem": "npm", "name": metadata["name"], "version": metadata["version"],
         "license": metadata.get("license"), "license_files": preserve(package.parent, target)}
    )

(ROOT / "third_party" / "dependencies.json").write_text(
    json.dumps(records, ensure_ascii=False, indent=2) + "\n"
)
print(f"Recorded {len(records)} dependencies")
