// Package ec provides erasure coding (EC) based data protection for AIStore.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ec

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/transport/bundle"
	"github.com/NVIDIA/aistore/xaction"
)

const (
	requestBufSizeGlobal = 140
	requestBufSizeFS     = 70
	requestBufSizeEncode = 16
	maxBgJobsPerJogger   = 32
)

type (
	xactECBase struct {
		xaction.XactDemandBase
		t cluster.Target

		smap  cluster.Sowner // cluster map
		si    *cluster.Snode // target daemonInfo
		stats stats          // EC statistics
		bck   cmn.Bck        // which bucket xact belongs to

		dOwner *dataOwner // data slice manager

		reqBundle  *bundle.Streams // a stream bundle to send lightweight requests
		respBundle *bundle.Streams // a stream bungle to transfer data between targets
	}

	xactReqBase struct {
		mpathReqCh chan mpathReq // notify about mountpath changes
		ecCh       chan *Request // to request object encoding

		controlCh chan RequestsControlMsg

		rejectReq atomic.Bool // marker if EC requests should be rejected
	}

	mpathReq struct {
		action string
		mpath  string
	}

	// Manages SGL objects that are waiting for a data from a remote target
	dataOwner struct {
		mtx    sync.Mutex
		slices map[string]*slice
	}
)

func newXactReqECBase() xactReqBase {
	return xactReqBase{
		mpathReqCh: make(chan mpathReq, 1),
		ecCh:       make(chan *Request, requestBufSizeGlobal),
		controlCh:  make(chan RequestsControlMsg, 8),
	}
}

func newXactECBase(t cluster.Target, smap cluster.Sowner,
	si *cluster.Snode, bck cmn.Bck, reqBundle, respBundle *bundle.Streams) xactECBase {
	return xactECBase{
		t:     t,
		smap:  smap,
		si:    si,
		stats: stats{bck: bck},
		bck:   bck,

		dOwner: &dataOwner{
			mtx:    sync.Mutex{},
			slices: make(map[string]*slice, 10),
		},

		reqBundle:  reqBundle,
		respBundle: respBundle,
	}
}

// ClearRequests disables receiving new EC requests, they will be terminated with error
// Then it starts draining a channel from pending EC requests
// It does not enable receiving new EC requests, it has to be done explicitly, when EC is enabled again
func (r *xactReqBase) ClearRequests() {
	msg := RequestsControlMsg{
		Action: ActClearRequests,
	}

	r.controlCh <- msg
}

func (r *xactReqBase) EnableRequests() {
	msg := RequestsControlMsg{
		Action: ActEnableRequests,
	}

	r.controlCh <- msg
}

func (r *xactReqBase) setEcRequestsDisabled() {
	r.rejectReq.Store(true)
}

func (r *xactReqBase) setEcRequestsEnabled() {
	r.rejectReq.Store(false)
}

func (r *xactReqBase) ecRequestsEnabled() bool {
	return !r.rejectReq.Load()
}

// Create a request header: initializes the `Sender` field with local target's
// daemon ID, and sets `Exists:true` that means "local object exists".
// Later `Exists` can be changed to `false` if local file is unreadable or does
// not exist
func (r *xactECBase) newIntraReq(act intraReqType, meta *Metadata) *intraReq {
	req := &intraReq{
		act:    act,
		sender: r.si.ID(),
		meta:   meta,
		exists: true,
	}
	if act == reqGet && meta != nil {
		req.isSlice = !meta.IsCopy
	}
	return req
}

func (r *xactECBase) IsMountpathXact() bool { return true }

func (r *xactECBase) newSliceResponse(md *Metadata, attrs *transport.ObjectAttrs, fqn string) (reader cmn.ReadOpenCloser, err error) {
	attrs.Version = md.ObjVersion
	attrs.CksumType = md.CksumType
	attrs.CksumValue = md.CksumValue

	stat, err := os.Stat(fqn)
	if err != nil {
		return nil, err
	}
	attrs.Size = stat.Size()
	reader, err = cmn.NewFileHandle(fqn)
	if err != nil {
		glog.Warningf("Failed to read file stats: %s", err)
		return nil, err
	}
	return reader, nil
}

