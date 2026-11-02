#!/usr/bin/env python3
# Copyright 2026 Кислов Роман Сергеевич
# SPDX-License-Identifier: Apache-2.0
"""Boot the locally built ISO in QEMU; never attaches host block devices."""
import os, selectors, subprocess, sys, time
mode = sys.argv[1] if len(sys.argv) > 1 else 'bios'
if mode not in ('bios', 'uefi'):
    raise SystemExit('bios or uefi required')
command = 'truncate -s 64M /tmp/target.raw; exec qemu-system-x86_64 -accel tcg -m 2048 -nographic -no-reboot -boot order=d -cdrom /out/srk-rescue-amd64.iso -drive file=/tmp/target.raw,format=raw,if=virtio -nic user,model=virtio-net-pci'
if mode == 'uefi':
    command += ' -bios /usr/share/OVMF/OVMF_CODE.fd'
p = subprocess.Popen(['docker', 'run', '--rm', '-i', '--platform', 'linux/amd64', '--name', 'srk-smoke-'+mode, 'srk-rescue-builder:local', 'sh', '-c', command], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
selector = selectors.DefaultSelector()
selector.register(p.stdout, selectors.EVENT_READ)
log = bytearray()
cursor = 0
def until(needle, timeout=100):
    global cursor
    deadline = time.monotonic()+timeout
    while time.monotonic() < deadline:
        index = log.find(needle, cursor)
        if index >= 0:
            cursor = index + len(needle)
            return
        for key, _ in selector.select(1):
            chunk = os.read(key.fd, 65536)
            if not chunk:
                raise RuntimeError('QEMU exited before '+repr(needle))
            log.extend(chunk)
    raise TimeoutError('waiting for '+repr(needle))
def send(s):
    p.stdin.write(s.encode()); p.stdin.flush()
try:
    until(b'Serial console')
    send('s')
    until(b'0 Exit')
    send('5\n')
    until(b'/ # ')
    send('lsblk -J -b -o NAME,PATH,TYPE,SIZE,MODEL,SERIAL,RO,RM,TRAN,FSTYPE,MOUNTPOINTS,LOG-SEC\n')
    until(b'"log-sec": 512')
    until(b'/ # ')
    send('ip -4 -brief address\n')
    until(b'10.0.2.15')
    until(b'/ # ')
    send('test -d /proc/1 && test -d /sys/class/block/vda && /usr/local/bin/srk-rescue --version\n')
    until(b'SRK Rescue 0.6.0')
    until(b'/ # ')
    send('poweroff -f\n')
    p.wait(timeout=30)
    print(mode.upper()+': menu, virtual disk, 512-byte sector, DHCP, /proc, /sys and version OK')
finally:
    os.makedirs('.work', exist_ok=True)
    with open('.work/rescue-'+mode+'.log','wb') as f: f.write(log)
    if p.poll() is None:
        subprocess.run(['docker','stop','-t','1','srk-smoke-'+mode], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        p.terminate()
