#!/bin/sh
# Copyright 2026 Кислов Роман Сергеевич
# SPDX-License-Identifier: Apache-2.0
set -eu
cd "$(dirname "$0")/.."

if [ "$#" -eq 0 ]; then
    set -- alt-p11 redos8 astra18 ubuntu2404 centos9
fi
if [ -n "${SRK_PACKAGE_IMAGE:-}" ] && [ "$#" -ne 1 ]; then
    printf '%s\n' 'SRK_PACKAGE_IMAGE requires exactly one target' >&2
    exit 2
fi
targets=
for target do
    case "$target" in
        alt-p11|redos8|astra18|ubuntu2404|centos9) ;;
        *) printf 'Unknown target: %s\n' "$target" >&2; exit 2 ;;
    esac
    case " $targets " in
        *" $target "*) printf 'Duplicate target: %s\n' "$target" >&2; exit 2 ;;
    esac
    targets="$targets $target"
done

version=$(sed -n 's/^Version: *//p' packaging/altlinux/srk.spec)
app_version=$(sed -n 's/^const Version = "\([^"]*\)"$/\1/p' internal/model/model.go)
if [ -z "$version" ] || [ "$version" != "$app_version" ]; then
    printf 'Package version (%s) differs from application version (%s)\n' "$version" "$app_version" >&2
    exit 1
fi

mkdir -p dist/linux-packages
out=$(cd dist/linux-packages && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
trap 'exit 1' HUP INT TERM
bundle="gdata-srk-$version"
mkdir "$stage/$bundle" "$stage/out"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$stage/$bundle/srk" ./cmd/srk
cp README.md LICENSE NOTICE "$stage/$bundle/"
cp -R docs third_party "$stage/$bundle/"
sed 's|/usr/local/bin/srk|/usr/bin/srk|' packaging/systemd/srk@.service > "$stage/$bundle/srk@.service"
COPYFILE_DISABLE=1 tar -czf "$stage/$bundle.tar.gz" -C "$stage" "$bundle"
cp packaging/altlinux/srk.spec packaging/build-linux-package.sh "$stage/"
cp -R packaging/deb "$stage/"
chmod -R a+rX "$stage"

for target do
    case "$target" in
        alt-p11) image=alt:p11@sha256:fd4ada6a0ed0271b5220a00fb1318523143851a217296b50430bdeafff443332 ;;
        redos8) image=registry.red-soft.ru/ubi8/ubi:latest ;;
        astra18) image=registry.astralinux.ru/library/astra/ubi18:latest ;;
        ubuntu2404) image=ubuntu:24.04 ;;
        centos9) image=quay.io/centos/centos:stream9 ;;
    esac
    printf 'Building and checking %s\n' "$target"
    docker run --rm --platform linux/amd64 --user 0:0 -v "$stage:/input:ro" -v "$stage/out:/out" \
        "${SRK_PACKAGE_IMAGE:-$image}" /bin/sh /input/build-linux-package.sh "$target" "$version"
done

for package in "$stage/out/"*; do
    cp "$package" "$out/"
done
if [ "$#" -eq 5 ]; then
    if command -v sha256sum >/dev/null 2>&1; then
        (cd "$stage/out" && sha256sum -- *) > "$stage/SHA256SUMS-linux-packages"
    else
        (cd "$stage/out" && shasum -a 256 -- *) > "$stage/SHA256SUMS-linux-packages"
    fi
    cp "$stage/SHA256SUMS-linux-packages" "$out/SHA256SUMS-linux-packages"
fi
printf 'Packages: %s\n' "$out"
