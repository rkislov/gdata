#!/usr/bin/env python3
# Copyright 2026 Кислов Роман Сергеевич
# SPDX-License-Identifier: Apache-2.0
"""Local-only end-to-end test: disposable QEMU disks, SRK Rescue ISO, real services.
No host block device is attached. One test backup remains in the local catalog.
Requires Go, Docker Desktop, the built rescue image and running dev services.
"""
import base64, fcntl, hashlib, http.cookiejar, json, os, pathlib, re, selectors, ssl, struct, subprocess, time, urllib.request, uuid, zlib
root = pathlib.Path(__file__).resolve().parents[1]
os.chdir(root)
work = root / '.work/v06-e2e'
work.mkdir(parents=True, exist_ok=True)
lock = (work/'test.lock').open('w')
fcntl.flock(lock, fcntl.LOCK_EX|fcntl.LOCK_NB)
build_env=os.environ.copy();build_env.update({'CGO_ENABLED':'0','GOOS':'linux','GOARCH':'amd64'})
subprocess.run(['go','build','-trimpath','-o',str(work/'proxy'),'./scripts/rescue-test-forwarder'],env=build_env,check=True)
size = 64*1024*1024
raw = bytearray(size)
boot = bytes.fromhex('fa31c08ed8be167cbae900ac84c07403eeebf8f4ebfd')
raw[:len(boot)] = boot
message=b'SRK_06_BOOT_OK\n\0'
raw[len(boot):len(boot)+len(message)]=message
raw[450] = 0xee
struct.pack_into('<II',raw,454,1,size//512-1)
raw[510:512] = b'\x55\xaa'
entries = bytearray(128*128)
entries[:16] = uuid.UUID('0fc63daf-8483-4772-8e79-3d69d8477de4').bytes_le
entries[16:32] = uuid.UUID('83aa4332-cc09-4333-a1dc-b51f63889977').bytes_le
struct.pack_into('<QQQ',entries,32,2048,8191,0)
name='SRK test partition'.encode('utf-16le');entries[56:56+len(name)]=name
last=size//512-1
disk_guid=uuid.UUID('92091822-3a5c-4e08-b4c3-d0136c150187').bytes_le
for current,backup,table in [(1,last,2),(last,1,last-32)]:
    header=bytearray(512)
    struct.pack_into('<8sIIIIQQQQ16sQIII',header,0,b'EFI PART',0x10000,92,0,0,current,backup,34,last-33,disk_guid,table,128,128,zlib.crc32(entries))
    struct.pack_into('<I',header,16,zlib.crc32(header[:92]))
    raw[current*512:(current+1)*512]=header
    raw[table*512:table*512+len(entries)]=entries
source=work/'source.img';target=work/'target.img'
source.write_bytes(raw)
with target.open('wb') as f:f.truncate(size)
expected=hashlib.sha256(raw).hexdigest()
# TLS trust bypass is restricted to this host-side development test. The actual
# rescue client validates certificate pins on both manager and media connections.
base='https://localhost:8443'
cookies=http.cookiejar.CookieJar()
http=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookies),urllib.request.HTTPSHandler(context=ssl._create_unverified_context()))
def api(path,body=None,method=None):
    req=urllib.request.Request(base+path,None if body is None else json.dumps(body).encode(),headers={'Content-Type':'application/json','Origin':base},method=method or ('POST' if body is not None else 'GET'))
    with http.open(req,timeout=30) as response:return json.load(response)
