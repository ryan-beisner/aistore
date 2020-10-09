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
	"sync"
	"unsafe"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/dsort/extract"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/sys"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/transport/bundle"
	jsoniter "github.com/json-iterator/go"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

// This implementation of dsorter focuses on creation phase and maximizing
// memory usage in that phase. It has an active push mechanism which instead
// of waiting for requests, it sends all the record objects it has for the
// shard other target is building. The requirement for using this dsorter
// implementation is a lot of memory available. In creation phase target
// needs to have enough memory to build a given shard all in memory otherwise
// it would be easy to deadlock when targets would send the record objects in
// incorrect order.

const (
	DSorterMemType = "dsort_mem"
)

type (
	rwConnection struct {
		r   io.Reader
		wgr *cmn.TimeoutGroup
		// In case the reader is first to connect, the data is copied into SGL
		// so that the reader will not block on the connection.
		sgl *memsys.SGL

		w   io.Writer
		wgw *sync.WaitGroup

		n int64
	}

	rwConnector struct {
		mu          sync.Mutex
		m           *Manager
		connections map[string]*rwConnection
	}

	dsorterMem struct {
		m *Manager

		streams struct {
			builder *bundle.Streams // streams for sending information about building shards
			records *bundle.Streams // streams for sending the record
		}

		creationPhase struct {
			connector       *rwConnector // used to connect readers (streams, local data) with writers (shards)
			requestedShards chan string

			adjuster struct {
				read  *concAdjuster
				write *concAdjuster
			}
		}
	}
)

var _ dsorter = &dsorterMem{}

func newRWConnection(r io.Reader, w io.Writer) *rwConnection {
	cmn.Assert(r != nil || w != nil)
	wgr := cmn.NewTimeoutGroup()
	wgr.Add(1)
	wgw := &sync.WaitGroup{}
	wgw.Add(1)
	return &rwConnection{
		r:   r,
		w:   w,
		wgr: wgr,
		wgw: wgw,
	}
}

func newRWConnector(m *Manager) *rwConnector {
	return &rwConnector{
		m:           m,
		connections: make(map[string]*rwConnection, 1000),
	}
}

func (c *rwConnector) free() {
	c.mu.Lock()
	for _, v := range c.connections {
		if v.sgl != nil {
			v.sgl.Free()
		}
	}
	c.mu.Unlock()
}

func (c *rwConnector) connect(key string, r io.Reader, w io.Writer) (rwc *rwConnection, all bool) {
	var ok bool

	if rwc, ok = c.connections[key]; !ok {
		rwc = newRWConnection(r, w)
		c.connections[key] = rwc
	} else {
		if rwc.r == nil {
			rwc.r = r
		}
		if rwc.w == nil {
			rwc.w = w
		}
	}
	all = rwc.r != nil && rwc.w != nil
	return
}

func (c *rwConnector) connectReader(key string, r io.Reader, size int64) (err error) {
	c.mu.Lock()
	rw, all := c.connect(key, r, nil)
	c.mu.Unlock()

	if !all {
		rw.sgl = mm.NewSGL(size)
		_, err = io.Copy(rw.sgl, r)
		rw.wgr.Done()
		return
	}

	rw.wgr.Done()
	rw.wgw.Wait()
	return
}

func (c *rwConnector) connectWriter(key string, w io.Writer) (int64, error) {
	c.mu.Lock()
	rw, all := c.connect(key, nil, w)
	c.mu.Unlock()
	defer rw.wgw.Done() // inform the reader that the copying has finished

	timed, stopped := rw.wgr.WaitTimeoutWithStop(c.m.callTimeout, c.m.listenAborted()) // wait for reader
	if timed {
		return 0, errors.Errorf("wait for remote content has timed out (%q was waiting)", c.m.ctx.node.DaemonID)
	} else if stopped {
		return 0, errors.Errorf("wait for remote content was aborted")
	}

	if all { // reader connected and left SGL with the content
		n, err := io.Copy(rw.w, rw.sgl)
		rw.sgl.Free()
		rw.sgl = nil
		return n, err
	}

	n, err := io.CopyBuffer(rw.w, rw.r, nil)
	rw.n = n
	return n, err
}

func newDSorterMem(m *Manager) *dsorterMem {
	return &dsorterMem{
		m: m,
	}
}

func (ds *dsorterMem) name() string { return DSorterMemType }

