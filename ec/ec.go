// Package ec provides erasure coding (EC) based data protection for AIStore.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/xaction/registry"
	jsoniter "github.com/json-iterator/go"
)

// EC module provides data protection on a per bucket basis. By default, the
// data protection is off. To enable it, set the bucket EC configuration:
//	ECConf:
//		Enable: true|false    # enables or disables protection
//		DataSlices: [1-32]    # the number of data slices
//		ParitySlices: [1-32]  # the number of parity slices
//		ObjSizeLimit: 0       # replication versus erasure coding
//
// NOTE: replicating small object is cheaper than erasure encoding.
// The ObjSizeLimit option sets the corresponding threshold. Set it to the
// size (in bytes), or 0 (zero) to use the AIStore default 256KiB.
//
// NOTE: ParitySlices defines the maximum number of storage targets a cluster
// can loose but it is still able to restore the original object
//
// NOTE: Since small objects are always replicated, they always have only one
// data slice and #ParitySlices replicas
//
// NOTE: All slices and replicas must be on the different targets. The target
// list is calculated by HrwTargetList. The first target in the list is the
// "main" target that keeps the full object, the others keep only slices/replicas
//
// NOTE: All slices must be of the same size. So, the last slice can be padded
// with zeros. In most cases, padding results in the total size of data
// replicas being a bit bigger than than the size of the original object.
//
// NOTE: Every slice and replica must have corresponding metadata file that is
// located in the same mountpath as its slice/replica
//
//
// EC local storage directories inside mountpaths:
//		/obj/  - for main object and its replicas
//		/ec/   - for object data and parity slices
//		/meta/ - for metadata files
//
//
// Metadata content:
//		size - size of the original object (required for correct restoration)
//		data - the number of data slices (unused if the object was replicated)
//		parity - the number of parity slices
//		copy - whether the object was replicated or erasure encoded
//		chk - original object checksum (used to choose the correct slices when
//			restoring the object, sort of versioning)
//		sliceid - used if the object was encoded, the ordinal number of slice
//			starting from 1 (0 means 'full copy' - either orignal object or
//			its replica)
//
//
// How protection works.
//
// Object PUT:
// 1. The main target - the target that is responsible for keeping the full object
//	  data and for restoring the object in case of it is damaged - is selected by
//	  HrwTarget. A proxy delegates object PUT request to it.
// 2. The main target calculates all other targets to keep slices/replicas. For
//	  small files it is #ParitySlices, for big ones it #DataSlices+#ParitySlices
//	  targets.
// 3. If the object is small, the main target broadcast the replicas.
//    Otherwise, the target calculates data and parity slices, then sends them.
//
// Object GET:
// 1. The main target - the target that is responsible for keeping the full object
//	  data and for restoring the object becomes damaged - is determined by
//	  HrwTarget algorithm. A proxy delegates object GET request to it.
// 2. If the main target has the original object, it sends the data back
//    Otherwise it tries to look up it inside other mountpaths (if resilver
//	  is running) or on remote targets (if rebalance is running).
// 3. If everything fails and EC is enabled for the bucket, the main target
//	  initiates object restoration process:
//    - First, the main target requests for object's metafile from all targets
//	    in the cluster. If no target responds with a valid metafile, the object
//		is considered missing.
//    - Otherwise, the main target tries to download and restore the original data:
//      Replica case:
//	        The main target request targets which have valid metafile for a replica
//			one by one. When a target sends a valid object, the main target saves
//			the object to local storage and reuploads its replicas to the targets.
//      EC case:
//			The main target requests targets which have valid metafile for slices
//			in parallel. When all the targets respond, the main target starts
//			restoring the object, and, in case of success, saves the restored object
//			to local storage and sends recalculated data and parity slices to the
//			targets which must have a slice but are 'empty' at this moment.
// NOTE: the slices are stored on targets in random order, except the first
//	     PUT when the main target stores the slices in the order of HrwTargetList
//		 algorithm returns.

const (
	SliceType = "ec" // object slice prefix
	MetaType  = "mt" // metafile prefix

	ActSplit   = "split"
	ActRestore = "restore"
	ActDelete  = "delete"

	RespStreamName = "ec-resp"
	ReqStreamName  = "ec-req"

	ActClearRequests  = "clear-requests"
	ActEnableRequests = "enable-requests"

	URLCT   = "ct"   // for using in URL path - requests for slices/replicas
	URLMeta = "meta" /// .. - metadata requests

	// EC switches to disk from SGL when memory pressure is high and the amount of
	// memory required to encode an object exceeds the limit
	objSizeHighMem = 50 * cmn.MiB
)

type (
	// request - structure to request an object to be EC'ed or restored
	Request struct {
		LOM      *cluster.LOM // object info
		Action   string       // what to do with the object (see Act* consts)
		ErrCh    chan error   // for final EC result
		Callback cluster.OnFinishObj

		putTime time.Time // time when the object is put into main queue
		tm      time.Time // to measure different steps
		IsCopy  bool      // replicate or use erasure coding
		rebuild bool      // true - internal request to reencode, e.g., from ec-encode xaction
	}

	RequestsControlMsg struct {
		Action string
	}
)

