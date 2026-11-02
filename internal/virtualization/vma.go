// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package virtualization

import (
	"crypto/md5"
	"encoding/binary"
	"errors"
	"io"
	"math/bits"
	"os"
	"strings"
)

type VMADevice struct {
	Name  string
	Bytes int64
}

// VMA uses big-endian integers. MD5 is only the format's legacy header checksum;
// SRK additionally hashes every archive block and the entire archive with SHA-256.
func ValidateVMA(file string) (map[string]VMADevice, error) {
	f, e := os.Open(file)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	first := make([]byte, 60)
	if _, e = io.ReadFull(f, first); e != nil {
		return nil, e
	}
	if string(first[:4]) != "VMA\x00" || binary.BigEndian.Uint32(first[4:8]) != 1 {
		return nil, errors.New("unsupported VMA archive")
	}
	size := int64(binary.BigEndian.Uint32(first[56:60]))
	if size < 12288 || size > 16<<20 || size%512 != 0 {
		return nil, errors.New("invalid VMA header length")
	}
	header := make([]byte, size)
	copy(header, first)
	if _, e = io.ReadFull(f, header[60:]); e != nil {
		return nil, e
	}
	expected := string(header[32:48])
	clear(header[32:48])
	sum := md5.Sum(header)
	if string(sum[:]) != expected {
		return nil, errors.New("VMA header checksum mismatch")
	}
	offset := int64(binary.BigEndian.Uint32(header[48:52]))
	blobSize := int64(binary.BigEndian.Uint32(header[52:56]))
	if offset < 12288 || blobSize < 1 || offset+blobSize > size {
		return nil, errors.New("invalid VMA blob table")
	}
	devices := map[string]VMADevice{}
	byID := map[byte]VMADevice{}
	seen := map[byte][]byte{}
	counts := map[byte]uint64{}
	var total uint64
	for id := 1; id < 256; id++ {
		base := 4096 + id*32
		pointer := int64(binary.BigEndian.Uint32(header[base : base+4]))
		if pointer == 0 {
			continue
		}
		if pointer+2 > blobSize {
			return nil, errors.New("invalid VMA device name offset")
		}
		at := offset + pointer
		length := int64(binary.BigEndian.Uint16(header[at : at+2]))
		if length < 1 || pointer+2+length > blobSize {
			return nil, errors.New("invalid VMA device name")
		}
		name := strings.TrimRight(string(header[at+2:at+2+length]), "\x00")
		bytes := binary.BigEndian.Uint64(header[base+8 : base+16])
		if !strings.HasPrefix(name, "drive-") || bytes == 0 || bytes > 4<<40 || total > 4<<40-bytes {
			return nil, errors.New("unsupported VMA device")
		}
		if _, exists := devices[name]; exists {
			return nil, errors.New("duplicate VMA disk")
		}
		d := VMADevice{Name: name, Bytes: int64(bytes)}
		devices[name] = d
		byID[byte(id)] = d
		clusters := (bytes + 65535) / 65536
		seen[byte(id)] = make([]byte, (clusters+7)/8)
		total += bytes
	}
	if len(devices) == 0 || len(devices) > 32 {
		return nil, errors.New("VMA requires 1–32 complete disks")
	}
	extent := make([]byte, 512)
	for {
		_, e = io.ReadFull(f, extent)
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, errors.New("truncated VMA extent")
		}
		if string(extent[:4]) != "VMAE" || string(extent[8:24]) != string(header[8:24]) {
			return nil, errors.New("VMA extent identity mismatch")
		}
		digest := string(extent[24:40])
		clear(extent[24:40])
		sum := md5.Sum(extent)
		if string(sum[:]) != digest {
			return nil, errors.New("VMA extent checksum mismatch")
		}
		var blocks int
		for i := 0; i < 59; i++ {
			base := 40 + i*8
			mask := binary.BigEndian.Uint16(extent[base : base+2])
			id := extent[base+3]
			cluster := uint64(binary.BigEndian.Uint32(extent[base+4 : base+8]))
			if id == 0 {
				if mask != 0 {
					return nil, errors.New("VMA unused block has payload")
				}
				continue
			}
			d, ok := byID[id]
			if !ok || cluster >= (uint64(d.Bytes)+65535)/65536 {
				return nil, errors.New("VMA extent exceeds disk")
			}
			if seen[id][cluster/8]&(1<<(cluster%8)) != 0 {
				return nil, errors.New("duplicate VMA disk cluster")
			}
			seen[id][cluster/8] |= 1 << (cluster % 8)
			counts[id]++
			blocks += bits.OnesCount16(mask)
		}
		if blocks != int(binary.BigEndian.Uint16(extent[6:8])) {
			return nil, errors.New("VMA payload count mismatch")
		}
		if _, e = io.CopyN(io.Discard, f, int64(blocks)*4096); e != nil {
			return nil, errors.New("truncated VMA payload")
		}
	}
	for id, d := range byID {
		if counts[id] != (uint64(d.Bytes)+65535)/65536 {
			return nil, errors.New("VMA archive omits disk clusters")
		}
	}
	return devices, nil
}