func (ds *dsorterMem) init() error {
	ds.creationPhase.connector = newRWConnector(ds.m)
	ds.creationPhase.requestedShards = make(chan string, 10000)

	ds.creationPhase.adjuster.read = newConcAdjuster(
		ds.m.rs.CreateConcMaxLimit,
		1, /*goroutineLimitCoef*/
	)
	ds.creationPhase.adjuster.write = newConcAdjuster(
		ds.m.rs.CreateConcMaxLimit,
		1, /*goroutineLimitCoef*/
	)
	return nil
}

func (ds *dsorterMem) start() error {
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

	ds.streams.builder = bundle.NewStreams(ds.m.ctx.smapOwner, ds.m.ctx.node, client, reqSbArgs)
	ds.streams.records = bundle.NewStreams(ds.m.ctx.smapOwner, ds.m.ctx.node, client, respSbArgs)
	return nil
}

func (ds *dsorterMem) cleanupStreams() error {
	cfg := cmn.GCO.Get()
	reqNetwork := cmn.NetworkIntraControl
	if !cfg.Net.UseIntraControl {
		reqNetwork = cmn.NetworkPublic
	}
	// Responses to the other targets are objects that is why we want to use
	// intraData network.
	respNetwork := cmn.NetworkIntraData
	if !cfg.Net.UseIntraData {
		respNetwork = cmn.NetworkPublic
	}

	if ds.streams.builder != nil {
		trname := fmt.Sprintf(recvReqStreamNameFmt, ds.m.ManagerUUID)
		if err := transport.Unregister(reqNetwork, trname); err != nil {
			return errors.WithStack(err)
		}
	}

	if ds.streams.records != nil {
		trname := fmt.Sprintf(recvRespStreamNameFmt, ds.m.ManagerUUID)
		if err := transport.Unregister(respNetwork, trname); err != nil {
			return errors.WithStack(err)
		}
	}

	for _, streamBundle := range []*bundle.Streams{ds.streams.builder, ds.streams.records} {
		if streamBundle != nil {
			streamBundle.Close(!ds.m.aborted())
		}
	}
	return nil
}

func (ds *dsorterMem) cleanup() {}

func (ds *dsorterMem) finalCleanup() error {
	if err := ds.cleanupStreams(); err != nil {
		return err
	}
	close(ds.creationPhase.requestedShards)
	ds.creationPhase.connector.free()
	ds.creationPhase.connector = nil
	return nil
}

func (ds *dsorterMem) postRecordDistribution() {}

func (ds *dsorterMem) preShardCreation(shardName string, mpathInfo *fs.MountpathInfo) error {
	bsi := &buildingShardInfo{
		shardName: shardName,
	}
	opaque := bsi.NewPack(ds.m.ctx.t.SmallMMSA())
	if err := ds.streams.builder.Send(transport.Obj{Hdr: transport.ObjHdr{Opaque: opaque}}, nil); err != nil {
		return err
	}
	ds.creationPhase.requestedShards <- shardName // we also need to inform ourselves
	ds.creationPhase.adjuster.write.acquireSema(mpathInfo)
	return nil
}

func (ds *dsorterMem) postShardCreation(mpathInfo *fs.MountpathInfo) {
	ds.creationPhase.adjuster.write.releaseSema(mpathInfo)
}

func (ds *dsorterMem) loadContent() extract.LoadContentFunc {
	return func(w io.Writer, rec *extract.Record, obj *extract.RecordObj) (int64, error) {
		if ds.m.aborted() {
			return 0, newDsortAbortedError(ds.m.ManagerUUID)
		}

		return ds.creationPhase.connector.connectWriter(rec.MakeUniqueName(obj), w)
	}
}

