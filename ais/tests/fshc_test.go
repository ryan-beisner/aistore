// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package integration

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/devtools/readers"
	"github.com/NVIDIA/aistore/devtools/tassert"
	"github.com/NVIDIA/aistore/devtools/tlog"
	"github.com/NVIDIA/aistore/devtools/tutils"
)

const (
	fshcDetectTimeMax = time.Second * 10
	fshcRunTimeMax    = time.Second * 15
	fshcDir           = "fschecker"
)

type checkerMD struct {
	t          *testing.T
	seed       int64
	numObjs    int
	proxyURL   string
	bck        cmn.Bck
	smap       *cluster.Smap
	mpList     cluster.NodeMap
	allMps     map[*cluster.Snode]*cmn.MountpathList
	origAvail  int
	fileSize   int64
	baseParams api.BaseParams
	chstop     chan struct{}
	chfail     chan struct{}
	wg         *sync.WaitGroup
}

func newCheckerMD(t *testing.T) *checkerMD {
	md := &checkerMD{
		t:        t,
		seed:     300,
		numObjs:  100,
		proxyURL: tutils.RandomProxyURL(),
		bck: cmn.Bck{
			Name:     testBucketName,
			Provider: cmn.ProviderAIS,
		},
		fileSize: 64 * cos.KiB,
		mpList:   make(cluster.NodeMap, 10),
		allMps:   make(map[*cluster.Snode]*cmn.MountpathList, 10),
		chstop:   make(chan struct{}),
		chfail:   make(chan struct{}),
		wg:       &sync.WaitGroup{},
	}

	md.init()

	return md
}

func (md *checkerMD) init() {
	md.baseParams = tutils.BaseAPIParams(md.proxyURL)
	md.smap = tutils.GetClusterMap(md.t, md.proxyURL)

	for targetID, tinfo := range md.smap.Tmap {
		tlog.Logf("Target: %s\n", targetID)
		lst, err := api.GetMountpaths(md.baseParams, tinfo)
		tassert.CheckFatal(md.t, err)
		tlog.Logf("    Mountpaths: %v\n", lst)

		for _, fqn := range lst.Available {
			md.mpList[fqn] = tinfo
		}
		md.allMps[tinfo] = lst

		md.origAvail += len(lst.Available)
	}
}

func (md *checkerMD) randomTargetMpath() (target *cluster.Snode, mpath string, mpathMap *cmn.MountpathList) {
	// select random target and mountpath
	for m, t := range md.mpList {
		target, mpath = t, m
		mpathMap = md.allMps[target]
		break
	}
	return
}

func (md *checkerMD) runTestAsync(method string, target *cluster.Snode, mpath string, mpathList *cmn.MountpathList, suffix string) {
	md.wg.Add(1)
	go runAsyncJob(md.t, md.bck, md.wg, method, mpath, fileNames, md.chfail, md.chstop, suffix)
	// let the job run for a while and then make a mountpath broken
	time.Sleep(2 * time.Second)
	md.chfail <- struct{}{}
	if detected := waitForMountpathChanges(md.t, target, len(mpathList.Available)-1, len(mpathList.Disabled)+1, true); detected {
		// let the job run for a while with broken mountpath, so FSHC detects the trouble
		time.Sleep(2 * time.Second)
		md.chstop <- struct{}{}
	}
	md.wg.Wait()

	repairMountpath(md.t, target, mpath, len(mpathList.Available), len(mpathList.Disabled), suffix)
}

func (md *checkerMD) runTestSync(method string, target *cluster.Snode, mpath string, mpathList *cmn.MountpathList,
	objList []string, suffix string) {
	breakMountpath(md.t, mpath, suffix)
	defer repairMountpath(md.t, target, mpath, len(mpathList.Available), len(mpathList.Disabled), suffix)

	switch method {
	case http.MethodPut:
		p, err := api.HeadBucket(md.baseParams, md.bck)
		tassert.CheckFatal(md.t, err)
		for _, objName := range objList {
			r, _ := readers.NewRandReader(md.fileSize, p.Cksum.Type)
			err := api.PutObject(api.PutObjectArgs{
				BaseParams: md.baseParams,
				Bck:        md.bck,
				Object:     path.Join(fshcDir, objName),
				Reader:     r,
				Size:       uint64(md.fileSize),
			})
			if err != nil {
				tlog.Logf("%s: %v\n", objName, err)
			}
		}
	case http.MethodGet:
		for _, objName := range objList {
			// GetObject must fail - so no error checking
			_, err := api.GetObject(md.baseParams, md.bck, objName)
			if err == nil {
				md.t.Errorf("Get %q must fail", objName)
			}
		}
	}

	if detected := waitForMountpathChanges(md.t, target, len(mpathList.Available)-1, len(mpathList.Disabled)+1, false); detected {
		md.t.Error("PUT objects to a broken mountpath should not disable the mountpath when FSHC is disabled")
	}
}

