#!/bin/sh
# Copyright 2026 Кислов Роман Сергеевич
# SPDX-License-Identifier: Apache-2.0
set -eu
target=$1
version=$2
bundle="gdata-srk-$version"
export DEBIAN_FRONTEND=noninteractive
. /etc/os-release

case "$target" in
    alt-p11)
        [ "$ID" = altlinux ]
        [ "${ALT_BRANCH_ID:-}" = p11 ]
        release=alt1.p11
        if ! command -v rpmbuild >/dev/null 2>&1; then
            apt-get update
            apt-get install -y rpm-build
        fi
        id srkbuild >/dev/null 2>&1 || useradd -m srkbuild
        topdir=/home/srkbuild/RPM
        ;;
    redos8|centos9)
        case "$target" in
            redos8)
                [ "$ID" = redos ]
                [ "${VERSION_ID%%.*}" = 8 ]
                release=1.redos8
                ;;
            centos9)
                [ "$ID" = centos ]
                [ "$VERSION_ID" = 9 ]
                release=1.el9
                ;;
        esac
        if ! command -v rpmbuild >/dev/null 2>&1; then
            dnf -y install rpm-build
        fi
        topdir=/tmp/srk-rpm
        ;;
    astra18|ubuntu2404)
        case "$target" in
            astra18)
                [ "$ID" = astra ]
                [ "${VERSION_ID%%_*}" = 1.8 ]
                distro=astra1.8
                ;;
            ubuntu2404)
                [ "$ID" = ubuntu ]
                [ "$VERSION_ID" = 24.04 ]
                distro=ubuntu24.04
                ;;
        esac
        apt-get update
        ;;
esac

case "$target" in
    alt-p11|redos8|centos9)
        mkdir -p "$topdir/SOURCES" "$topdir/SPECS"
        cp "/input/$bundle.tar.gz" "$topdir/SOURCES/"
        cp /input/srk.spec "$topdir/SPECS/"
        set -- rpmbuild --define "_topdir $topdir" --define "_unitdir /usr/lib/systemd/system" \
            --define "srk_release $release" -bb "$topdir/SPECS/srk.spec"
        if [ "$target" = alt-p11 ]; then
            chown -R srkbuild:srkbuild "$topdir"
            runuser -u srkbuild -- "$@"
        else
            "$@"
        fi
        package="gdata-srk-$version-$release.x86_64.rpm"
        artifact="$topdir/RPMS/x86_64/$package"
        if [ "$target" = alt-p11 ]; then
            # ALT APT resolves local RPM dependencies after updating its indexes.
            apt-get update
            apt-get install -y "$artifact"
        else
            dnf -y install "$artifact"
        fi
        rpm -q gdata-srk
        ;;
    astra18|ubuntu2404)
        debversion="$version-1~$distro"
        # GitHub normalizes tildes in release asset filenames.
        package="gdata-srk_${version}-1.${distro}_amd64.deb"
        artifact="/tmp/$package"
        root=/tmp/srk-deb
        mkdir -p "$root/DEBIAN" "$root/usr/share/doc/gdata-srk"
        cat > "$root/DEBIAN/control" <<CONTROL
Package: gdata-srk
Version: $debversion
Section: utils
Priority: optional
Architecture: amd64
Depends: adduser, systemd
Maintainer: Roman Kislov <roman@kislovs.ru>
Homepage: https://github.com/rkislov/gdata
Description: Distributed backup system with a web interface
 GData SRK manager, media server, agent and proxy. Includes a static Go
 binary and embedded web UI. Configure PostgreSQL and the services
 separately after installing the package.
CONTROL
        for script in postinst prerm postrm; do
            install -m 0755 "/input/deb/$script" "$root/DEBIAN/$script"
        done
        install -Dm755 "/input/$bundle/srk" "$root/usr/bin/srk"
        install -Dm644 "/input/$bundle/srk@.service" "$root/lib/systemd/system/srk@.service"
        for item in docs LICENSE NOTICE README.md third_party; do
            cp -R "/input/$bundle/$item" "$root/usr/share/doc/gdata-srk/"
        done
        dpkg-deb --root-owner-group -Zgzip --build "$root" "$artifact"
        apt-get install -y "$artifact"
        dpkg-query -W gdata-srk
        ;;
esac

srk version | grep -F "$version"
test "$(stat -c '%a %U %G' /etc/srk)" = '750 root srk'
for dir in /var/lib/srk /var/log/srk; do
    test "$(stat -c '%a %U %G' "$dir")" = '750 srk srk'
done
smoke=$(mktemp -d)
chown srk:srk "$smoke"
runuser -u srk -- srk dev init --dir "$smoke/config"
for role in manager media agent proxy; do
    runuser -u srk -- srk --config "$smoke/config/$role.json" "$role" check-config
done
cp "$artifact" "/out/$package"
