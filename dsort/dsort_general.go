// Package dsort provides distributed massively parallel resharding for very large datasets.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dsort

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"sync"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/dsort/extract"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/sys"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/transport/bundle"
	jsoniter "github.com/json-iterator/go"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

// This is general implementation of dsort, for all types of workloads.
// Historically, it is the first implementation of dsorter ever created.
// It tries to use memory in extraction phase to pull as most of the data
// into memory as possible. This way, later, dsorter will use it later in
// creation phase. It means that if the data takes less space than the available
// memory in the cluster, we can extract whole data into memory and make
// the creation phase super fast.

const (
	DSorterGeneralType = "dsort_general"
)

type (
	dsorterGeneral struct {
		m  *Manager
		mw *memoryWatcher

		streams struct {
			request  *bundle.Streams
			response *bundle.Streams
		}

		creationPhase struct {
			adjuster *concAdjuster

			streamWriters struct {
				mu      sync.Mutex
				writers map[string]*streamWriter
			}
		}
	}

	streamWriter struct {
		w   io.Writer
		n   int64
		err error
		wg  *cmn.TimeoutGroup
	}

	remoteRequest struct {
		DaemonID  string             `json:"d"`
		Record    *extract.Record    `json:"r"`
		RecordObj *extract.RecordObj `json:"o"`
	}
)

var _ dsorter = &dsorterGeneral{}

func newDSorterGeneral(m *Manager) (*dsorterGeneral, error) {
	// Memory watcher
	mem, err := sys.Mem()
	if err != nil {
		return nil, err
	}
	maxMemoryToUse := calcMaxMemoryUsage(m.rs.MaxMemUsage, mem)
	ds := &dsorterGeneral{
		m:  m,
		mw: newMemoryWatcher(m, maxMemoryToUse),
	}
	ds.creationPhase.streamWriters.writers = make(map[string]*streamWriter, 10000)
	return ds, nil
}

func (ds *dsorterGeneral) newStreamWriter(pathToContents string, w io.Writer) *streamWriter {
	writer := &streamWriter{
		w:  w,
		wg: cmn.NewTimeoutGroup(),
	}
	writer.wg.Add(1)
	ds.creationPhase.streamWriters.mu.Lock()
	ds.creationPhase.streamWriters.writers[pathToContents] = writer
	ds.creationPhase.streamWriters.mu.Unlock()
	return writer
}

func (ds *dsorterGeneral) pullStreamWriter(objName string) *streamWriter {
	ds.creationPhase.streamWriters.mu.Lock()
	writer := ds.creationPhase.streamWriters.writers[objName]
	delete(ds.creationPhase.streamWriters.writers, objName)
	ds.creationPhase.streamWriters.mu.Unlock()
	return writer
}

func (ds *dsorterGeneral) name() string { return DSorterGeneralType }

func (ds *dsorterGeneral) init() error {
	ds.creationPhase.adjuster = newConcAdjuster(
		ds.m.rs.CreateConcMaxLimit,
		1, /*goroutineLimitCoef*/
	)
	return nil
}

func (ds *dsorterGeneral) start() error {
	// Requests are usually small packets, no more 1KB that is why we want to
	// utilize intraControl network.
	config := cmn.GCO.Get()
	reqNetwork := cmn.NetworkIntraControl
	if !config.Net.UseIntraControl {
		reqNetwork = cmn.NetworkPublic
	}
	// Responses to the other targets are objects that is why we want to use
	// intraData network.
	respNetwork := cmn.NetworkIntraData
	if !config.Net.UseIntraData {
		respNetwork = cmn.NetworkPublic
	}

	client := transport.NewIntraDataClient()

	streamMultiplier := bundle.Multiplier
	if ds.m.rs.StreamMultiplier != 0 {
		streamMultiplier = ds.m.rs.StreamMultiplier
	}

	trname := fmt.Sprintf(recvReqStreamNameFmt, ds.m.ManagerUUID)
	reqSbArgs := bundle.Args{
		Multiplier: 20,
		Network:    reqNetwork,
		Trname:     trname,
		Ntype:      cluster.Targets,
	}
	if _, err := transport.Register(reqNetwork, trname, ds.makeRecvRequestFunc()); err != nil {
		return errors.WithStack(err)
	}

	trname = fmt.Sprintf(recvRespStreamNameFmt, ds.m.ManagerUUID)
	respSbArgs := bundle.Args{
		Multiplier: streamMultiplier,
		Network:    respNetwork,
		Trname:     trname,
		Ntype:      cluster.Targets,
		Extra: &transport.Extra{
			Compression: config.DSort.Compression,
			Config:      config,
			MMSA:        mm,
		},
	}
	if _, err := transport.Register(respNetwork, trname, ds.makeRecvResponseFunc()); err != nil {
		return errors.WithStack(err)
	}

	ds.streams.request = bundle.NewStreams(ds.m.ctx.smapOwner, ds.m.ctx.node, client, reqSbArgs)
	ds.streams.response = bundle.NewStreams(ds.m.ctx.smapOwner, ds.m.ctx.node, client, respSbArgs)

	// start watching memory
	return ds.mw.watch()
}