// createShardsLocally waits until it's given the signal to start creating
// shards, then creates shards in parallel.
func (ds *dsorterMem) createShardsLocally() (err error) {
	phaseInfo := &ds.m.creationPhase

	ds.creationPhase.adjuster.read.start()
	ds.creationPhase.adjuster.write.start()
	defer func() {
		ds.creationPhase.adjuster.write.stop()
		ds.creationPhase.adjuster.read.stop()
	}()

	metrics := ds.m.Metrics.Creation
	metrics.begin()
	defer metrics.finish()
	metrics.Lock()
	metrics.ToCreate = int64(len(phaseInfo.metadata.Shards))
	metrics.Unlock()

	var (
		wg     = &sync.WaitGroup{}
		errCh  = make(chan error, 2)
		stopCh = cmn.NewStopCh()
	)
	defer stopCh.Close()

	mem, err := sys.Mem()
	if err != nil {
		return err
	}
	maxMemoryToUse := calcMaxMemoryUsage(ds.m.rs.MaxMemUsage, mem)
	sa := newInmemShardAllocator(maxMemoryToUse - mem.ActualUsed)

	// read
	wg.Add(1)
	go func() {
		defer wg.Done()
		group, ctx := errgroup.WithContext(context.Background())
	SendAllShards:
		for {
			// If that was last shard to send we need to break and we will
			// be waiting for the result.
			if len(phaseInfo.metadata.SendOrder) == 0 {
				break SendAllShards
			}

			select {
			case shardName := <-ds.creationPhase.requestedShards:
				shard, ok := phaseInfo.metadata.SendOrder[shardName]
				if !ok {
					break
				}

				ds.creationPhase.adjuster.read.acquireGoroutineSema()
				group.Go(func(shard *extract.Shard) func() error {
					return func() error {
						defer ds.creationPhase.adjuster.read.releaseGoroutineSema()

						bck := cluster.NewBck(ds.m.rs.OutputBucket, ds.m.rs.OutputProvider, cmn.NsGlobal)
						if err := bck.Init(ds.m.ctx.bmdOwner, ds.m.ctx.t.Snode()); err != nil {
							return err
						}
						toNode, err := cluster.HrwTarget(bck.MakeUname(shard.Name), ds.m.ctx.smapOwner.Get())
						if err != nil {
							return err
						}

						for _, rec := range shard.Records.All() {
							for _, obj := range rec.Objects {
								if err := ds.sendRecordObj(rec, obj, toNode); err != nil {
									return err
								}
							}
						}
						return nil
					}
				}(shard))

				delete(phaseInfo.metadata.SendOrder, shardName)
			case <-ds.m.listenAborted():
				stopCh.Close()
				group.Wait()
				errCh <- newDsortAbortedError(ds.m.ManagerUUID)
				return
			case <-ctx.Done(): // context was canceled, therefore we have an error
				stopCh.Close()
				break SendAllShards
			case <-stopCh.Listen(): // writing side stopped we need to do the same
				break SendAllShards
			}
		}

		errCh <- group.Wait()
	}()

	// write
	wg.Add(1)
	go func() {
		defer wg.Done()
		group, ctx := errgroup.WithContext(context.Background())
	CreateAllShards:
		for _, s := range phaseInfo.metadata.Shards {
			select {
			case <-ds.m.listenAborted():
				stopCh.Close()
				group.Wait()
				errCh <- newDsortAbortedError(ds.m.ManagerUUID)
				return
			case <-ctx.Done(): // context was canceled, therefore we have an error
				stopCh.Close()
				break CreateAllShards
			case <-stopCh.Listen():
				break CreateAllShards // reading side stopped we need to do the same
			default:
			}

			sa.alloc(uint64(s.Size))

			ds.creationPhase.adjuster.write.acquireGoroutineSema()
			group.Go(func(s *extract.Shard) func() error {
				return func() error {
					err := ds.m.createShard(s)
					ds.creationPhase.adjuster.write.releaseGoroutineSema()
					sa.free(uint64(s.Size))
					return err
				}
			}(s))
		}

		errCh <- group.Wait()
	}()

	wg.Wait()

	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func (ds *dsorterMem) sendRecordObj(rec *extract.Record, obj *extract.RecordObj, toNode *cluster.Snode) (err error) {
	var (
		local = toNode.DaemonID == ds.m.ctx.node.DaemonID
		req   = RemoteResponse{
			Record:    rec,
			RecordObj: obj,
		}
		opaque = cmn.MustMarshal(req)
		hdr    = transport.ObjHdr{
			Opaque: opaque,
		}

		beforeSend int64
	)
	fullContentPath := ds.m.recManager.FullContentPath(obj)
	ct, err := cluster.NewCTFromBO(ds.m.rs.OutputBucket, ds.m.rs.OutputProvider, fullContentPath, nil)
	if err != nil {
		return
	}
	ds.creationPhase.adjuster.read.acquireSema(ct.ParsedFQN().MpathInfo)
	defer ds.creationPhase.adjuster.read.releaseSema(ct.ParsedFQN().MpathInfo)

	// Check if aborted
	if ds.m.aborted() {
		return newDsortAbortedError(ds.m.ManagerUUID)
	}

	if ds.m.Metrics.extended {
		beforeSend = mono.NanoTime()
	}

	cmn.Assert(ds.m.ctx.node.DaemonID == rec.DaemonID)

	if local {
		defer ds.m.decrementRef(1)
	}

	send := func(r cmn.ReadOpenCloser) (err error) {
		if local {
			err = ds.creationPhase.connector.connectReader(rec.MakeUniqueName(obj), r, hdr.ObjAttrs.Size)
			if ds.m.Metrics.extended {
				dur := mono.Since(beforeSend)
				ds.m.Metrics.Creation.Lock()
				ds.m.Metrics.Creation.LocalSendStats.updateTime(dur)
				ds.m.Metrics.Creation.LocalSendStats.updateThroughput(hdr.ObjAttrs.Size, dur)
				ds.m.Metrics.Creation.Unlock()
			}
		} else {
			o := transport.Obj{Hdr: hdr, Callback: ds.m.sentCallback, CmplPtr: unsafe.Pointer(&beforeSend)}
			err = ds.streams.records.Send(o, r, toNode)
		}
		return
	}

	if ds.m.rs.DryRun {
		lr := cmn.NopReader(obj.MetadataSize + obj.Size)
		r := cmn.NopOpener(ioutil.NopCloser(lr))
		hdr.ObjAttrs.Size = obj.MetadataSize + obj.Size
		return send(r)
	}

	switch obj.StoreType {
	case extract.OffsetStoreType:
		f, err := cmn.NewFileHandle(fullContentPath)
		if err != nil {
			return err
		}
		hdr.ObjAttrs.Size = obj.MetadataSize + obj.Size
		r, err := cmn.NewFileSectionHandle(f, obj.Offset-obj.MetadataSize, hdr.ObjAttrs.Size, 0)
		if err != nil {
			cmn.Close(f)
			return err
		}
		if err := send(r); err != nil {
			cmn.Close(f)
			return err
		}
	case extract.DiskStoreType:
		f, err := cmn.NewFileHandle(fullContentPath)
		if err != nil {
			return err
		}
		fi, err := f.Stat()
		if err != nil {
			cmn.Close(f)
			return err
		}
		hdr.ObjAttrs.Size = fi.Size()
		if err := send(f); err != nil {
			cmn.Close(f)
			return err
		}
	default:
		cmn.Assert(false)
	}

	return nil
}

func (ds *dsorterMem) postExtraction() {}

func (ds *dsorterMem) makeRecvRequestFunc() transport.Receive {
	return func(w http.ResponseWriter, hdr transport.ObjHdr, object io.Reader, err error) {
		if err != nil {
			ds.m.abort(err)
			return
		}

		defer cmn.DrainReader(object) // drain to prevent unnecessary stream errors

		unpacker := cmn.NewUnpacker(hdr.Opaque)
		req := buildingShardInfo{}
		if err := unpacker.ReadAny(&req); err != nil {
			ds.m.abort(err)
			return
		}

		if ds.m.aborted() {
			return
		}

		ds.creationPhase.requestedShards <- req.shardName
	}
}

func (ds *dsorterMem) makeRecvResponseFunc() transport.Receive {
	metrics := ds.m.Metrics.Creation
	return func(w http.ResponseWriter, hdr transport.ObjHdr, object io.Reader, err error) {
		if err != nil {
			ds.m.abort(err)
			return
		}

		defer cmn.DrainReader(object) // drain to prevent unnecessary stream errors

		req := RemoteResponse{}
		if err := jsoniter.Unmarshal(hdr.Opaque, &req); err != nil {
			ds.m.abort(err)
			return
		}

		var beforeSend int64
		if ds.m.Metrics.extended {
			beforeSend = mono.NanoTime()
		}

		if ds.m.aborted() {
			return
		}

		uname := req.Record.MakeUniqueName(req.RecordObj)
		if err := ds.creationPhase.connector.connectReader(uname, object, hdr.ObjAttrs.Size); err != nil {
			ds.m.abort(err)
			return
		}

		if ds.m.Metrics.extended {
			dur := mono.Since(beforeSend)
			metrics.Lock()
			metrics.LocalRecvStats.updateTime(dur)
			metrics.LocalRecvStats.updateThroughput(hdr.ObjAttrs.Size, dur)
			metrics.Unlock()
		}
	}
}

func (ds *dsorterMem) preShardExtraction(expectedUncompressedSize uint64) (toDisk bool) {
	return true
}

func (ds *dsorterMem) postShardExtraction(expectedUncompressedSize uint64) {}

func (ds *dsorterMem) onAbort() {
	_ = ds.cleanupStreams()
}
