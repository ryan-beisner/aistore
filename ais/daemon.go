// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/health"
	"github.com/NVIDIA/aistore/hk"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/sys"
	"github.com/NVIDIA/aistore/transport"
)

const usecli = `
   Usage:
        aisnode -role=<proxy|target> -config=</dir/config.json> -local_config=</dir/local-config.json> ...`

type (
	daemonCtx struct {
		cli       cliFlags
		dryRun    dryRunConfig
		rg        *rungroup
		version   string      // major.minor.build (see cmd/aisnode)
		buildTime string      // YYYY-MM-DD HH:MM:SS-TZ
		stopping  atomic.Bool // true when exiting
	}
	cliFlags struct {
		localConfigPath  string // path to local config
		globalConfigPath string // path to global config
		role             string // proxy | target
		daemonID         string // daemon ID to assign
		confCustom       string // "key1=value1,key2=value2" formatted to override selected entries in config
		ntargets         int    // expected number of targets in a starting-up cluster (proxy only)
		skipStartup      bool   // determines if the proxy should skip waiting for targets
		transient        bool   // false: make cmn.ConfigCLI settings permanent, true: leave them transient
		usage            bool   // show usage and exit
	}
	rungroup struct {
		rs    map[string]cos.Runner
		errCh chan error
	}
	// - selective disabling of a disk and/or network IO.
	// - dry-run is initialized at startup and cannot be changed.
	// - the values can be set via clivars or environment (environment will override clivars).
	// - for details see README, section "Performance testing"
	dryRunConfig struct {
		sizeStr string // random content size used when disk IO is disabled (-dryobjsize/AIS_DRY_OBJ_SIZE)
		size    int64  // as above converted to bytes from a string like '8m'
		disk    bool   // dry-run disk (-nodiskio/AIS_NO_DISK_IO)
	}
)

var daemon = daemonCtx{}

func init() {
	// role aka `DaemonType`
	flag.StringVar(&daemon.cli.role, "role", "", "role of this AIS daemon: proxy | target")
	flag.StringVar(&daemon.cli.daemonID, "daemon_id", "", "unique ID to be assigned to the AIS daemon")

	// config itself and its command line overrides
	flag.StringVar(&daemon.cli.globalConfigPath, "config", "",
		"config filename: local file that stores the global cluster configuration")
	flag.StringVar(&daemon.cli.localConfigPath, "local_config", "", "config filename: local file that stores daemon's local configuration")

	flag.StringVar(&daemon.cli.confCustom, "config_custom", "",
		"\"key1=value1,key2=value2\" formatted string to override selected entries in config")

	flag.IntVar(&daemon.cli.ntargets, "ntargets", 0, "number of storage targets to expect at startup (hint, proxy-only)")

	flag.BoolVar(&daemon.cli.transient, "transient", false,
		"false: apply command-line args to the configuration and save the latter to disk\ntrue: keep it transient (for this run only)")
	flag.BoolVar(&daemon.cli.skipStartup, "skip_startup", false,
		"determines if primary proxy should skip waiting for target registrations when starting up")
	flag.BoolVar(&daemon.cli.usage, "h", false, "show usage and exit")

	// dry-run
	flag.BoolVar(&daemon.dryRun.disk, "nodiskio", false, "dry-run: if true, no disk operations for GET and PUT")
	flag.StringVar(&daemon.dryRun.sizeStr, "dryobjsize", "8m", "dry-run: in-memory random content")
}

// dry-run environment overrides dry-run clivars
func dryRunInit() {
	str := os.Getenv("AIS_NO_DISK_IO")
	if b, err := cos.ParseBool(str); err == nil {
		daemon.dryRun.disk = b
	}
	str = os.Getenv("AIS_DRY_OBJ_SIZE")
	if str != "" {
		if size, err := cos.S2B(str); size > 0 && err == nil {
			daemon.dryRun.size = size
		}
	}
	if daemon.dryRun.disk {
		warning := "Dry-run: disk IO will be disabled"
		fmt.Fprintf(os.Stderr, "%s\n", warning)
		glog.Infof("%s - in memory file size: %d (%s) bytes", warning, daemon.dryRun.size, daemon.dryRun.sizeStr)
	}
}

