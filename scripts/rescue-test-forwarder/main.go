// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
// Local development test only: forward encrypted QEMU traffic to the host.
package main

import (
	"fmt"
	"io"
	"net"
)

func main() {
	for _, port := range []string{"8443", "8444"} {
		port := port
		l, e := net.Listen("tcp", "127.0.0.1:"+port)
		if e != nil {
			panic(e)
		}
		go func() {
			for {
				c, e := l.Accept()
				if e != nil {
					return
				}
				go func() {
					defer c.Close()
					remote, e := net.Dial("tcp", "host.docker.internal:"+port)
					if e != nil {
						return
					}
					defer remote.Close()
					go io.Copy(c, remote)
					io.Copy(remote, c)
				}()
			}
		}()
	}
	fmt.Println("TEST_FORWARDER_READY")
	select {}
}