type (
	// keeps temporarily a slice of object data until it is sent to remote node
	slice struct {
		obj     cmn.ReadOpenCloser // the whole object or its replica
		reader  cmn.ReadOpenCloser // used in encoding - a slice of `obj`
		writer  io.Writer          // for parity slices and downloading slices from other targets when restoring
		wg      *cmn.TimeoutGroup  // for synchronous download (for restore)
		lom     *cluster.LOM       // for xattrs
		n       int64              // number of byte sent/received
		refCnt  atomic.Int32       // number of references
		workFQN string             // FQN for temporary slice/replica
		cksum   *cmn.Cksum         // checksum of the slice
		version string             // version of the remote object
	}

	// a source for data response: the data to send to the caller
	// If obj is not nil then after the reader is sent to the remote target,
	// the obj's counter is decreased. And if its value drops to zero the
	// allocated SGL is freed. This logic is required to send a set of
	// sliceReaders that point to the same SGL (broadcasting data slices)
	dataSource struct {
		reader   cmn.ReadOpenCloser // a reader to sent to a remote target
		size     int64              // size of the data
		obj      *slice             // internal info about SGL slice
		metadata *Metadata          // object's metadata
		isSlice  bool               // is it slice or replica
		reqType  intraReqType       // request's type, slice/meta request/response
	}
)

// frees all allocated memory and removes slice's temporary file
func (s *slice) free() {
	freeObject(s.obj)
	s.obj = nil
	if s.reader != nil {
		cmn.Close(s.reader)
	}
	if s.writer != nil {
		switch w := s.writer.(type) {
		case *os.File:
			cmn.Close(w)
		case *memsys.SGL:
			w.Free()
		default:
			cmn.Assertf(false, "%T", w)
		}
	}
	if s.workFQN != "" {
		errRm := os.RemoveAll(s.workFQN)
		debug.AssertNoErr(errRm)
	}
}

// decreases the number of links to the object (the initial number is set
// at slice creation time). If the number drops to zero the allocated
// memory/temporary file is cleaned up
func (s *slice) release() {
	if s.obj != nil || s.workFQN != "" {
		refCnt := s.refCnt.Dec()
		if refCnt < 1 {
			s.free()
		}
	}
}

var (
	mm        *memsys.MMSA // memory manager and slab/SGL allocator
	XactCount atomic.Int32 // the number of currently active EC xactions

	ErrorECDisabled          = errors.New("EC is disabled for bucket")
	ErrorNoMetafile          = errors.New("no metafile")
	ErrorNotFound            = errors.New("not found")
	ErrorInsufficientTargets = errors.New("insufficient targets")
)

func Init(t cluster.Target) {
	mm = t.MMSA()

	fs.CSM.RegisterContentType(SliceType, &SliceSpec{})
	fs.CSM.RegisterContentType(MetaType, &MetaSpec{})

	registry.Registry.RegisterBucketXact(&xactGetProvider{})
	registry.Registry.RegisterBucketXact(&xactPutProvider{})
	registry.Registry.RegisterBucketXact(&xactRespondProvider{})
	registry.Registry.RegisterBucketXact(&xactBckEncodeProvider{})

	if err := initManager(t); err != nil {
		glog.Fatal(err)
	}
}

// SliceSize returns the size of one slice that EC will create for the object
func SliceSize(fileSize int64, slices int) int64 {
	return (fileSize + int64(slices) - 1) / int64(slices)
}

// Monitoring the background transferring of replicas and slices requires
// a unique ID for each of them. Because of all replicas/slices of an object have
// the same names, cluster.Uname is not enough to generate unique ID. Adding an
// extra prefix - an identifier of the destination - solves the issue
func unique(prefix string, bck *cluster.Bck, objName string) string {
	return prefix + string(filepath.Separator) + bck.MakeUname(objName)
}

func IsECCopy(size int64, ecConf *cmn.ECConf) bool {
	return size < ecConf.ObjSizeLimit
}

// returns whether EC must use disk instead of keeping everything in memory.
// Depends on available free memory and size of an object to process
func useDisk(objSize int64) bool {
	switch mm.MemPressure() {
	case memsys.OOM, memsys.MemPressureExtreme:
		return true
	case memsys.MemPressureHigh:
		return objSize > objSizeHighMem
	default:
		return false
	}
}

// Frees allocated memory if it is SGL or closes the file handle in case of regular file
func freeObject(r interface{}) {
	if r == nil {
		return
	}
	if sgl, ok := r.(*memsys.SGL); ok {
		if sgl != nil {
			sgl.Free()
		}
		return
	}
	if f, ok := r.(*cmn.FileHandle); ok {
		if f != nil {
			cmn.Close(f)
		}
		return
	}
	cmn.Assertf(false, "invalid object type: %v", r)
}

