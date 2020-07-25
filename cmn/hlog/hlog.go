// Package hlog is a cluster history logging facility
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package hlog

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/hk"
	"github.com/NVIDIA/aistore/memsys"
)

// History Logging Facility
// ========================
// The history consists of timestamped events listed below.
// The events are reliably and persistently preserved in a structured form that allows easy parsing.
// ====
// 1. Historical event is either an event that changes cluster map or an asynchronous operation
// 2. The former include: startup, shutdown, node-join, node-leave, primary-change
// 3. Async operations are, for the most part, *xactions* - namely:
//    (rebalance, make-n-copies, ec, dsort, election) * (start, abort, done)
// 4. TODO: keepalive events to record spikes in latency, and more
// 5. log records are buffered and peridocially flushed; buffering is done via memsys
// 6. log flushing is done upon: memory-buffer-is-full | process termination | log recycling
// 7. TODO: add GET(hlog) API and CLI
// 8. default configuration follows below; TODO add section in the global config
// 9. TODO: make date and time configurable, include Unix seconds and nanoseconds since 1970,
//    and also:
//    RFC3339 aka ISO8601 = "2020-06-02T15:04:05Z07:00"
//    RFC3339Nano         = "2020-06-02T15:04:05.999999999Z07:00"
//    StampMilli          = "Jun _3 15:04:05.123" (the default)

// config defaults
const (
	logpath = "/tmp/ais"
	logname = "hlog"
	logprev = ".prev"
	hktimer = 10 * time.Minute
	maxsize = cmn.MiB
	minsize = memsys.PageSize - cmn.KiB
)

type hlogger struct {
	sync.Mutex
	fqn string
	gmm *memsys.MMSA
	sgl struct {
		s    *memsys.SGL
		mtx  sync.RWMutex
		size atomic.Int64
	}
	fsize atomic.Int64
}

var (
	hlog = hlogger{}
)

func Init(paths ...string) (err error) {
	path := logpath
	if len(paths) > 0 {
		path = paths[0]
	}
	hlog.fqn = filepath.Join(path, logname)
	hlog.gmm = memsys.DefaultPageMM()
	hlog.sgl.s = hlog.gmm.NewSGL(memsys.PageSize, memsys.PageSize)

	hk.Reg(logname, housekeep) // register w/ hk
	err = rotate()             // rotate the log if need be
	return
}

func Log(event string, any interface{}, nows ...int64) (err error) {
	var (
		now time.Time
		n   int64
	)
	if len(nows) > 0 {
		now = time.Unix(0, nows[0])
	} else {
		now = time.Now()
	}
	buf := marshal(now, event, any)
	hlog.sgl.mtx.Lock()
	n, err = buf.WriteTo(hlog.sgl.s)
	hlog.sgl.size.Add(n)
	debug.Assert(hlog.sgl.size.Load() == hlog.sgl.s.Len())
	hlog.sgl.mtx.Unlock()
	buf.Free()
	return
}

func Begin(v interface{}) (err error)              { return Log("begin", v) }
func End(v interface{}, nows ...int64) (err error) { return Log("end", v, nows...) }

func Flush(forces ...bool) (err error) {
	var (
		file *os.File
		n    int64
		f    = len(forces) > 0 && forces[0]
	)
	if !f && hlog.sgl.size.Load() < minsize {
		return
	}
	hlog.Lock()
	defer hlog.Unlock()

	if file, err = os.OpenFile(hlog.fqn, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err != nil {
		return
	}
	defer file.Close()

	hlog.sgl.mtx.RLock()
	defer hlog.sgl.mtx.RUnlock()
	n, err = hlog.sgl.s.WriteTo(file)
	hlog.fsize.Add(n)
	debug.AssertNoErr(err)
	hlog.sgl.s.Reset()
	return
}

//
// private
//

func marshal(now time.Time, event string, any interface{}) *memsys.SGL {
	var (
		v = reflect.Indirect(reflect.ValueOf(any))
		t = v.Type()
		n = t.NumField()
		b = hlog.gmm.NewSGL(memsys.PageSize, memsys.PageSize)
	)
	b.WriteString(cmn.FormatTimestamp(now))
	b.WriteString(",")
	b.WriteString(event)
	b.WriteString(",")
	for i := 0; i < n; i++ {
		b.WriteString(strings.ToLower(t.Field(i).Name))
		b.WriteString("=")
		val := v.Field(i)
		switch val.Kind() {
		case reflect.String:
			b.WriteString(val.String())
		case reflect.Int32, reflect.Int64:
			b.WriteString(strconv.FormatInt(val.Int(), 10))
		case reflect.Uint32, reflect.Uint64:
			b.WriteString(strconv.FormatUint(val.Uint(), 10))
		default:
			b.WriteString(fmt.Sprintf("%v", val.Interface()))
		}
		if i < n-1 {
			b.WriteString(",")
		}
	}
	b.WriteString("\n")
	return b
}

func rotate() (err error) {
	var finfo os.FileInfo
	if hlog.fsize.Load() < maxsize {
		return
	}
	hlog.Lock()
	defer hlog.Unlock()
	if finfo, err = os.Stat(hlog.fqn); err == nil {
		debug.Assert(finfo.Size() == hlog.fsize.Load())
	} else {
		return
	}
	if err = os.Rename(hlog.fqn, hlog.fqn+logprev); err != nil {
		return
	}
	hlog.fsize.Store(0)
	return
}

func housekeep() time.Duration {
	if err := Flush(); err != nil {
		glog.Error(err)
	} else if err = rotate(); err != nil {
		glog.Error(err)
	}
	return hktimer
}
