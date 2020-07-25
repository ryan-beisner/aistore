// Package hlog_test provides tests for hlog package
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package hlog_test

import (
	"testing"

	"github.com/NVIDIA/aistore/cmn/hlog"
)

func Test123(t *testing.T) {
	hlog.Init()
	x := struct {
		Foo string
		Bar int
	}{"string-val", 12345}
	hlog.Log("event-blah", x)
	y := struct {
		Foo string
		Bar int
	}{"val-string", 98765}
	hlog.Log("notif-foo", y)

	hlog.Flush(true /* force */)
}