func (ds *dsorterGeneral) cleanupStreams() error {
	config := cmn.GCO.Get()
	reqNetwork := cmn.NetworkIntraControl
	if !config.Net.UseIntraControl {
		reqNetwork = cmn.NetworkPublic
	}
	// Responses to the other targets are objects that is why we want to use
	// intraData network.
	respNetwork := cmn.NetworkIntraData
	if !config.Net.UseIntraData {
		respNetwork = cmn.NetworkPublic
	}

	if ds.streams.request != nil {
		trname := fmt.Sprintf(recvReqStreamNameFmt, ds.m.ManagerUUID)
		if err := transport.Unregister(reqNetwork, trname); err != nil {
			return errors.WithStack(err)
		}
	}

	if ds.streams.response != nil {
		trname := fmt.Sprintf(recvRespStreamNameFmt, ds.m.ManagerUUID)
		if err := transport.Unregister(respNetwork, trname); err != nil {
			return errors.WithStack(err)
		}
	}

	for _, streamBundle := range []*bundle.Streams{ds.streams.request, ds.streams.response} {
		if streamBundle != nil {
			streamBundle.Close(!ds.m.aborted())
		}
	}
	return nil
}

func (ds *dsorterGeneral) cleanup() {
	ds.mw.stop()
}

func (ds *dsorterGeneral) finalCleanup() error {
	return ds.cleanupStreams()
}

func (ds *dsorterGeneral) postRecordDistribution() {
	// In shard creation we should not expect memory increase (at least
	// not from dSort). Also it would be really hard to have concurrent
	// sends and memory cleanup. We must stop before sending records
	// because it affects content of the records.
	ds.mw.stopWatchingExcess()
}

// createShardsLocally waits until it's given the signal to start creating
// shards, then creates shards in parallel.
func (ds *dsorterGeneral) createShardsLocally() (err error) {
	phaseInfo := &ds.m.creationPhase

	ds.creationPhase.adjuster.start()
	defer ds.creationPhase.adjuster.stop()

	metrics := ds.m.Metrics.Creation
	metrics.begin()
	defer metrics.finish()
	metrics.Lock()
	metrics.ToCreate = int64(len(phaseInfo.metadata.Shards))
	metrics.Unlock()

	group, ctx := errgroup.WithContext(context.Background())

CreateAllShards:
	for _, s := range phaseInfo.metadata.Shards {
		select {
		case <-ds.m.listenAborted():
			_ = group.Wait()
			return newDsortAbortedError(ds.m.ManagerUUID)
		case <-ctx.Done():
			break CreateAllShards // context was canceled, therefore we have an error
		default:
		}

		ds.creationPhase.adjuster.acquireGoroutineSema()
		group.Go(func(s *extract.Shard) func() error {
			return func() error {
				err := ds.m.createShard(s)
				ds.creationPhase.adjuster.releaseGoroutineSema()
				return err
			}
		}(s))
	}

	return group.Wait()
}

func (ds *dsorterGeneral) preShardCreation(shardName string, mpathInfo *fs.MountpathInfo) error {
	ds.creationPhase.adjuster.acquireSema(mpathInfo)
	return nil
}

func (ds *dsorterGeneral) postShardCreation(mpathInfo *fs.MountpathInfo) {
	ds.creationPhase.adjuster.releaseSema(mpathInfo)
}