// RestoreVMAHeader tags a fresh restore before PVE allocates its configuration,
// so a failed native restore can be cleaned up without touching a foreign VM.
// Disk extents and their UUID are unchanged; only the configuration blob grows.
func RestoreVMAHeader(header []byte, run, proxy string) ([]byte, error) {
	if len(header) < 12288 || string(header[:4]) != "VMA\x00" {
		return nil, errors.New("invalid VMA restore header")
	}
	blobOffset := int(binary.BigEndian.Uint32(header[48:52]))
	blobSize := int(binary.BigEndian.Uint32(header[52:56]))
	if blobOffset < 12288 || blobOffset+blobSize > len(header) {
		return nil, errors.New("invalid VMA restore blob table")
	}
	blob := append([]byte(nil), header[blobOffset:blobOffset+blobSize]...)
	get := func(offset uint32) ([]byte, error) {
		at := int(offset)
		if at < 0 || at+2 > len(blob) {
			return nil, errors.New("invalid VMA configuration offset")
		}
		n := int(binary.BigEndian.Uint16(blob[at : at+2]))
		if at+2+n > len(blob) {
			return nil, errors.New("truncated VMA configuration")
		}
		return blob[at+2 : at+2+n], nil
	}
	found := -1
	var config []byte
	for i := 0; i < 256; i++ {
		nameOffset := binary.BigEndian.Uint32(header[2044+i*4 : 2048+i*4])
		if nameOffset == 0 {
			continue
		}
		name, e := get(nameOffset)
		if e != nil {
			return nil, e
		}
		if strings.TrimRight(string(name), "\x00") == "qemu-server.conf" {
			found = i
			config, e = get(binary.BigEndian.Uint32(header[3068+i*4 : 3072+i*4]))
			if e != nil {
				return nil, e
			}
			break
		}
	}
	if found < 0 {
		return nil, errors.New("VMA qemu-server.conf is missing")
	}
	lines := []string{}
	for _, line := range strings.Split(strings.TrimRight(string(config), "\x00"), "\n") {
		key, _, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		if ok && (key == "args" || key == "hookscript" || strings.HasPrefix(key, "hostpci") || strings.HasPrefix(key, "usb") || strings.HasPrefix(key, "virtiofs")) {
			return nil, errors.New("unsafe host integration in VMA configuration")
		}
		if ok && (key == "description" || key == "name" || key == "vmgenid" || key == "smbios1" || key == "onboot" || key == "startup" || key == "protection" || key == "lock" || key == "template" || strings.HasPrefix(key, "unused")) {
			continue
		}
		lines = append(lines, line)
	}
	lines = append(lines, "onboot: 0", "protection: 0", "description: srk_run="+run+";srk_proxy="+proxy, "name: srk-"+run+"-vm", "vmgenid: "+cloudUUIDFromRun(run), "smbios1: uuid="+cloudUUIDFromRun(run))
	replacement := []byte(strings.Join(lines, "\n") + "\n")
	if len(replacement) > 65535 {
		return nil, errors.New("VMA restore configuration exceeds format bound")
	}
	pointer := len(blob)
	blob = append(blob, byte(len(replacement)>>8), byte(len(replacement)))
	blob = append(blob, replacement...)
	size := (blobOffset + len(blob) + 511) / 512 * 512
	out := make([]byte, size)
	copy(out, header[:blobOffset])
	copy(out[blobOffset:], blob)
	binary.BigEndian.PutUint32(out[52:56], uint32(len(blob)))
	binary.BigEndian.PutUint32(out[56:60], uint32(size))
	binary.BigEndian.PutUint32(out[3068+found*4:3072+found*4], uint32(pointer))
	clear(out[32:48])
	digest := md5.Sum(out)
	copy(out[32:48], digest[:])
	return out, nil
}
