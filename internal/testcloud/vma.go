// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package testcloud

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"sort"
)

// Fixture encoder follows the published VMA wire format. It is not a VM backend.
func VMA(config string, disks map[string][]byte) []byte {
	blob := []byte{0, 0}
	add := func(b []byte) uint32 {
		pointer := uint32(len(blob))
		blob = append(blob, byte(len(b)>>8), byte(len(b)))
		blob = append(blob, b...)
		return pointer
	}
	configName := add([]byte("qemu-server.conf"))
	configData := add([]byte(config))
	names := []string{}
	for name := range disks {
		names = append(names, name)
	}
	sort.Strings(names)
	pointers := map[string]uint32{}
	for _, name := range names {
		pointers[name] = add([]byte("drive-" + name))
	}
	size := (12288 + len(blob) + 511) / 512 * 512
	header := make([]byte, size)
	copy(header, []byte{'V', 'M', 'A', 0})
	binary.BigEndian.PutUint32(header[4:8], 1)
	copy(header[8:24], []byte("SRK-VMA-FIXTURE!!"))
	binary.BigEndian.PutUint64(header[24:32], 1793217600)
	binary.BigEndian.PutUint32(header[48:52], 12288)
	binary.BigEndian.PutUint32(header[52:56], uint32(len(blob)))
	binary.BigEndian.PutUint32(header[56:60], uint32(size))
	binary.BigEndian.PutUint32(header[2044:2048], configName)
	binary.BigEndian.PutUint32(header[3068:3072], configData)
	for i, name := range names {
		at := 4096 + (i+1)*32
		binary.BigEndian.PutUint32(header[at:at+4], pointers[name])
		binary.BigEndian.PutUint64(header[at+8:at+16], uint64(len(disks[name])))
	}
	copy(header[12288:], blob)
	sum := md5.Sum(header)
	copy(header[32:48], sum[:])
	var out bytes.Buffer
	out.Write(header)
	for i, name := range names {
		data := disks[name]
		for cluster := 0; cluster*65536 < len(data); cluster++ {
			extent := make([]byte, 512)
			copy(extent, []byte("VMAE"))
			binary.BigEndian.PutUint16(extent[6:8], 16)
			copy(extent[8:24], header[8:24])
			binary.BigEndian.PutUint16(extent[40:42], 0xffff)
			extent[43] = byte(i + 1)
			binary.BigEndian.PutUint32(extent[44:48], uint32(cluster))
			checksum := md5.Sum(extent)
			copy(extent[24:40], checksum[:])
			out.Write(extent)
			block := make([]byte, 65536)
			copy(block, data[cluster*65536:min((cluster+1)*65536, len(data))])
			out.Write(block)
		}
	}
	return out.Bytes()
}
func VMAConfig(archive []byte) (string, error) {
	if len(archive) < 12288 || string(archive[:4]) != "VMA\x00" {
		return "", fmt.Errorf("invalid fixture VMA")
	}
	offset := int(binary.BigEndian.Uint32(archive[48:52]))
	pointer := int(binary.BigEndian.Uint32(archive[3068:3072]))
	at := offset + pointer
	if at+2 > len(archive) {
		return "", fmt.Errorf("invalid VMA configuration")
	}
	n := int(binary.BigEndian.Uint16(archive[at : at+2]))
	if at+2+n > len(archive) {
		return "", fmt.Errorf("truncated VMA configuration")
	}
	return string(archive[at+2 : at+2+n]), nil
}
