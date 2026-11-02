# Copyright 2026 Кислов Роман Сергеевич
# SPDX-License-Identifier: Apache-2.0
FROM --platform=linux/amd64 alpine:3.22 AS rootfs
RUN apk add --no-cache linux-firmware-none && apk add --no-cache alpine-base linux-lts util-linux e2fsprogs dosfstools iproute2 ca-certificates cpio gzip
RUN apk add --no-cache kbd kbd-misc
COPY bin/srk-rescue-linux-amd64 /usr/local/bin/srk-rescue
COPY packaging/rescue/init /init
COPY LICENSE NOTICE /usr/share/licenses/srk/
RUN chmod 755 /init && mkdir -p /image && cp /boot/vmlinuz-lts /image/vmlinuz && apk info -vv > /image/packages.txt && rm -rf /boot /var/cache/apk/* && find / -xdev -path /proc -prune -o -path /sys -prune -o -path /dev -prune -o -path /image -prune -o -print | sed 's,^/,,' | grep -v '^$' | cpio -o -H newc | gzip -1 > /image/initramfs.gz
FROM --platform=linux/amd64 debian:bookworm-slim AS iso
RUN apt-get update && apt-get install -y --no-install-recommends grub-pc-bin grub-efi-amd64-bin grub-common xorriso mtools qemu-system-x86 ovmf && rm -rf /var/lib/apt/lists/*
COPY --from=rootfs /image /disk/boot
COPY packaging/rescue/grub.cfg /disk/boot/grub/grub.cfg
COPY LICENSE NOTICE /disk/
RUN mkdir -p /out /disk/boot/grub/fonts && cp /usr/share/grub/unicode.pf2 /disk/boot/grub/fonts/unicode.pf2 && grub-mkrescue -o /out/srk-rescue-amd64.iso /disk && sha256sum /out/srk-rescue-amd64.iso > /out/srk-rescue-amd64.iso.sha256 && cp /disk/boot/packages.txt /out/packages.txt
