// Package reb provides resilvering and rebalancing functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package reb

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"sync"
	"time"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/filter"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/transport/bundle"
	"github.com/NVIDIA/aistore/xaction/registry"
	"github.com/NVIDIA/aistore/xaction/runners"
)

const (
	rebTrname     = "rebalance"
	rebPushTrname = "rebpush" // broadcast push notifications
)

// rebalance stage enum
const (
	rebStageInactive = iota
	rebStageInit
	rebStageTraverse
	rebStageECNamespace // local CT list built
	rebStageECDetect    // all lists are received, start detecting which objects to fix
	rebStageECRepair    // all local CTs are fine, targets start rebalance
	rebStageECBatch     // target sends message that the current batch is processed
	rebStageECCleanup   // all is done, time to cleanup memory etc
	rebStageWaitAck
	rebStageFin
	rebStageFinStreams
	rebStageDone
	rebStageAbort // one of targets aborts the rebalancing (never set, only sent)
)

type (
	syncCallback func(tsi *cluster.Snode, md *rebArgs) (ok bool)
	joggerBase   struct {
		m    *Manager
		xreb *runners.RebBase
		wg   *sync.WaitGroup
	}

	Manager struct {
		t          cluster.Target
		dm         *bundle.DataMover
		pushes     *bundle.Streams // broadcast notifications
		statRunner *stats.Trunner
		filterGFN  *filter.Filter
		smap       atomic.Pointer // new smap which will be soon live
		lomacks    [cmn.MultiSyncMapCount]*lomAcks
		awaiting   struct {
			mu      sync.Mutex
			targets cluster.NodeMap // targets for which we are waiting for
			ts      int64           // last time we have recomputed
		}
		semaCh     chan struct{}
		beginStats atomic.Pointer // *stats.ExtRebalanceStats
		xreb       atomic.Pointer // *xaction.Rebalance
		stages     *nodeStages
		ec         *ecData
		ecClient   *http.Client
		rebID      atomic.Int64
		inQueue    atomic.Int64
		laterx     atomic.Bool
	}
	// Stage status of a single target
	stageStatus struct {
		batchID int64  // current batch ID (0 for non-EC stages)
		stage   uint32 // current stage
	}
	lomAcks struct {
		mu *sync.Mutex
		q  map[string]*cluster.LOM // on the wire, waiting for ACK
	}
)

var stages = map[uint32]string{
	rebStageInactive:    "<inactive>",
	rebStageInit:        "<init>",
	rebStageTraverse:    "<traverse>",
	rebStageWaitAck:     "<wack>",
	rebStageFin:         "<fin>",
	rebStageFinStreams:  "<fin-streams>",
	rebStageDone:        "<done>",
	rebStageECNamespace: "<namespace>",
	rebStageECDetect:    "<build-fix-list>",
	rebStageECRepair:    "<ec-transfer>",
	rebStageECCleanup:   "<ec-fin>",
	rebStageECBatch:     "<ec-batch>",
	rebStageAbort:       "<abort>",
}

//////////////////////////////////////////////
// rebalance manager: init, receive, common //
//////////////////////////////////////////////

func NewManager(t cluster.Target, config *cmn.Config, strunner *stats.Trunner) *Manager {
	ecClient := cmn.NewClient(cmn.TransportArgs{
		Timeout:    config.Client.Timeout,
		UseHTTPS:   config.Net.HTTP.UseHTTPS,
		SkipVerify: config.Net.HTTP.SkipVerify,
	})
	reb := &Manager{
		t:          t,
		filterGFN:  filter.NewDefaultFilter(),
		statRunner: strunner,
		stages:     newNodeStages(),
		ecClient:   ecClient,
	}
	rebcfg := &config.Rebalance
	dmExtra := bundle.Extra{
		RecvAck:     reb.recvAck,
		Compression: rebcfg.Compression,
		Multiplier:  int(rebcfg.Multiplier),
	}
	dm, err := bundle.NewDataMover(t, rebTrname, reb.recvObj, dmExtra)
	if err != nil {
		cmn.ExitLogf("%v", err)
	}
	reb.dm = dm
	reb.ec = newECData()
	reb.registerRecv()
	return reb
}