// replica/full object request
func (r *xactECBase) newReplicaResponse(attrs *transport.ObjectAttrs, bck *cluster.Bck, objName string) (reader cmn.ReadOpenCloser, err error) {
	lom := &cluster.LOM{T: r.t, ObjName: objName}
	err = lom.Init(bck.Bck)
	if err != nil {
		glog.Warning(err)
		return nil, err
	}
	if err = lom.Load(); err != nil {
		glog.Warning(err)
		return nil, err
	}
	reader, err = cmn.NewFileHandle(lom.FQN)
	if err != nil {
		return nil, err
	}
	if lom.Size() == 0 {
		return nil, nil
	}
	attrs.Size = lom.Size()
	attrs.Version = lom.Version()
	attrs.Atime = lom.AtimeUnix()
	if lom.Cksum() != nil {
		attrs.CksumType, attrs.CksumValue = lom.Cksum().Get()
	}
	return reader, nil
}

// Sends the replica/meta/slice data: either to copy replicas/slices after
// encoding or to send requested "object" to a client. In the latter case
// if the local object does not exist, it sends an empty body and sets
// exists=false in response header
func (r *xactECBase) dataResponse(act intraReqType, fqn string, bck *cluster.Bck, objName, id string, md *Metadata) (err error) {
	var (
		reader   cmn.ReadOpenCloser
		objAttrs transport.ObjectAttrs
	)
	ireq := r.newIntraReq(act, nil)
	if md != nil && md.SliceID != 0 {
		// slice request
		reader, err = r.newSliceResponse(md, &objAttrs, fqn)
		ireq.exists = err == nil
	} else {
		// replica/full object request
		reader, err = r.newReplicaResponse(&objAttrs, bck, objName)
		ireq.exists = err == nil
	}
	cmn.Assert((objAttrs.Size == 0 && reader == nil) || (objAttrs.Size != 0 && reader != nil))

	rHdr := transport.ObjHdr{
		Bck:      bck.Bck,
		ObjName:  objName,
		ObjAttrs: objAttrs,
	}
	rHdr.Opaque = ireq.NewPack(r.t.SmallMMSA())

	r.ObjectsInc()
	r.BytesAdd(objAttrs.Size)

	cb := func(hdr transport.ObjHdr, c io.ReadCloser, _ unsafe.Pointer, err error) {
		r.t.SmallMMSA().Free(hdr.Opaque)
		if err != nil {
			glog.Errorf("Failed to send %s/%s: %v", hdr.Bck, hdr.ObjName, err)
		}
	}
	return r.sendByDaemonID([]string{id}, rHdr, reader, cb, false)
}

// Send a data or request to one or few targets by their DaemonIDs. Most of the time
// only DaemonID is known - that is why the function gets DaemonID and internally
// transforms it into cluster.Snode.
// * daemonIDs - a list of targets
// * hdr - transport header
// * reader - a data to send
// * cb - optional callback to be called when the transfer completes
// * isRequest - defines the type of request:
//		- true - send lightweight request to all targets (usually reader is nil
//			in this case)
//	    - false - send a slice/replica/metadata to targets
func (r *xactECBase) sendByDaemonID(daemonIDs []string, hdr transport.ObjHdr,
	reader cmn.ReadOpenCloser, cb transport.SendCallback, isRequest bool) error {
	nodes := make([]*cluster.Snode, 0, len(daemonIDs))
	smap := r.smap.Get()
	for _, id := range daemonIDs {
		si, ok := smap.Tmap[id]
		if !ok {
			glog.Errorf("Target with ID %s not found", id)
			continue
		}
		nodes = append(nodes, si)
	}

	if len(nodes) == 0 {
		return errors.New("destination list is empty")
	}

	var err error
	if isRequest {
		err = r.reqBundle.Send(transport.Obj{Hdr: hdr, Callback: cb}, reader, nodes...)
	} else {
		err = r.respBundle.Send(transport.Obj{Hdr: hdr, Callback: cb}, reader, nodes...)
	}
	return err
}

