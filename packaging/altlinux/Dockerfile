# Builder and test dependencies for ALT Linux 10 x86_64.
FROM alt:p10@sha256:5edc92edf6bc866824427f05834ffff3f815974fc1cd9840e0dda2492d11ce30
# Use an official mirror; only the native architecture and noarch are needed.
RUN sed -i 's|http://ftp.altlinux.org/pub/distributions/ALTLinux|https://mirror.yandex.ru/altlinux|; /x86_64-i586/d' /etc/apt/sources.list.d/alt.list \
    && apt-get -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 update \
    && apt-get -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 install -y rpm-build systemd