func waitForMountpathChanges(t *testing.T, target *cluster.Snode, availLen, disabledLen int, failIfDiffer bool) bool {
	var (
		err        error
		newMpaths  *cmn.MountpathList
		baseParams = tutils.BaseAPIParams()
	)

	detectStart := time.Now()
	detectLimit := time.Now().Add(fshcDetectTimeMax)

	for detectLimit.After(time.Now()) {
		newMpaths, err = api.GetMountpaths(baseParams, target)
		if err != nil {
			t.Errorf("Failed to read target mountpaths: %v\n", err)
			break
		}
		if len(newMpaths.Disabled) == disabledLen {
			break
		}
		time.Sleep(time.Millisecond * 100)
	}
	detectTime := time.Since(detectStart)
	tlog.Logf("passed %v\n", detectTime)

	if len(newMpaths.Disabled) == disabledLen && len(newMpaths.Available) == availLen {
		tlog.Logf("Check is successful in %v\n", detectTime)
		return true
	}

	if !failIfDiffer {
		return false
	}

	tlog.Logf("Current mpath list: %v\n", newMpaths)
	if len(newMpaths.Disabled) != disabledLen {
		t.Errorf("Disabled mpath count mismatch, old count: %v, new list: %v",
			disabledLen, newMpaths.Disabled)
	} else if len(newMpaths.Available) != availLen {
		t.Errorf("Available mpath count mismatch, old count: %v, new list: %v",
			availLen, newMpaths.Available)
	}
	return false
}

// Simulating mountpath death requested.
// It is the easiest way to simulate: stop putting data and
// replace the mountpath with regular file. If we do not stop
// putting objects it recreates the mountpath and does not fail
func breakMountpath(t *testing.T, mpath, suffix string) {
	os.Rename(mpath, mpath+suffix)
	f, err := os.OpenFile(mpath, os.O_CREATE|os.O_WRONLY, cos.PermRWR)
	if err != nil {
		t.Errorf("Failed to create file: %v", err)
	}
	f.Close()
}

func repairMountpath(t *testing.T, target *cluster.Snode, mpath string, availLen, disabledLen int, suffix string) {
	var (
		err        error
		baseParams = tutils.BaseAPIParams()
	)

	// "broken" mpath does no exist, nothing to restore
	if _, err := os.Stat(mpath + suffix); err != nil {
		return
	}
	// cleanup
	// restore original mountpath
	os.Remove(mpath)
	cos.Rename(mpath+suffix, mpath)

	// ask fschecker to check all mountpath - it should make disabled
	// mountpath back to available list
	api.EnableMountpath(baseParams, target, mpath)
	tlog.Logln("Recheck mountpaths")
	detectStart := time.Now()
	detectLimit := time.Now().Add(fshcDetectTimeMax)
	var mpaths *cmn.MountpathList
	// Wait for fsckeeper detects that the mountpath is accessible now
	for detectLimit.After(time.Now()) {
		mpaths, err = api.GetMountpaths(baseParams, target)
		if err != nil {
			t.Errorf("Failed to read target mountpaths: %v\n", err)
			break
		}
		if len(mpaths.Disabled) == disabledLen && len(mpaths.Available) == availLen {
			break
		}
		time.Sleep(time.Second)
	}

	// final test checks - available and disabled lists must equal list
	// before starting the test
	if len(mpaths.Disabled) != disabledLen {
		t.Errorf("Failed mountpath is still disabled in %v\nExpected disabled count: %d\nNew list:%v\n",
			time.Since(detectStart), disabledLen, mpaths.Disabled)
	} else if len(mpaths.Available) != availLen {
		t.Errorf("Failed mountpath is not back in %v.\nExpected available count: %d\nNew list:%v\n",
			time.Since(detectStart), availLen, mpaths.Available)
	}
}

