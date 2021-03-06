// Package mirror provides local mirroring and replica management
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package mirror

import (
	"errors"
	"fmt"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/fs/mpather"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/xaction"
	"github.com/NVIDIA/aistore/xaction/xreg"
)

const (
	logNumProcessed = 256 // unit of house-keeping
)

type (
	putMirrorProvider struct {
		xreg.BaseBckEntry
		xact *XactPut

		t    cluster.Target
		uuid string
		lom  *cluster.LOM
	}
	XactPut struct {
		// implements cluster.Xact interface
		xaction.XactDemandBase
		// runtime
		workers *mpather.WorkerGroup
		workCh  chan cluster.LIF
		// init
		mirror  cmn.MirrorConf
		total   atomic.Int64
		dropped int64
	}
)

// interface guard
var _ cluster.Xact = (*XactPut)(nil)

func (*putMirrorProvider) New(args xreg.XactArgs) xreg.BucketEntry {
	return &putMirrorProvider{t: args.T, uuid: args.UUID, lom: args.Custom.(*cluster.LOM)}
}

func (p *putMirrorProvider) Start(_ cmn.Bck) error {
	slab, err := p.t.MMSA().GetSlab(memsys.MaxPageSlabSize) // TODO: estimate
	cos.AssertNoErr(err)
	xact, err := runXactPut(p.lom, slab, p.t)
	if err != nil {
		glog.Error(err)
		return err
	}
	p.xact = xact
	return nil
}
func (*putMirrorProvider) Kind() string        { return cmn.ActPutCopies }
func (p *putMirrorProvider) Get() cluster.Xact { return p.xact }

// main
func runXactPut(lom *cluster.LOM, slab *memsys.Slab, t cluster.Target) (r *XactPut, err error) {
	mirror := *lom.MirrorConf()
	if !mirror.Enabled {
		return nil, errors.New("mirroring disabled, nothing to do")
	}
	if err = fs.ValidateNCopies(t.Sname(), int(mirror.Copies)); err != nil {
		return
	}
	bck := lom.Bucket()
	args := xaction.Args{ID: xaction.BaseID(""), Kind: cmn.ActPutCopies, Bck: &bck}
	r = &XactPut{
		XactDemandBase: *xaction.NewXDB(args),
		mirror:         mirror,
		workCh:         make(chan cluster.LIF, mirror.Burst),
	}
	r.InitIdle()

	r.workers = mpather.NewWorkerGroup(&mpather.WorkerGroupOpts{
		Callback:  r.workCb,
		Slab:      slab,
		QueueSize: mirror.Burst,
	})
	// Run
	go r.Run()
	return
}

// mpather/worker callback (one worker per mountpath)
func (r *XactPut) workCb(lom *cluster.LOM, buf []byte) {
	copies := int(lom.Bprops().Mirror.Copies)
	if _, err := addCopies(lom, copies, buf); err != nil {
		glog.Error(err)
	} else {
		r.ObjectsAdd(int64(copies))
	}
	r.DecPending() // to support action renewal on-demand
	cluster.FreeLOM(lom)
}

// control logic: stop and idle timer
// (LOMs get dispatched directly to workers)
func (r *XactPut) Run() {
	glog.Infoln(r.String())
	r.workers.Run()
	for {
		select {
		case <-r.IdleTimer():
			err := r.stop()
			r.Finish(err)
			return
		case <-r.ChanAbort():
			if err := r.stop(); err != nil {
				r.Finish(err)
			} else {
				r.Finish(cmn.NewAbortedError(r.String()))
			}
			return
		}
	}
}

// main method: replicate onto a given (and different) mountpath
func (r *XactPut) Repl(lom *cluster.LOM) (err error) {
	if r.Finished() {
		err = xaction.NewErrXactExpired("Cannot replicate: " + r.String())
		return
	}
	r.total.Inc()

	// [throttle]
	// when the optimization objective is write perf,
	// we start dropping requests to make sure callers don't block
	pending, max := int(r.Pending()), r.mirror.Burst
	if r.mirror.OptimizePUT {
		if pending > 1 && pending >= max {
			r.dropped++
			if (r.dropped % logNumProcessed) == 0 {
				glog.Errorf("%s: pending=%d, total=%d, dropped=%d", r, pending, r.total.Load(), r.dropped)
			}
			return
		}
	}
	r.IncPending() // ref-count via base to support on-demand action

	if ok := r.workers.Do(lom); !ok {
		err = fmt.Errorf("%s: failed to post %s work", r, lom)
		debug.AssertNoErr(err)
		return
	}

	// [throttle]
	// a bit of back-pressure when approaching the fixed boundary
	if pending > 1 && max > 10 {
		// increase the chances for the burst of PUTs to subside
		// but only to a point
		if pending > max/2 && pending < max-max/8 {
			time.Sleep(cmn.ThrottleAvg)
		}
	}
	return
}

func (r *XactPut) stop() (err error) {
	r.XactDemandBase.Stop()
	n := r.workers.Stop()
	if nn := drainWorkCh(r.workCh); nn > 0 {
		n += nn
	}
	if n > 0 {
		r.SubPending(n)
		err = fmt.Errorf("%s: dropped %d object(s)", r, n)
	}
	return err
}

func (r *XactPut) Stats() cluster.XactStats {
	baseStats := r.XactDemandBase.Stats().(*xaction.BaseXactStatsExt)
	baseStats.Ext = &xaction.BaseXactDemandStatsExt{IsIdle: r.Pending() == 0}
	return baseStats
}