// NOTE: these receive handlers are statically present throughout: unreg never done
func (reb *Manager) registerRecv() {
	if err := reb.dm.RegRecv(); err != nil {
		cmn.ExitLogf("%v", err)
	}
	if _, err := transport.Register(reb.dm.NetC(), rebPushTrname, reb.recvPush); err != nil {
		cmn.ExitLogf("%v", err)
	}
	// serialization: one at a time
	reb.semaCh = make(chan struct{}, 1)
	reb.semaCh <- struct{}{}
}

func (reb *Manager) RebID() int64           { return reb.rebID.Load() }
func (reb *Manager) FilterAdd(uname []byte) { reb.filterGFN.Insert(uname) }

func (reb *Manager) xact() *runners.Rebalance                  { return (*runners.Rebalance)(reb.xreb.Load()) }
func (reb *Manager) setXact(xact *runners.Rebalance)           { reb.xreb.Store(unsafe.Pointer(xact)) }
func (reb *Manager) lomAcks() *[cmn.MultiSyncMapCount]*lomAcks { return &reb.lomacks }
func (reb *Manager) addLomAck(lom *cluster.LOM) {
	_, idx := lom.Hkey()
	lomAck := reb.lomAcks()[idx]
	lomAck.mu.Lock()
	lomAck.q[lom.Uname()] = lom
	lomAck.mu.Unlock()
}

func (reb *Manager) delLomAck(lom *cluster.LOM) {
	_, idx := lom.Hkey()
	lomAck := reb.lomAcks()[idx]
	lomAck.mu.Lock()
	delete(lomAck.q, lom.Uname())
	lomAck.mu.Unlock()
}

func (reb *Manager) logHdr(md *rebArgs) string {
	stage := stages[reb.stages.stage.Load()]
	return fmt.Sprintf("%s[g%d,v%d,%s]", reb.t.Snode(), md.id, md.smap.Version, stage)
}

func (reb *Manager) rebIDMismatchMsg(remoteID int64) string {
	return fmt.Sprintf("rebalance IDs mismatch: local %d, remote %d", reb.RebID(), remoteID)
}

func (reb *Manager) getStats() (s *stats.ExtRebalanceStats) {
	s = &stats.ExtRebalanceStats{}
	statsRunner := reb.statRunner
	s.RebTxCount = statsRunner.Get(stats.RebTxCount)
	s.RebTxSize = statsRunner.Get(stats.RebTxSize)
	s.RebRxCount = statsRunner.Get(stats.RebRxCount)
	s.RebRxSize = statsRunner.Get(stats.RebRxSize)
	s.RebID = reb.rebID.Load()
	return
}

func (reb *Manager) beginStreams() {
	cmn.Assert(reb.stages.stage.Load() == rebStageInit)

	reb.dm.Open()
	pushArgs := bundle.Args{Network: reb.dm.NetC(), Trname: rebPushTrname}
	reb.pushes = bundle.NewStreams(reb.t.Sowner(), reb.t.Snode(), transport.NewIntraDataClient(), pushArgs)

	reb.laterx.Store(false)
	reb.inQueue.Store(0)
}

func (reb *Manager) endStreams() {
	if reb.stages.stage.CAS(rebStageFin, rebStageFinStreams) {
		reb.dm.Close()
		reb.pushes.Close(true)
	}
}