// loadContent returns function to load content from local storage (either disk or memory).
func (ds *dsorterGeneral) loadContent() extract.LoadContentFunc {
	return func(w io.Writer, rec *extract.Record, obj *extract.RecordObj) (int64, error) {
		loadLocal := func(w io.Writer) (written int64, err error) {
			var (
				slab      *memsys.Slab
				buf       []byte
				storeType = obj.StoreType
			)

			if storeType != extract.SGLStoreType { // SGL does not need buffer as it is buffer itself
				buf, slab = mm.Alloc(obj.Size)
			}

			defer func() {
				if storeType != extract.SGLStoreType {
					slab.Free(buf)
				}
				ds.m.decrementRef(1)
			}()

			fullContentPath := ds.m.recManager.FullContentPath(obj)

			if ds.m.rs.DryRun {
				r := cmn.NopReader(obj.MetadataSize + obj.Size)
				written, err = io.CopyBuffer(w, r, buf)
				return
			}

			var n int64
			switch obj.StoreType {
			case extract.OffsetStoreType:
				f, err := os.Open(fullContentPath) // TODO: it should be open always
				if err != nil {
					return written, errors.WithMessage(err, "(offset) open local content failed")
				}
				defer cmn.Close(f)
				_, err = f.Seek(obj.Offset-obj.MetadataSize, io.SeekStart)
				if err != nil {
					return written, errors.WithMessage(err, "(offset) seek local content failed")
				}
				if n, err = io.CopyBuffer(w, io.LimitReader(f, obj.MetadataSize+obj.Size), buf); err != nil {
					return written, errors.WithMessage(err, "(offset) copy local content failed")
				}
			case extract.SGLStoreType:
				v, ok := ds.m.recManager.RecordContents().Load(fullContentPath)
				cmn.AssertMsg(ok, fullContentPath)
				ds.m.recManager.RecordContents().Delete(fullContentPath)
				sgl := v.(*memsys.SGL)
				defer sgl.Free()
				if n, err = io.CopyBuffer(w, sgl, buf); err != nil {
					return written, errors.WithMessage(err, "(sgl) copy local content failed")
				}
			case extract.DiskStoreType:
				f, err := os.Open(fullContentPath)
				if err != nil {
					return written, errors.WithMessage(err, "(disk) open local content failed")
				}
				defer cmn.Close(f)
				if n, err = io.CopyBuffer(w, f, buf); err != nil {
					return written, errors.WithMessage(err, "(disk) copy local content failed")
				}
			default:
				cmn.Assert(false)
			}

			debug.Assert(n > 0)
			written += n
			return
		}

		loadRemote := func(w io.Writer, daemonID string) (int64, error) {
			var (
				cbErr      error
				beforeRecv int64
				beforeSend int64

				wg      = cmn.NewTimeoutGroup()
				writer  = ds.newStreamWriter(rec.MakeUniqueName(obj), w)
				metrics = ds.m.Metrics.Creation

				toNode = ds.m.smap.GetTarget(daemonID)
			)

			if toNode == nil {
				return 0, errors.Errorf("tried to send request to node %q which is not present in the smap", daemonID)
			}

			req := remoteRequest{
				DaemonID:  ds.m.ctx.node.DaemonID,
				Record:    rec,
				RecordObj: obj,
			}
			opaque := cmn.MustMarshal(req)
			hdr := transport.ObjHdr{
				Opaque: opaque,
			}

			if ds.m.Metrics.extended {
				beforeSend = mono.NanoTime()
			}

			cb := func(hdr transport.ObjHdr, r io.ReadCloser, _ unsafe.Pointer, err error) {
				if err != nil {
					cbErr = err
				}
				if ds.m.Metrics.extended {
					delta := mono.Since(beforeSend)
					metrics.Lock()
					metrics.RequestStats.updateTime(delta)
					metrics.Unlock()

					ds.m.ctx.stats.AddMany(
						stats.NamedVal64{Name: stats.DSortCreationReqCount, Value: 1},
						stats.NamedVal64{Name: stats.DSortCreationReqLatency, Value: int64(delta)},
					)
				}
				wg.Done()
			}

			wg.Add(1)
			if err := ds.streams.request.Send(transport.Obj{Hdr: hdr, Callback: cb}, nil, toNode); err != nil {
				return 0, errors.WithStack(err)
			}

			// Send should be synchronous to make sure that 'wait timeout' is
			// calculated only for receive side.
			wg.Wait()

			if cbErr != nil {
				return 0, errors.WithStack(cbErr)
			}

			if ds.m.Metrics.extended {
				beforeRecv = mono.NanoTime()
			}

			// It may happen that the target we are trying to contact was
			// aborted or for some reason is not responding. Thus we need to do
			// some precaution and wait for the content only for limited time or
			// until we receive abort signal.
			var pulled bool
			timed, stopped := writer.wg.WaitTimeoutWithStop(ds.m.callTimeout, ds.m.listenAborted())
			if timed || stopped {
				// In case of time out or abort we need to pull the writer to
				// avoid concurrent Close and Write on `writer.w`.
				pulled = ds.pullStreamWriter(rec.MakeUniqueName(obj)) != nil
			}

			if ds.m.Metrics.extended {
				delta := mono.Since(beforeRecv)
				metrics.Lock()
				metrics.ResponseStats.updateTime(delta)
				metrics.Unlock()

				ds.m.ctx.stats.AddMany(
					stats.NamedVal64{Name: stats.DSortCreationRespCount, Value: 1},
					stats.NamedVal64{Name: stats.DSortCreationRespLatency, Value: int64(delta)},
				)
			}

			// If we timed out or were stopped but we didn't manage to pull the
			// writer then this means that someone else did it and we barely
			// missed. In this case we should wait for the job to be finished -
			// in case of being stopped we should receive error anyway.
			if !pulled {
				if timed || stopped {
					writer.wg.Wait()
				}
			} else {
				// We managed to pull the writer, we can safely return error.
				var err error
				if stopped {
					err = cmn.NewAbortedError("wait for remote content")
				} else if timed {
					err = errors.Errorf("wait for remote content has timed out (%q was waiting for %q)", ds.m.ctx.node.DaemonID, daemonID)
				} else {
					cmn.AssertMsg(false, "pulled but not stopped or timed?!")
				}
				return 0, err
			}

			return writer.n, writer.err
		}

		if ds.m.aborted() {
			return 0, newDsortAbortedError(ds.m.ManagerUUID)
		}

		if rec.DaemonID != ds.m.ctx.node.DaemonID { // File source contents are located on a different target.
			return loadRemote(w, rec.DaemonID)
		}

		// Load from local source
		return loadLocal(w)
	}
}