func runAsyncJob(t *testing.T, bck cmn.Bck, wg *sync.WaitGroup, op, mpath string, filelist []string, chfail,
	chstop chan struct{}, suffix string) {
	defer wg.Done()

	const fileSize = 64 * cos.KiB
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
	)

	tlog.Logf("Testing mpath fail detection on %s\n", op)
	stopTime := time.Now().Add(fshcRunTimeMax)

	p, err := api.HeadBucket(baseParams, bck)
	tassert.CheckFatal(t, err)

	for stopTime.After(time.Now()) {
		errCh := make(chan error, len(filelist))
		objsPutCh := make(chan string, len(filelist))

		for _, fname := range filelist {
			select {
			case <-chfail:
				breakMountpath(t, mpath, suffix)
			case <-chstop:
				return
			default:
				// do nothing and just start the next loop
			}

			switch op {
			case "PUT":
				r, _ := readers.NewRandReader(fileSize, p.Cksum.Type)
				api.PutObject(api.PutObjectArgs{
					BaseParams: baseParams,
					Bck:        bck,
					Object:     path.Join(fshcDir, fname),
					Reader:     r,
					Size:       fileSize,
				})
			case "GET":
				api.GetObject(baseParams, bck, path.Join(fshcDir, fname))
				time.Sleep(time.Millisecond * 10)
			default:
				t.Errorf("Invalid operation: %s", op)
			}
		}

		close(errCh)
		close(objsPutCh)
	}
}

func TestFSCheckerDetectionEnabled(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		md     = newCheckerMD(t)
		suffix = "-" + cos.RandString(5)
	)

	if md.origAvail == 0 {
		t.Fatal("No available mountpaths found")
	}

	tutils.CreateFreshBucket(t, md.proxyURL, md.bck, nil)
	selectedTarget, selectedMpath, selectedMpathList := md.randomTargetMpath()
	tlog.Logf("mountpath %s of %s is selected for the test\n", selectedMpath, selectedTarget)
	defer func() {
		if err := api.RemoveMountpath(md.baseParams, selectedTarget.ID(), selectedMpath); err != nil {
			t.Logf("Failed to remove mpath %s of %s: %v", selectedMpath, selectedTarget, err)
		}
		if err := api.AddMountpath(md.baseParams, selectedTarget, selectedMpath); err != nil {
			t.Logf("Failed to add mpath %s of %s: %v", selectedMpath, selectedTarget, err)
		}

		tutils.WaitForRebalanceToComplete(t, md.baseParams, rebalanceTimeout)
	}()

	// generate some filenames to PUT to them in a loop
	generateRandomNames(md.numObjs)

	// Checking detection on object PUT
	md.runTestAsync(http.MethodPut, selectedTarget, selectedMpath, selectedMpathList, suffix)
	// Checking detection on object GET
	md.runTestAsync(http.MethodGet, selectedTarget, selectedMpath, selectedMpathList, suffix)

	// Checking that reading "bad" objects does not disable mpath if the mpath is OK
	tlog.Logf("Reading non-existing objects: read is expected to fail but mountpath must be available\n")
	for n := 1; n < 10; n++ {
		objName := fmt.Sprintf("%s/o%d", fshcDir, n)
		if _, err := api.GetObject(md.baseParams, md.bck, objName); err == nil {
			t.Error("Should not be able to GET non-existing objects")
		}
	}
	if detected := waitForMountpathChanges(t, selectedTarget, len(selectedMpathList.Available), len(selectedMpathList.Disabled), false); !detected {
		t.Error("GETting non-existing objects should not disable mountpath")
		repairMountpath(t, selectedTarget, selectedMpath, len(selectedMpathList.Available), len(selectedMpathList.Disabled), suffix)
	}
}

func TestFSCheckerDetectionDisabled(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		md     = newCheckerMD(t)
		suffix = "-" + cos.RandString(5)
	)

	if md.origAvail == 0 {
		t.Fatal("No available mountpaths found")
	}

	tlog.Logf("*** Testing with disabled FSHC***\n")
	tutils.SetClusterConfig(t, cos.SimpleKVs{"fshc.enabled": "false"})
	defer tutils.SetClusterConfig(t, cos.SimpleKVs{"fshc.enabled": "true"})

	selectedTarget, selectedMpath, selectedMap := md.randomTargetMpath()
	tlog.Logf("mountpath %s of %s is selected for the test\n", selectedMpath, selectedTarget)
	tutils.CreateFreshBucket(t, md.proxyURL, md.bck, nil)
	defer func() {
		if err := api.RemoveMountpath(md.baseParams, selectedTarget.ID(), selectedMpath); err != nil {
			t.Logf("Failed to remove mpath %s of %s: %v", selectedMpath, selectedTarget, err)
		}
		if err := api.AddMountpath(md.baseParams, selectedTarget, selectedMpath); err != nil {
			t.Logf("Failed to add mpath %s of %s: %v", selectedMpath, selectedTarget, err)
		}

		tutils.WaitForRebalanceToComplete(t, md.baseParams, rebalanceTimeout)
	}()

	// generate a short list of file to run the test (to avoid flooding the log with false errors)
	objList := make([]string, 0, 5)
	for n := 0; n < 5; n++ {
		objName := fmt.Sprintf("obj-fshc-%d", n)
		objList = append(objList, objName)
	}

	// Checking detection on object PUT
	md.runTestSync(http.MethodPut, selectedTarget, selectedMpath, selectedMap, objList, suffix)
	// Checking detection on object GET
	md.runTestSync(http.MethodGet, selectedTarget, selectedMpath, selectedMap, objList, suffix)
}