func (reb *Manager) recvObjRegular(hdr transport.ObjHdr, smap *cluster.Smap, unpacker *cmn.ByteUnpack, objReader io.Reader) {
	defer cmn.DrainReader(objReader)

	ack := &regularAck{}
	if err := unpacker.ReadAny(ack); err != nil {
		glog.Errorf("Failed to parse acknowledgement: %v", err)
		return
	}

	if ack.rebID != reb.RebID() {
		glog.Warningf("received object %s/%s: %s", hdr.Bck, hdr.ObjName, reb.rebIDMismatchMsg(ack.rebID))
		return
	}
	tsid := ack.daemonID // the sender
	// Rx
	lom := &cluster.LOM{T: reb.t, ObjName: hdr.ObjName}
	if err := lom.Init(hdr.Bck); err != nil {
		glog.Error(err)
		return
	}
	marked := registry.GetRebMarked()
	if marked.Interrupted || marked.Xact == nil {
		return
	}

	if stage := reb.stages.stage.Load(); stage >= rebStageFin {
		reb.laterx.Store(true)
		f := glog.Warningf
		if stage > rebStageFin {
			f = glog.Errorf
		}
		f("%s: late receive from %s %s (stage %s)", reb.t.Snode(), tsid, lom, stages[stage])
	} else if stage < rebStageTraverse {
		glog.Errorf("%s: early receive from %s %s (stage %s)", reb.t.Snode(), tsid, lom, stages[stage])
	}
	lom.SetAtimeUnix(hdr.ObjAttrs.Atime)
	lom.SetVersion(hdr.ObjAttrs.Version)

	params := cluster.PutObjectParams{
		Reader:       ioutil.NopCloser(objReader),
		WorkFQN:      fs.CSM.GenContentParsedFQN(lom.ParsedFQN, fs.WorkfileType, fs.WorkfilePut),
		RecvType:     cluster.Migrated,
		Cksum:        cmn.NewCksum(hdr.ObjAttrs.CksumType, hdr.ObjAttrs.CksumValue),
		Started:      time.Now(),
		WithFinalize: true,
	}
	if err := reb.t.PutObject(lom, params); err != nil {
		glog.Error(err)
		return
	}

	if glog.FastV(5, glog.SmoduleReb) {
		glog.Infof("%s: from %s %s", reb.t.Snode(), tsid, lom)
	}
	reb.statRunner.AddMany(
		stats.NamedVal64{Name: stats.RebRxCount, Value: 1},
		stats.NamedVal64{Name: stats.RebRxSize, Value: hdr.ObjAttrs.Size},
	)
	// ACK
	tsi := smap.GetTarget(tsid)
	if tsi == nil {
		glog.Errorf("%s target is not found in smap", tsid)
		return
	}
	if stage := reb.stages.stage.Load(); stage < rebStageFinStreams && stage != rebStageInactive {
		var (
			ack = &regularAck{rebID: reb.RebID(), daemonID: reb.t.Snode().ID()}
			mm  = reb.t.SmallMMSA()
		)
		hdr.Opaque = ack.NewPack(mm)
		hdr.ObjAttrs.Size = 0
		if err := reb.dm.ACK(hdr, reb.rackSentCallback, tsi); err != nil {
			mm.Free(hdr.Opaque)
			glog.Error(err) // TODO: collapse same-type errors e.g. "src-id=>network: destination mismatch"
		}
	}
}

func (reb *Manager) rackSentCallback(hdr transport.ObjHdr, _ io.ReadCloser, _ unsafe.Pointer, _ error) {
	reb.t.SmallMMSA().Free(hdr.Opaque)
}

func (reb *Manager) waitForSmap() (*cluster.Smap, error) {
	smap := (*cluster.Smap)(reb.smap.Load())
	if smap == nil {
		var (
			config = cmn.GCO.Get()
			sleep  = config.Timeout.CplaneOperation
			maxwt  = config.Rebalance.DestRetryTime
			curwt  time.Duration
		)
		maxwt = cmn.MinDuration(maxwt, config.Timeout.SendFile/3)
		glog.Warningf("%s: waiting to start...", reb.t.Snode())
		time.Sleep(sleep)
		for curwt < maxwt {
			smap = (*cluster.Smap)(reb.smap.Load())
			if smap != nil {
				return smap, nil
			}
			time.Sleep(sleep)
			curwt += sleep
		}
		if curwt >= maxwt {
			err := fmt.Errorf("%s: timed-out waiting to start", reb.t.Snode())
			return nil, err
		}
	}
	return smap, nil
}

func (reb *Manager) recvObj(w http.ResponseWriter, hdr transport.ObjHdr, objReader io.Reader, err error) {
	if err != nil {
		glog.Error(err)
		return
	}
	smap, err := reb.waitForSmap()
	if err != nil {
		glog.Errorf("%v: dropping %s/%s", err, hdr.Bck, hdr.ObjName)
		return
	}

	unpacker := cmn.NewUnpacker(hdr.Opaque)
	act, err := unpacker.ReadByte()
	if err != nil {
		glog.Errorf("Failed to read message type: %v", err)
		return
	}

	if act == rebMsgRegular {
		reb.recvObjRegular(hdr, smap, unpacker, objReader)
		return
	}

	if act != rebMsgEC {
		glog.Errorf("Invalid ACK type %d, expected %d", act, rebMsgEC)
	}

	reb.recvECData(hdr, unpacker, objReader)
}