api('/v1/login',{'password':(root/'.local/admin.password').read_text().strip()})
nodes=api('/v1/nodes')
media=next(n for n in nodes if n['role']=='media' and n['status'].get('rescue_capture_enabled'))
issued=api('/v1/rescue/codes',{'operation':'backup','name':'Тест SRK Rescue 0.6 · GPT и загрузчик','media_id':media['id'],'retention_days':7,'max_bytes':size})
command='/test/proxy & exec qemu-system-x86_64 -accel tcg -m 2048 -nographic -no-reboot -boot order=d -cdrom /out/srk-rescue-amd64.iso -drive file=/test/source.img,format=raw,if=none,id=source,readonly=on -device virtio-blk-pci,drive=source,serial=SRK06SOURCE -drive file=/test/target.img,format=raw,if=none,id=target -device virtio-blk-pci,drive=target,serial=SRK06TARGET -nic user,model=virtio-net-pci'
p=subprocess.Popen(['docker','run','--rm','-i','--platform','linux/amd64','--name','srk-disk-e2e','-v',str(work)+':/test','srk-rescue-builder:local','sh','-c',command],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
selector=selectors.DefaultSelector();selector.register(p.stdout,selectors.EVENT_READ)
log=bytearray();cursor=0
secrets=[issued['code']]
def until(needle,timeout=120):
    global cursor
    deadline=time.monotonic()+timeout
    while time.monotonic()<deadline:
        index=log.find(needle,cursor)
        if index>=0:cursor=index+len(needle);return
        for key,_ in selector.select(1):
            part=os.read(key.fd,65536)
            if not part:raise RuntimeError('VM exited before '+repr(needle))
            log.extend(part)
    raise TimeoutError('waiting for '+repr(needle))
def send(text):p.stdin.write(text.encode());p.stdin.flush()
try:
    until(b'Serial console');send('s');until(b'0 Exit')
    send('5\n');until(b'/ # ')
    send("printf '10.0.2.2 localhost\\n' > /etc/hosts\n");until(b'/ # ')
    send('exit\n');until(b'0 Exit')
    send('2\n');until(b'(hidden): ');send(issued['code']+'\n');until(b'Connected to');until(b'0 Exit')
    send('7\n');until(b'Number (0 = cancel): ');send('1\n');until(b'Type exactly [BACKUP /dev/vda SRK06SOURCE]: ');send('BACKUP /dev/vda SRK06SOURCE\n')
    until(b'Backup verified. Run ID:',180);until(b'The administrator')
    match=re.search(rb'Backup verified\. Run ID:\s*([a-f0-9]{32})',log)
    if not match:raise RuntimeError('missing backup ID')
    run_id=match[1].decode();(work/'run.json').write_text(json.dumps({'run_id':run_id}))
    run=api('/v1/runs/'+run_id)
    assert run['status']=='success' and run['result']['verified'] and run['result']['logical_bytes']==size
    assert 0<=run['result']['uploaded_bytes']<size
    events=api('/v1/runs/'+run_id+'/events');assert any(e['action']=='capture.progress' for e in events)
    restore=api('/v1/rescue/codes',{'run_ids':[run_id]});secrets.append(restore['code'])
    until(b'0 Exit');send('2\n');until(b'(hidden): ');send(restore['code']+'\n');until(b'Connected to');until(b'0 Exit')
    send('3\n');until(b'Number (0 = cancel): ');send('1\n');until(b'Number (0 = cancel): ');send('1\n');until(b'Type exactly [ERASE /dev/vdb SRK06TARGET]: ');send('ERASE /dev/vdb SRK06TARGET\n')
    until(b'Restore and complete disk read-back verified',180)
    until(b'0 Exit');send('6\n');until(b'Type REBOOT: ');send('REBOOT\n');p.wait(timeout=30)
    assert hashlib.sha256(source.read_bytes()).hexdigest()==expected,'source disk was modified'
    assert hashlib.sha256(target.read_bytes()).hexdigest()==expected,'restored disk differs'
    api('/v1/rescue/codes/'+restore['id'],method='DELETE')
    report={'backup_run_id':run_id,'bytes':size,'uploaded_bytes':run['result']['uploaded_bytes'],'reused_bytes':size-run['result']['uploaded_bytes'],'whole_disk_sha256':expected,'source_unchanged':True,'restored_identical':True,'events':len(events)}
    (work/'report.json').write_text(json.dumps(report,indent=2));print(json.dumps(report))
finally:
    # Redact codes defensively even if the terminal failed to suppress echo.
    redacted=bytes(log)
    for secret in secrets:redacted=redacted.replace(secret.encode(),b'[ONE_TIME_CODE_REDACTED]')
    (work/'vm.log').write_bytes(redacted)
    try:api('/v1/rescue/codes/'+issued['id'],method='DELETE')
    except Exception:pass
    if p.poll() is None:
        subprocess.run(['docker','stop','-t','1','srk-disk-e2e'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL);p.terminate()
