// Package cmn provides common constants, types, and utilities for AIS clients
// and AIStore.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"syscall"

	"github.com/NVIDIA/aistore/cmn/cos"
)

func (args *TransportArgs) setSockOpt(_, _ string, c syscall.RawConn) (err error) {
	return c.Control(args.ConnControl(c))
}

func (args *TransportArgs) ConnControl(c syscall.RawConn) (cntl func(fd uintptr)) {
	cntl = func(fd uintptr) {
		// NOTE: is limited by /proc/sys/net/core/rmem_max
		err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, args.SndRcvBufSize)
		cos.AssertNoErr(err)
		// NOTE: is limited by /proc/sys/net/core/wmem_max
		err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, args.SndRcvBufSize)
		cos.AssertNoErr(err)
	}
	return
}