// removes all temporary slices in case of erasure coding fails in the middle
func freeSlices(slices []*slice) {
	for _, s := range slices {
		if s != nil {
			s.free()
		}
	}
}

// requestECMeta returns an EC metadata found on a remote target.
func requestECMeta(bck cmn.Bck, objName string, si *cluster.Snode, client *http.Client) (md *Metadata, err error) {
	path := cmn.JoinWords(cmn.Version, cmn.EC, URLMeta, bck.Name, objName)
	query := url.Values{}
	query = cmn.AddBckToQuery(query, bck)
	url := si.URL(cmn.NetworkIntraData) + path
	rq, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	rq.URL.RawQuery = query.Encode()
	resp, err := client.Do(rq) // nolint:bodyclose // closed inside cmn.Close
	if err != nil {
		return nil, err
	}
	defer cmn.Close(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s/%s not found on %s", bck, objName, si.ID())
	} else if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to read %s GET request: %v", objName, err)
	}
	md = &Metadata{}
	err = jsoniter.NewDecoder(resp.Body).Decode(md)
	return md, err
}

// Saves the main replica to local drives
func WriteObject(t cluster.Target, lom *cluster.LOM, reader io.Reader, size int64, cksumType string) error {
	if size > 0 {
		reader = io.LimitReader(reader, size)
	}
	readCloser := ioutil.NopCloser(reader)
	bdir := lom.ParsedFQN.MpathInfo.MakePathBck(lom.Bck().Bck)
	if err := fs.Access(bdir); err != nil {
		return err
	}
	params := cluster.PutObjectParams{
		Reader:       readCloser,
		WorkFQN:      fs.CSM.GenContentFQN(lom.FQN, fs.WorkfileType, "ec"),
		SkipEncode:   true,
		WithFinalize: true,
		RecvType:     cluster.Migrated, // to avoid changing version
	}
	return t.PutObject(lom, params)
}

// Saves slice and its metafile
func WriteSliceAndMeta(t cluster.Target, hdr transport.ObjHdr, data io.Reader, md []byte) error {
	ct, err := cluster.NewCTFromBO(hdr.Bck.Name, hdr.Bck.Provider, hdr.ObjName, t.Bowner(), SliceType)
	if err != nil {
		return err
	}
	tmpFQN := ct.Make(fs.WorkfileType)
	if err := ct.Write(t, data, hdr.ObjAttrs.Size, tmpFQN); err != nil {
		return err
	}
	ctMeta := ct.Clone(MetaType)
	err = ctMeta.Write(t, bytes.NewReader(md), -1)
	if err != nil {
		if rmErr := os.Remove(ct.FQN()); rmErr != nil && !os.IsNotExist(rmErr) {
			glog.Errorf("nested error: save replica -> remove replica: %v", rmErr)
		}
	}
	return err
}

func LomFromHeader(t cluster.Target, hdr transport.ObjHdr) (*cluster.LOM, error) {
	lom := &cluster.LOM{T: t, ObjName: hdr.ObjName}
	if err := lom.Init(hdr.Bck); err != nil {
		return nil, err
	}
	lom.SetSize(hdr.ObjAttrs.Size)
	if hdr.ObjAttrs.Version != "" {
		lom.SetVersion(hdr.ObjAttrs.Version)
	}
	if hdr.ObjAttrs.Atime != 0 {
		lom.SetAtimeUnix(hdr.ObjAttrs.Atime)
	}
	if hdr.ObjAttrs.CksumType != cmn.ChecksumNone && hdr.ObjAttrs.CksumValue != "" {
		lom.SetCksum(cmn.NewCksum(hdr.ObjAttrs.CksumType, hdr.ObjAttrs.CksumValue))
	}
	return lom, nil
}

// Saves replica and its metafile
func WriteReplicaAndMeta(t cluster.Target, lom *cluster.LOM, data io.Reader, md []byte, cksumType, cksumValue string) error {
	err := WriteObject(t, lom, data, lom.Size(), cksumType)
	if err != nil {
		return err
	}
	if cksumType != cmn.ChecksumNone && cksumValue != "" {
		cksumHdr := cmn.NewCksum(cksumType, cksumValue)
		if !lom.Cksum().Equal(cksumHdr) {
			return fmt.Errorf("mismatched hash for %s/%s, version %s, hash calculated %s/md %s",
				lom.Bck().Bck, lom.ObjName, lom.Version(), cksumHdr, lom.Cksum())
		}
	}
	ctMeta := cluster.NewCTFromLOM(lom, MetaType)
	err = ctMeta.Write(t, bytes.NewReader(md), -1)
	if err != nil {
		if rmErr := os.Remove(lom.FQN); rmErr != nil && !os.IsNotExist(rmErr) {
			glog.Errorf("nested error: save replica -> remove replica: %v", rmErr)
		}
	}
	return err
}