// Rebalance moves to the next stage:
// - update internal stage
// - send notification to all other targets that this one is in a new stage
func (reb *Manager) changeStage(newStage uint32, batchID int64) {
	// first, set own stage
	reb.stages.stage.Store(newStage)
	var (
		req = pushReq{
			daemonID: reb.t.Snode().DaemonID, stage: newStage,
			rebID: reb.rebID.Load(), batch: int(batchID),
		}
		hdr = transport.ObjHdr{}
		mm  = reb.t.SmallMMSA()
	)
	hdr.Opaque = reb.encodePushReq(&req, mm)
	// second, notify all
	if err := reb.pushes.Send(transport.Obj{Hdr: hdr, Callback: reb.pushSentCallback}, nil); err != nil {
		glog.Warningf("Failed to broadcast ack %s: %v", stages[newStage], err)
		mm.Free(hdr.Opaque)
	}
}

func (reb *Manager) pushSentCallback(hdr transport.ObjHdr, _ io.ReadCloser, _ unsafe.Pointer, _ error) {
	reb.t.SmallMMSA().Free(hdr.Opaque)
}

func (reb *Manager) recvPush(w http.ResponseWriter, hdr transport.ObjHdr, objReader io.Reader, err error) {
	if err != nil {
		glog.Errorf("Failed to get notification %s from %s: %v", hdr.ObjName, hdr.Bck, err)
		return
	}

	req, err := reb.decodePushReq(hdr.Opaque)
	if err != nil {
		glog.Error(err)
		return
	}

	if req.stage == rebStageAbort && reb.RebID() <= req.rebID {
		// a target aborted its xaction and sent the signal to others
		glog.Warningf("Rebalance abort notification from %s", req.daemonID)
		if reb.xact() != nil {
			reb.xact().Abort()
		}
		return
	}

	if reb.RebID() != req.rebID {
		glog.Warningf("Stage %v push notification: %s", stages[req.stage], reb.rebIDMismatchMsg(req.rebID))
		return
	}

	if req.stage == rebStageECBatch {
		if glog.FastV(4, glog.SmoduleReb) {
			glog.Infof("%s Target %s finished batch %d", reb.t.Snode(), req.daemonID, req.batch)
		}
	}

	reb.stages.setStage(req.daemonID, req.stage, int64(req.batch))
}

func (reb *Manager) recvECAck(hdr transport.ObjHdr, unpacker *cmn.ByteUnpack) {
	ack := &ecAck{}
	if err := unpacker.ReadAny(ack); err != nil {
		glog.Errorf("Failed to unmarshal EC ACK for %s/%s: %v", hdr.Bck, hdr.ObjName, err)
		return
	}

	rt := &retransmitCT{
		header:  transport.ObjHdr{Bck: hdr.Bck, ObjName: hdr.ObjName},
		sliceID: int16(ack.sliceID), daemonID: ack.daemonID,
	}
	if glog.FastV(4, glog.SmoduleReb) {
		glog.Infof("%s: EC ack from %s on %s/%s [%d]",
			reb.t.Snode(), ack.daemonID, hdr.Bck, hdr.ObjName, ack.sliceID)
	}
	reb.ec.ackCTs.remove(rt)
}

func (reb *Manager) recvRegularAck(hdr transport.ObjHdr, unpacker *cmn.ByteUnpack) {
	ack := &regularAck{}
	if err := unpacker.ReadAny(ack); err != nil {
		glog.Errorf("Failed to parse acknowledge: %v", err)
		return
	}
	if ack.rebID != reb.rebID.Load() {
		glog.Warningf("ACK from %s: %s", ack.daemonID, reb.rebIDMismatchMsg(ack.rebID))
		return
	}

	lom := &cluster.LOM{T: reb.t, ObjName: hdr.ObjName}
	if err := lom.Init(hdr.Bck); err != nil {
		glog.Error(err)
		return
	}
	if glog.FastV(5, glog.SmoduleReb) {
		glog.Infof("%s: ack from %s on %s", reb.t.Snode(), string(hdr.Opaque), lom)
	}
	reb.delLomAck(lom)

	// TODO: configurable delay - postponed or manual object deletion
	lom.Lock(true)
	if err := lom.Remove(); err != nil {
		glog.Errorf("%s: error removing %s, err: %v", reb.t.Snode(), lom, err)
	}
	lom.Unlock(true)
}