// send request to a target, wait for its response, read the data into writer.
// * daemonID - target to send a request
// * bucket/objName - what to request
// * uname - unique name for the operation: the name is built from daemonID,
//		bucket and object names. HTTP data receiving handler generates a name
//		when receiving data and if it finds a writer registered with the same
//		name, it puts the data to its writer and notifies when download is done
// * request - request to send
// * writer - an opened writer that will receive the replica/slice/meta
func (r *xactECBase) readRemote(lom *cluster.LOM, daemonID, uname string, request []byte, writer io.Writer) (int64, error) {
	hdr := transport.ObjHdr{
		Bck:     lom.Bck().Bck,
		ObjName: lom.ObjName,
		Opaque:  request,
	}

	sw := &slice{
		writer: writer,
		wg:     cmn.NewTimeoutGroup(),
		lom:    lom,
	}

	sw.wg.Add(1)
	r.regWriter(uname, sw)

	if glog.V(4) {
		glog.Infof("Requesting object %s/%s from %s", lom.Bck(), lom.ObjName, daemonID)
	}
	if err := r.sendByDaemonID([]string{daemonID}, hdr, nil, nil, true); err != nil {
		r.unregWriter(uname)
		return 0, err
	}
	c := cmn.GCO.Get()
	if sw.wg.WaitTimeout(c.Timeout.SendFile) {
		r.unregWriter(uname)
		return 0, fmt.Errorf("timed out waiting for %s is read", uname)
	}
	r.unregWriter(uname)
	lom.Uncache()
	if glog.V(4) {
		glog.Infof("Received object %s/%s from %s", lom.Bck(), lom.ObjName, daemonID)
	}
	return sw.n, nil
}

// Registers a new slice that will wait for the data to come from
// a remote target
func (r *xactECBase) regWriter(uname string, writer *slice) bool {
	r.dOwner.mtx.Lock()
	_, ok := r.dOwner.slices[uname]
	if ok {
		glog.Errorf("Writer for %s is already registered", uname)
	} else {
		r.dOwner.slices[uname] = writer
	}
	r.dOwner.mtx.Unlock()

	return !ok
}

// Unregisters a slice that has been waiting for the data to come from
// a remote target
func (r *xactECBase) unregWriter(uname string) {
	r.dOwner.mtx.Lock()
	delete(r.dOwner.slices, uname)
	r.dOwner.mtx.Unlock()
}