func initDaemon(version, buildTime string) (rmain cos.Runner) {
	const erfm = "Missing `%s` flag pointing to configuration file (must be provided via command line)\n"
	var (
		config *cmn.Config
		err    error
	)
	flag.Parse()
	if daemon.cli.usage || len(os.Args[1:]) == 0 {
		flag.Usage()
		cos.Exitf(usecli)
	}
	if daemon.cli.role != cmn.Proxy && daemon.cli.role != cmn.Target {
		cos.ExitLogf("Invalid daemon's role %q, expecting %q or %q", daemon.cli.role, cmn.Proxy, cmn.Target)
	}
	if daemon.dryRun.disk {
		daemon.dryRun.size, err = cos.S2B(daemon.dryRun.sizeStr)
		if daemon.dryRun.size < 1 || err != nil {
			cos.ExitLogf("Invalid dry-run size: %d [%s]\n", daemon.dryRun.size, daemon.dryRun.sizeStr)
		}
	}
	if daemon.cli.globalConfigPath == "" {
		cos.ExitLogf(erfm, "config")
	}
	if daemon.cli.localConfigPath == "" {
		cos.ExitLogf(erfm, "local-config")
	}
	config = &cmn.Config{}
	err = cmn.LoadConfig(daemon.cli.globalConfigPath, daemon.cli.localConfigPath, daemon.cli.role, config)
	if err != nil {
		cos.ExitLogf("%v", err)
	}
	cmn.GCO.Put(config)

	// Examples overriding default configuration at a node startup via command line:
	// 1) set client timeout to 13s and store the updated value on disk:
	// $ aisnode -config=/etc/ais.json -local_config=/etc/ais_local.json -role=target \
	//   -config_custom="client.client_timeout=13s"
	//
	// 2) same as above except that the new timeout will remain transient
	//    (won't persist across restarts):
	// $ aisnode -config=/etc/ais.json -local_config=/etc/ais_local.json -role=target -transient=true \
	//   -config_custom="client.client_timeout=13s"
	if daemon.cli.confCustom != "" {
		var (
			toUpdate = &cmn.ConfigToUpdate{}
			kvs      = strings.Split(daemon.cli.confCustom, ",")
		)
		if err := toUpdate.FillFromKVS(kvs); err != nil {
			cos.ExitLogf(err.Error())
		}
		if err := cmn.GCO.SetConfigInMem(toUpdate, config, cmn.Daemon); err != nil {
			cos.ExitLogf("Failed to update config in memory: %v", err)
		}

		overrideConfig := cmn.GCO.GetOverrideConfig()
		if overrideConfig == nil {
			overrideConfig = toUpdate
		} else {
			overrideConfig.Merge(toUpdate)
		}

		if !daemon.cli.transient {
			if err = cmn.SaveOverrideConfig(config.ConfigDir, overrideConfig); err != nil {
				cos.ExitLogf("Failed to save override config: %v", err)
			}
		}
	}

	daemon.version, daemon.buildTime = version, buildTime
	glog.Infof("| version: %s | build-time: %s |\n", version, buildTime)
	debug.Errorln("starting with debug asserts/logs")

	containerized := sys.Containerized()
	cpus := sys.NumCPU()
	glog.Infof("num CPUs(%d, %d), container %t", cpus, runtime.NumCPU(), containerized)
	sys.SetMaxProcs()

	memStat, err := sys.Mem()
	debug.AssertNoErr(err)
	glog.Infof("Memory total: %s, free: %s(actual free %s)",
		cos.B2S(int64(memStat.Total), 0), cos.B2S(int64(memStat.Free), 0), cos.B2S(int64(memStat.ActualFree), 0))

	// NOTE: Daemon terminations get executed in the same exact order as initializations below.
	daemon.rg = &rungroup{rs: make(map[string]cos.Runner, 8)}

	daemon.rg.add(hk.DefaultHK)
	if daemon.cli.role == cmn.Proxy {
		rmain = initProxy()
	} else {
		rmain = initTarget()
	}
	return
}