func TestFSCheckerEnablingMpath(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		smap       = tutils.GetClusterMap(t, proxyURL)
		mpList     = make(cluster.NodeMap, 10)
		origAvail  = 0
	)

	for targetID, tinfo := range smap.Tmap {
		tlog.Logf("Target: %s\n", targetID)
		lst, err := api.GetMountpaths(baseParams, tinfo)
		tassert.CheckFatal(t, err)
		tlog.Logf("    Mountpaths: %v\n", lst)

		for _, fqn := range lst.Available {
			mpList[fqn] = tinfo
		}

		origAvail += len(lst.Available)
	}

	if origAvail == 0 {
		t.Fatal("No available mountpaths found")
	}

	// select random target and mountpath
	var (
		selectedTarget *cluster.Snode
		selectedMpath  string
	)
	for m, t := range mpList {
		selectedTarget, selectedMpath = t, m
		break
	}

	err := api.EnableMountpath(baseParams, selectedTarget, selectedMpath)
	if err != nil {
		t.Errorf("Enabling available mountpath should return success, got: %v", err)
	}

	err = api.EnableMountpath(baseParams, selectedTarget, selectedMpath+"some_text")
	if err == nil {
		t.Errorf("Enabling non-existing mountpath should return error")
	} else {
		status := api.HTTPStatus(err)
		if status != http.StatusNotFound {
			t.Errorf("Expected status %d, got %d, %v", http.StatusNotFound, status, err)
		}
	}
	tutils.WaitForRebalanceToComplete(t, baseParams)
}

func TestFSCheckerTargetDisable(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})
	var (
		target *cluster.Snode

		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams()
		smap       = tutils.GetClusterMap(t, proxyURL)
		proxyCnt   = smap.CountActiveProxies()
		targetCnt  = smap.CountActiveTargets()
	)

	if targetCnt < 2 {
		t.Skip("The number of targets must be at least 2")
	}

	target, _ = smap.GetRandTarget()
	oldMpaths, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)
	if len(oldMpaths.Available) == 0 {
		t.Fatalf("Target %s does not have available mountpaths", target)
	}

	tlog.Logf("Removing all mountpaths from target: %s\n", target)
	for _, mpath := range oldMpaths.Available {
		err = api.DisableMountpath(baseParams, target.ID(), mpath)
		tassert.CheckFatal(t, err)
	}

	smap, err = tutils.WaitForClusterState(proxyURL, "all mpath disabled", smap.Version, proxyCnt, targetCnt-1)
	tassert.CheckFatal(t, err)

	tlog.Logf("Restoring target %s mountpaths\n", target.ID())
	for _, mpath := range oldMpaths.Available {
		err = api.EnableMountpath(baseParams, target, mpath)
		tassert.CheckFatal(t, err)
	}

	_, err = tutils.WaitForClusterState(proxyURL, "all mpath enabled", smap.Version, proxyCnt, targetCnt)
	tassert.CheckFatal(t, err)
	tutils.WaitForRebalanceToComplete(t, baseParams)
}