// Used to copy replicas/slices after the object is encoded after PUT/restored
// after GET, or to respond to meta/slice/replica request.
// * daemonIDs - receivers of the data
// * bucket/objName - object path
// * reader - object/slice/meta data
// * src - extra information about the data to send
// * cb - a caller may set its own callback to execute when the transfer is done.
//		A special case:
//		if a caller does not define its own callback, and it sets the `obj` in
//		`src` it means that the caller wants to automatically free the memory
//		allocated for the `obj` SGL after the object is transferred. The caller
//		may set optional counter in `obj` - the default callback decreases the
//		counter each time the callback is called and when the value drops below 1,
//		`writeRemote` callback frees the SGL
//      The counter is used for sending slices of one big SGL to a few nodes. In
//		this case every slice must be sent to only one target, and transport bundle
//		cannot help to track automatically when SGL should be freed.
func (r *xactECBase) writeRemote(daemonIDs []string, lom *cluster.LOM, src *dataSource, cb transport.SendCallback) error {
	if src.metadata != nil && src.metadata.ObjVersion == "" {
		src.metadata.ObjVersion = lom.Version()
	}
	req := r.newIntraReq(src.reqType, src.metadata)
	req.isSlice = src.isSlice

	mm := r.t.SmallMMSA()
	putData := req.NewPack(mm)
	objAttrs := transport.ObjectAttrs{
		Size:    src.size,
		Version: lom.Version(),
		Atime:   lom.AtimeUnix(),
	}
	if src.metadata != nil && src.metadata.SliceID != 0 {
		// for a slice read everything from slice's metadata
		if src.metadata.ObjVersion != "" {
			objAttrs.Version = src.metadata.ObjVersion
		}
		if src.metadata.CksumType != "" && src.metadata.CksumValue != "" {
			objAttrs.CksumType, objAttrs.CksumValue = src.metadata.CksumType, src.metadata.CksumValue
		}
	} else if lom.Cksum() != nil {
		objAttrs.CksumType, objAttrs.CksumValue = lom.Cksum().Get()
	}
	hdr := transport.ObjHdr{
		Bck:      lom.Bck().Bck,
		ObjName:  lom.ObjName,
		ObjAttrs: objAttrs,
		Opaque:   putData,
	}
	if cb == nil && src.obj != nil {
		obj := src.obj
		cb = func(hdr transport.ObjHdr, reader io.ReadCloser, _ unsafe.Pointer, err error) {
			mm.Free(hdr.Opaque)
			if obj != nil {
				obj.release()
			}
			if err != nil {
				glog.Errorf("Failed to send %s/%s to %v: %v", lom.Bck(), lom.ObjName, daemonIDs, err)
			}
		}
	} else {
		// wrapper to properly cleanup memory allocated by MMSA
		oldCallback := cb
		cb = func(hdr transport.ObjHdr, reader io.ReadCloser, ptr unsafe.Pointer, err error) {
			mm.Free(hdr.Opaque)
			if oldCallback != nil {
				oldCallback(hdr, reader, ptr, err)
			}
		}
	}
	return r.sendByDaemonID(daemonIDs, hdr, src.reader, cb, false)
}

// Save data from a target response to SGL or file. When exists is false it
// just drains the response body and returns - because it does not contain
// any data. On completion the function must call writer.wg.Done to notify
// the caller that the data read is completed.
// * writer - where to save the slice/meta/replica data
// * exists - if the remote target had the requested object
// * reader - response body
func (r *xactECBase) writerReceive(writer *slice, exists bool, objAttrs transport.ObjectAttrs,
	reader io.Reader) (err error) {
	if !exists {
		writer.wg.Done()
		// drain the body, to avoid panic:
		// http: panic serving: assertion failed: "expecting an error or EOF as the reason for failing to read
		cmn.DrainReader(reader)
		return ErrorNotFound
	}

	buf, slab := mm.Alloc()
	writer.n, err = io.CopyBuffer(writer.writer, reader, buf)
	writer.cksum = cmn.NewCksum(objAttrs.CksumType, objAttrs.CksumValue)
	if writer.version != "" && objAttrs.Version != "" {
		writer.version = objAttrs.Version
	}

	writer.wg.Done()
	slab.Free(buf)
	return err
}

func (r *xactECBase) ECStats() *ECStats {
	return r.stats.stats()
}

type (
	BckXacts struct {
		get atomic.Pointer // *XactGet
		put atomic.Pointer // *XactPut
		req atomic.Pointer // *XactRespond
	}
)

func (xacts *BckXacts) Get() *XactGet {
	return (*XactGet)(xacts.get.Load())
}

func (xacts *BckXacts) Put() *XactPut {
	return (*XactPut)(xacts.put.Load())
}

func (xacts *BckXacts) Req() *XactRespond {
	return (*XactRespond)(xacts.req.Load())
}

func (xacts *BckXacts) SetGet(xact *XactGet) {
	xacts.get.Store(unsafe.Pointer(xact))
}

func (xacts *BckXacts) SetPut(xact *XactPut) {
	xacts.put.Store(unsafe.Pointer(xact))
}

func (xacts *BckXacts) SetReq(xact *XactRespond) {
	xacts.req.Store(unsafe.Pointer(xact))
}

func (xacts *BckXacts) AbortGet() {
	xact := (*XactGet)(xacts.get.Load())
	if xact != nil && !xact.Finished() {
		xact.Abort()
	}
}

func (xacts *BckXacts) AbortPut() {
	xact := (*XactPut)(xacts.put.Load())
	if xact != nil && !xact.Finished() {
		xact.Abort()
	}
}