func initProxy() cos.Runner {
	p := &proxyrunner{}
	p.initConfOwner(cmn.Proxy)
	p.initSI(cmn.Proxy)

	// Persist daemon ID on disk
	if err := writeProxyDID(cmn.GCO.Get(), p.si.ID()); err != nil {
		cos.ExitLogf("%v", err)
	}

	p.initClusterCIDR()
	daemon.rg.add(p)

	ps := &stats.Prunner{}
	startedUp := ps.Init(p)
	daemon.rg.add(ps)
	p.statsT = ps

	k := newProxyKeepaliveRunner(p, ps, startedUp)
	daemon.rg.add(k)
	p.keepalive = k

	m := newMetasyncer(p)
	daemon.rg.add(m)
	p.metasyncer = m
	return p
}

func newTarget() *targetrunner {
	t := &targetrunner{backend: make(backends, 8)}
	t.gfn.local.tag, t.gfn.global.tag = "local GFN", "global GFN"
	t.owner.bmd = newBMDOwnerTgt()
	return t
}

func initTarget() cos.Runner {
	// Initialize filesystem/mountpaths manager.
	fs.Init()

	t := newTarget()

	t.initConfOwner(cmn.Target)
	// fs.Mountpaths must be inited prior to all runners that utilize them
	// for mountpath definition, see fs/mountfs.go
	config := cmn.GCO.Get()
	if config.TestingEnv() {
		glog.Infof("Warning: configuring %d fspaths for testing", config.TestFSP.Count)
		fs.DisableFsIDCheck()
		t.testCachepathMounts()
	}
	t.initSI(cmn.Target)
	if err := fs.InitMpaths(t.si.ID()); err != nil {
		cos.ExitLogf("%v", err)
	}

	t.initHostIP()
	daemon.rg.add(t)

	ts := &stats.Trunner{T: t} // iostat below
	startedUp := ts.Init(t)
	daemon.rg.add(ts)
	t.statsT = ts

	k := newTargetKeepaliveRunner(t, ts, startedUp)
	daemon.rg.add(k)
	t.keepalive = k

	t.fsprg.init(t) // subgroup of the daemon.rg rungroup

	// Stream Collector - a singleton object with responsibilities that include:
	sc := transport.Init()
	daemon.rg.add(sc)

	fshc := health.NewFSHC(t)
	daemon.rg.add(fshc)
	t.fshc = fshc

	if err := ts.InitCapacity(); err != nil { // goes after fs.Init
		cos.ExitLogf("%s", err)
	}
	return t
}

// Run is the 'main' where everything gets started
func Run(version, buildTime string) int {
	defer glog.Flush() // always flush

	rmain := initDaemon(version, buildTime)
	err := daemon.rg.run(rmain)

	if err == nil {
		glog.Infoln("Terminated OK")
		return 0
	}
	if e, ok := err.(*cos.ErrSignal); ok {
		glog.Infof("Terminated OK (via signal: %v)\n", e)
		return e.ExitCode()
	}
	if errors.Is(err, cmn.ErrStartupTimeout) {
		// NOTE: stats and keepalive runners wait for the ClusterStarted() - i.e., for the primary
		//       to reach the corresponding stage. There must be an external "restarter" (e.g. K8s)
		//       to restart the daemon if the primary gets killed or panics prior (to reaching that state)
		glog.Errorln("Timed-out while starting up")
	}
	glog.Errorf("Terminated with err: %s", err)
	return 1
}

//////////////
// rungroup //
//////////////