func (ds *dsorterGeneral) makeRecvRequestFunc() transport.Receive {
	errHandler := func(err error, hdr transport.ObjHdr, node *cluster.Snode) {
		hdr.Opaque = []byte(err.Error())
		hdr.ObjAttrs.Size = 0
		if err = ds.streams.response.Send(transport.Obj{Hdr: hdr}, nil, node); err != nil {
			ds.m.abort(err)
		}
	}

	return func(w http.ResponseWriter, hdr transport.ObjHdr, object io.Reader, err error) {
		req := remoteRequest{}
		if err := jsoniter.Unmarshal(hdr.Opaque, &req); err != nil {
			ds.m.abort(fmt.Errorf("received damaged request: %s", err))
			return
		}

		fromNode := ds.m.smap.GetTarget(req.DaemonID)
		if fromNode == nil {
			glog.Errorf("received request from node %q which is not present in the smap", req.DaemonID)
			return
		}

		if err != nil {
			errHandler(err, hdr, fromNode)
			return
		}

		respHdr := transport.ObjHdr{
			ObjName: req.Record.MakeUniqueName(req.RecordObj),
		}

		if ds.m.aborted() {
			return
		}

		var beforeSend int64
		if ds.m.Metrics.extended {
			beforeSend = mono.NanoTime()
		}

		fullContentPath := ds.m.recManager.FullContentPath(req.RecordObj)

		if ds.m.rs.DryRun {
			lr := cmn.NopReader(req.RecordObj.MetadataSize + req.RecordObj.Size)
			r := cmn.NopOpener(ioutil.NopCloser(lr))
			respHdr.ObjAttrs.Size = req.RecordObj.MetadataSize + req.RecordObj.Size
			o := transport.Obj{Hdr: respHdr, Callback: ds.responseCallback, CmplPtr: unsafe.Pointer(&beforeSend)}
			if err := ds.streams.response.Send(o, r, fromNode); err != nil {
				ds.m.abort(err)
			}
			return
		}

		switch req.RecordObj.StoreType {
		case extract.OffsetStoreType:
			f, err := cmn.NewFileHandle(fullContentPath)
			if err != nil {
				errHandler(err, respHdr, fromNode)
				return
			}
			respHdr.ObjAttrs.Size = req.RecordObj.MetadataSize + req.RecordObj.Size
			r, err := cmn.NewFileSectionHandle(f, req.RecordObj.Offset-req.RecordObj.MetadataSize,
				respHdr.ObjAttrs.Size, 0)
			if err != nil {
				cmn.Close(f)
				errHandler(err, respHdr, fromNode)
				return
			}
			o := transport.Obj{Hdr: respHdr, Callback: ds.responseCallback, CmplPtr: unsafe.Pointer(&beforeSend)}
			if err := ds.streams.response.Send(o, r, fromNode); err != nil {
				cmn.Close(f)
				ds.m.abort(err)
			}
		case extract.SGLStoreType:
			v, ok := ds.m.recManager.RecordContents().Load(fullContentPath)
			cmn.AssertMsg(ok, fullContentPath)
			ds.m.recManager.RecordContents().Delete(fullContentPath)
			sgl := v.(*memsys.SGL)
			respHdr.ObjAttrs.Size = sgl.Size()
			o := transport.Obj{Hdr: respHdr, Callback: ds.responseCallback, CmplPtr: unsafe.Pointer(&beforeSend)}
			if err := ds.streams.response.Send(o, sgl, fromNode); err != nil {
				sgl.Free()
				ds.m.abort(err)
			}
		case extract.DiskStoreType:
			f, err := cmn.NewFileHandle(fullContentPath)
			if err != nil {
				errHandler(err, respHdr, fromNode)
				return
			}
			fi, err := f.Stat()
			if err != nil {
				cmn.Close(f)
				errHandler(err, respHdr, fromNode)
				return
			}
			respHdr.ObjAttrs.Size = fi.Size()
			o := transport.Obj{Hdr: respHdr, Callback: ds.responseCallback, CmplPtr: unsafe.Pointer(&beforeSend)}
			if err := ds.streams.response.Send(o, f, fromNode); err != nil {
				cmn.Close(f)
				ds.m.abort(err)
			}
		default:
			cmn.Assert(false)
		}
	}
}