func (reb *Manager) recvAck(w http.ResponseWriter, hdr transport.ObjHdr, _ io.Reader, err error) {
	if err != nil {
		glog.Error(err)
		return
	}

	unpacker := cmn.NewUnpacker(hdr.Opaque)
	act, err := unpacker.ReadByte()
	if err != nil {
		glog.Errorf("failed to read message type: %v", err)
		return
	}

	if act == rebMsgEC {
		reb.recvECAck(hdr, unpacker)
		return
	}
	if act != rebMsgRegular {
		glog.Errorf("Invalid ACK type %d, expected %d", act, rebMsgRegular)
	}

	reb.recvRegularAck(hdr, unpacker)
}

func (reb *Manager) retransmit(md *rebArgs) (cnt int) {
	aborted := func() (yes bool) {
		yes = reb.xact().Aborted()
		yes = yes || (md.smap.Version != reb.t.Sowner().Get().Version)
		return
	}
	if aborted() {
		return
	}
	var (
		rj = &rebalanceJogger{joggerBase: joggerBase{
			m: reb, xreb: &reb.xact().RebBase,
			wg: &sync.WaitGroup{},
		}, smap: md.smap}
		query = url.Values{}
	)
	query.Add(cmn.URLParamSilent, "true")
	for _, lomAck := range reb.lomAcks() {
		lomAck.mu.Lock()
		for uname, lom := range lomAck.q {
			if err := lom.Load(false); err != nil {
				if cmn.IsObjNotExist(err) {
					glog.Warningf("%s: %s %s", reb.logHdr(md), lom, cmn.DoesNotExist)
				} else {
					glog.Errorf("%s: failed loading %s, err: %s", reb.logHdr(md), lom, err)
				}
				delete(lomAck.q, uname)
				continue
			}
			tsi, _ := cluster.HrwTarget(lom.Uname(), md.smap)
			if reb.t.LookupRemoteSingle(lom, tsi) {
				if glog.FastV(4, glog.SmoduleReb) {
					glog.Infof("%s: HEAD ok %s at %s", reb.logHdr(md), lom, tsi)
				}
				delete(lomAck.q, uname)
				continue
			}
			// send obj
			if err := rj.send(lom, tsi, false /*addAck*/); err == nil {
				glog.Warningf("%s: resending %s => %s", reb.logHdr(md), lom, tsi)
				cnt++
			} else {
				glog.Errorf("%s: failed resending %s => %s, err: %v", reb.logHdr(md), lom, tsi, err)
			}
			if aborted() {
				lomAck.mu.Unlock()
				return 0
			}
		}
		lomAck.mu.Unlock()
		if aborted() {
			return 0
		}
	}
	return
}

// Aborts rebalance xaction and notifies all other targets
// that they has to abort rebalance as well.
// Useful for EC rebalance: after each batch EC rebalance waits in a loop
// for all targets to finish their batches. No stream interactions in this loop,
// except listening to push notifications. So, if any target stops its xaction
// and closes all its streams, others wouldn't notice that. That is why the
// target should send notification.
func (reb *Manager) abortRebalance() {
	xreb := reb.xact()
	if xreb == nil || xreb.Aborted() || xreb.Finished() {
		return
	}
	glog.Info("aborting rebalance...")
	xreb.Abort()
	var (
		req = pushReq{
			daemonID: reb.t.Snode().DaemonID,
			rebID:    reb.RebID(),
			stage:    rebStageAbort,
		}
		hdr = transport.ObjHdr{}
		mm  = reb.t.SmallMMSA()
	)
	hdr.Opaque = reb.encodePushReq(&req, mm)
	if err := reb.pushes.Send(transport.Obj{Hdr: hdr, Callback: reb.pushSentCallback}, nil); err != nil {
		glog.Errorf("Failed to broadcast abort notification: %v", err)
	}
}

// Returns if the target is quiescent: transport queue is empty, or xaction
// has already aborted or finished.
func (reb *Manager) isQuiescent() bool {
	// Finished or aborted xaction = no traffic
	xact := reb.xact()
	if xact == nil || xact.Aborted() || xact.Finished() {
		return true
	}

	// Has not finished the stage that generates network traffic yet
	if reb.stages.stage.Load() < rebStageECBatch {
		return false
	}
	// Check for both regular and EC transport queues are empty
	return reb.inQueue.Load() == 0 && reb.ec.onAir.Load() == 0
}