func TestFSAddMPathRestartNode(t *testing.T) {
	t.Skip("enable after targets load mount paths from VMD on startup")
	var (
		target *cluster.Snode

		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams()
		smap       = tutils.GetClusterMap(t, proxyURL)
		proxyCnt   = smap.CountProxies()
		targetCnt  = smap.CountActiveTargets()
		tmpMpath   = "/tmp/testmp"
	)

	if targetCnt < 2 {
		t.Skip("The number of targets must be at least 2")
	}

	target, _ = smap.GetRandTarget()
	oldMpaths, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)
	numMpaths := len(oldMpaths.Available)
	tassert.Fatalf(t, numMpaths != 0, "target %s doesn't have available mountpaths", target)

	cos.CreateDir(tmpMpath)
	defer os.Remove(tmpMpath)

	tlog.Logf("Adding a mount path to target: %s\n", target)
	err = api.AddMountpath(baseParams, target, tmpMpath)
	tassert.CheckFatal(t, err)
	defer api.RemoveMountpath(baseParams, target.ID(), tmpMpath)

	newMpaths, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)

	tassert.Fatalf(t, numMpaths+1 == len(newMpaths.Available),
		"should add new mount path - available %d!=%d", numMpaths+1, len(newMpaths.Available))

	// Kill and restore target
	tcmd, err := tutils.KillNode(target)
	tassert.CheckFatal(t, err)
	tutils.RestoreNode(tcmd, false, "target")

	smap, err = tutils.WaitForClusterState(smap.Primary.URL(cmn.NetworkPublic), "to restore", smap.Version,
		proxyCnt, targetCnt)
	tassert.CheckFatal(t, err)
	if _, ok := smap.Tmap[target.ID()]; !ok {
		t.Fatalf("Removed target didn't rejoin")
	}

	// Check if the node has newly added mount path
	newMpaths, err = api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, numMpaths+1 == len(newMpaths.Available),
		"should include newly added mount path after restore - available %d!=%d", numMpaths+1, len(newMpaths.Available))
}

func TestFSDisableMpathRestart(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true, RequiredDeployment: tutils.ClusterTypeLocal})
	var (
		target *cluster.Snode

		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams()
		smap       = tutils.GetClusterMap(t, proxyURL)
		proxyCnt   = smap.CountProxies()
		targetCnt  = smap.CountActiveTargets()
		enabled    bool
	)

	if targetCnt < 2 {
		t.Skip("The number of targets must be at least 2")
	}

	for _, tinfo := range smap.Tmap {
		target = tinfo
		break
	}
	oldMpaths, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)
	numMpaths := len(oldMpaths.Available)
	tassert.Fatalf(t, numMpaths != 0, "target %s doesn't have available mountpaths", target)

	// 1. Disable mountpaths temporarily
	mpath := oldMpaths.Available[0]
	tlog.Logf("Disable mountpath on target %s\n", target.ID())
	err = api.DisableMountpath(baseParams, target.ID(), mpath)
	tassert.CheckFatal(t, err)
	defer func() {
		if !enabled {
			api.EnableMountpath(baseParams, target, mpath)
			tutils.WaitForRebalanceToComplete(t, baseParams)
		}
	}()

	// Kill and restore target
	tlog.Logf("Killing target %s\n", target)
	tcmd, err := tutils.KillNode(target)
	tassert.CheckFatal(t, err)
	smap, err = tutils.WaitForClusterState(proxyURL, "remove target", smap.Version, proxyCnt, targetCnt-1)
	tassert.CheckFatal(t, err)

	tutils.RestoreNode(tcmd, false, "target")
	smap, err = tutils.WaitForClusterState(smap.Primary.URL(cmn.NetworkPublic), "to restore", smap.Version,
		proxyCnt, targetCnt)
	tassert.CheckFatal(t, err)
	if _, ok := smap.Tmap[target.ID()]; !ok {
		t.Fatalf("Removed target didn't rejoin")
	}

	// Check if the the mountpath is disabled after restart
	newMpaths, err := api.GetMountpaths(baseParams, target)
	tassert.CheckError(t, err)
	tassert.Errorf(t, numMpaths-1 == len(newMpaths.Available),
		"should not have disabled path after restore - available %d!=%d", numMpaths-1, len(newMpaths.Available))
	tassert.Errorf(t, len(newMpaths.Disabled) == 1 && newMpaths.Disabled[0] == mpath, "should have disabled mountpath: %s not in %v", mpath, newMpaths.Disabled)

	// Re-enable the mountpath should work
	err = api.EnableMountpath(baseParams, target, mpath)
	tassert.CheckFatal(t, err)
	enabled = true
	tutils.WaitForRebalanceToComplete(t, baseParams)

	newMpaths, err = api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)
	tassert.Errorf(t, numMpaths == len(newMpaths.Available),
		"should restore all mountpaths - available %d!=%d", numMpaths, len(newMpaths.Available))
	tassert.Errorf(t, len(newMpaths.Disabled) == 0, "should not have disabled mountpaths: has %v", newMpaths.Disabled)
}