func (ds *dsorterGeneral) responseCallback(hdr transport.ObjHdr, rc io.ReadCloser, x unsafe.Pointer, err error) {
	if ds.m.Metrics.extended {
		dur := mono.Since(*(*int64)(x))
		ds.m.Metrics.Creation.Lock()
		ds.m.Metrics.Creation.LocalSendStats.updateTime(dur)
		ds.m.Metrics.Creation.LocalSendStats.updateThroughput(hdr.ObjAttrs.Size, dur)
		ds.m.Metrics.Creation.Unlock()
	}

	if sgl, ok := rc.(*memsys.SGL); ok {
		sgl.Free()
	}
	ds.m.decrementRef(1)
	if err != nil {
		ds.m.abort(err)
	}
}

func (ds *dsorterGeneral) postExtraction() {
	ds.mw.stopWatchingReserved()
}

func (ds *dsorterGeneral) makeRecvResponseFunc() transport.Receive {
	metrics := ds.m.Metrics.Creation
	return func(w http.ResponseWriter, hdr transport.ObjHdr, object io.Reader, err error) {
		if err != nil {
			ds.m.abort(err)
			return
		}

		defer cmn.DrainReader(object) // drain to prevent unnecessary stream errors

		writer := ds.pullStreamWriter(hdr.ObjName)
		if writer == nil { // was removed after timing out
			return
		}

		if len(hdr.Opaque) > 0 {
			writer.n, writer.err = 0, errors.New(string(hdr.Opaque))
			writer.wg.Done()
			return
		}

		var beforeSend int64
		if ds.m.Metrics.extended {
			beforeSend = mono.NanoTime()
		}

		buf, slab := mm.Alloc(hdr.ObjAttrs.Size)
		writer.n, writer.err = io.CopyBuffer(writer.w, object, buf)
		writer.wg.Done()
		slab.Free(buf)

		if ds.m.Metrics.extended {
			dur := mono.Since(beforeSend)
			metrics.Lock()
			metrics.LocalRecvStats.updateTime(dur)
			metrics.LocalRecvStats.updateThroughput(writer.n, dur)
			metrics.Unlock()
		}
	}
}

func (ds *dsorterGeneral) preShardExtraction(expectedUncompressedSize uint64) bool {
	return ds.mw.reserveMem(expectedUncompressedSize)
}

func (ds *dsorterGeneral) postShardExtraction(expectedUncompressedSize uint64) {
	ds.mw.unreserveMem(expectedUncompressedSize)
}

func (ds *dsorterGeneral) onAbort() {
	_ = ds.cleanupStreams()
}
