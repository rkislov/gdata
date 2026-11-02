#!/bin/sh
# Copyright 2026 Кислов Роман Сергеевич
# SPDX-License-Identifier: Apache-2.0
set -eu
cd "$(dirname "$0")/.."
version=$(sed -n 's/^Version: *//p' packaging/altlinux/srk.spec)
# Official ALT p10 image, pinned for reproducible packaging environment.
image=${ALT_IMAGE:-alt:p10@sha256:5edc92edf6bc866824427f05834ffff3f815974fc1cd9840e0dda2492d11ce30}
mkdir -p dist
out=$(cd dist && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
bundle="gdata-srk-$version"
mkdir -p "$stage/$bundle"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$stage/$bundle/srk" ./cmd/srk
cp README.md LICENSE NOTICE "$stage/$bundle/"
cp -R docs third_party "$stage/$bundle/"
sed 's|/usr/local/bin/srk|/usr/bin/srk|' packaging/systemd/srk@.service > "$stage/$bundle/srk@.service"
COPYFILE_DISABLE=1 tar -czf "$out/gdata-srk-$version-alt10-x86_64.tar.gz" -C "$stage" "$bundle"
cp "$out/gdata-srk-$version-alt10-x86_64.tar.gz" "$stage/gdata-srk-$version.tar.gz"
cp packaging/altlinux/srk.spec "$stage/"
docker run --rm --platform linux/amd64 -v "$stage:/input:ro" -v "$out:/out" "$image" sh -ec '
  if ! command -v rpmbuild >/dev/null 2>&1; then
    apt-get update
    apt-get install -y rpm-build
  fi
  id srkbuild >/dev/null 2>&1 || useradd -m srkbuild
  mkdir -p /home/srkbuild/RPM/SOURCES /home/srkbuild/RPM/SPECS
  cp /input/*.tar.gz /home/srkbuild/RPM/SOURCES/
  cp /input/srk.spec /home/srkbuild/RPM/SPECS/
  chown -R srkbuild:srkbuild /home/srkbuild/RPM
  runuser -u srkbuild -- rpmbuild --define "_topdir /home/srkbuild/RPM" -bb /home/srkbuild/RPM/SPECS/srk.spec
  cp /home/srkbuild/RPM/RPMS/x86_64/*.rpm /out/
'
(cd "$out" && shasum -a 256 "gdata-srk-$version-alt10-x86_64.tar.gz" gdata-srk-"$version"-alt*.x86_64.rpm > SHA256SUMS)
printf 'ALT Linux 10 artifacts: %s\n' "$out"