func (g *rungroup) add(r cos.Runner) {
	cos.Assert(r.Name() != "")
	_, exists := g.rs[r.Name()]
	cos.Assert(!exists)

	g.rs[r.Name()] = r
}

func (g *rungroup) run(mainRunner cos.Runner) error {
	var mainDone atomic.Bool
	g.errCh = make(chan error, len(g.rs))
	daemon.stopping.Store(false)
	for _, r := range g.rs {
		go func(r cos.Runner) {
			err := r.Run()
			if err != nil {
				glog.Warningf("runner [%s] exited with err [%v]", r.Name(), err)
			}
			if r.Name() == mainRunner.Name() {
				mainDone.Store(true) // load it only once
			}
			g.errCh <- err
		}(r)
	}

	// Stop all runners, target (or proxy) first.
	err := <-g.errCh
	daemon.stopping.Store(true)
	if !mainDone.Load() {
		mainRunner.Stop(err)
	}
	for _, r := range g.rs {
		if r.Name() != mainRunner.Name() {
			r.Stop(err)
		}
	}
	// Wait for all terminations.
	for i := 0; i < len(g.rs)-1; i++ {
		<-g.errCh
	}
	return err
}

///////////////
// daemon ID //
///////////////

const (
	daemonIDEnv  = "AIS_DAEMON_ID"
	proxyIDFname = ".ais.proxy_id"
)

func initDaemonID(daemonType string, config *cmn.Config) (daemonID string) {
	if daemon.cli.daemonID != "" {
		return daemon.cli.daemonID
	}
	if daemonID = os.Getenv(daemonIDEnv); daemonID != "" {
		glog.Infof("%s[%s] ID from env", daemonType, daemonID)
		return
	}
	switch daemonType {
	case cmn.Target:
		daemonID = initTargetDaemonID(config)
	case cmn.Proxy:
		daemonID = initProxyDaemonID(config)
	default:
		cos.AssertMsg(false, daemonType)
	}
	return daemonID
}

func generateDaemonID(daemonType string, config *cmn.Config) string {
	if !config.TestingEnv() {
		return cos.GenDaemonID()
	}
	daemonID := cos.RandStringStrong(4)
	switch daemonType {
	case cmn.Target:
		return fmt.Sprintf("%st%d", daemonID, config.HostNet.Port)
	case cmn.Proxy:
		return fmt.Sprintf("%sp%d", daemonID, config.HostNet.Port)
	}
	cos.AssertMsg(false, daemonType)
	return ""
}

// proxy ID
func initProxyDaemonID(config *cmn.Config) (daemonID string) {
	if daemonID = readProxyDaemonID(config); daemonID != "" {
		glog.Infof("p[%s] from %q", daemonID, proxyIDFname)
		return
	}
	daemonID = generateDaemonID(cmn.Proxy, config)
	cos.Assert(daemonID != "")
	glog.Infof("p[%s] ID randomly generated", daemonID)
	return daemonID
}

func writeProxyDID(config *cmn.Config, id string) error {
	return ioutil.WriteFile(filepath.Join(config.ConfigDir, proxyIDFname), []byte(id), cos.PermRWR)
}

func readProxyDaemonID(config *cmn.Config) (id string) {
	if b, err := ioutil.ReadFile(filepath.Join(config.ConfigDir, proxyIDFname)); err == nil {
		id = string(b)
	} else if !os.IsNotExist(err) {
		glog.Error(err)
	}
	return
}

// target ID
func initTargetDaemonID(config *cmn.Config) (daemonID string) {
	var err error
	if daemonID, err = fs.LoadDaemonID(config.FSpaths.Paths); err != nil {
		cos.ExitLogf("%v", err)
	}
	if daemonID != "" {
		return
	}
	daemonID = generateDaemonID(cmn.Target, config)
	cos.Assert(daemonID != "")
	glog.Infof("t[%s] ID randomly generated", daemonID)
	return daemonID
}
