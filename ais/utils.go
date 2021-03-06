// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/k8s"
	"github.com/NVIDIA/aistore/xaction/xreg"
)

const (
	accessNetPublic = netAccess(1 << iota)
	accessNetIntraControl
	accessNetIntraData

	accessNetPublicControl = accessNetPublic | accessNetIntraControl
	accessNetPublicData    = accessNetPublic | accessNetIntraData
	accessControlData      = accessNetIntraControl | accessNetIntraData
	accessNetAll           = accessNetPublic | accessNetIntraData | accessNetIntraControl
)
const gconfFname = ".ais.conf"

// Network access of handlers (Public, IntraControl, & IntraData)
type netAccess int

func (na netAccess) isSet(flag netAccess) bool {
	return na&flag == flag
}

//
// request validation helpers - TODO: optionally, check node IDs vs Smap
//
func isIntraCall(hdr http.Header) bool {
	return hdr != nil && hdr.Get(cmn.HeaderCallerID) != "" && hdr.Get(cmn.HeaderCallerName) != ""
}
func isIntraPut(hdr http.Header) bool { return hdr != nil && hdr.Get(cmn.HeaderPutterID) != "" }

func isRedirect(q url.Values) (delta string) {
	if len(q) == 0 || q.Get(cmn.URLParamProxyID) == "" {
		return
	}
	return q.Get(cmn.URLParamUnixTime)
}

func requestLatency(started time.Time, ptime string) (delta int64) {
	pts, err := cos.S2UnixNano(ptime)
	if err != nil {
		glog.Errorf("unexpected: failed to convert %s to int, err: %v", ptime, err)
		return
	}
	delta = started.UnixNano() - pts
	if delta < 0 && -delta < int64(clusterClockDrift) {
		delta = 0
	}
	return
}

//
// IPV4
//

// Local unicast IP info
type localIPv4Info struct {
	ipv4 string
	mtu  int
}

// getLocalIPv4List returns a list of local unicast IPv4 with MTU
func getLocalIPv4List() (addrlist []*localIPv4Info, err error) {
	addrlist = make([]*localIPv4Info, 0, 4)
	addrs, e := net.InterfaceAddrs()
	if e != nil {
		err = fmt.Errorf("failed to get host unicast IPs, err: %v", e)
		return
	}
	iflist, e := net.Interfaces()
	if e != nil {
		err = fmt.Errorf("failed to get interface list: %v", e)
		return
	}

	var (
		testingEnv  = cmn.GCO.Get().TestingEnv()
		k8sDetected = k8s.Detect() == nil
	)
	for _, addr := range addrs {
		curr := &localIPv4Info{}
		if ipnet, ok := addr.(*net.IPNet); ok {
			// Ignore loopback addresses in production env.
			if ipnet.IP.IsLoopback() && (!testingEnv || k8sDetected) {
				continue
			}
			if ipnet.IP.To4() == nil {
				continue
			}
			curr.ipv4 = ipnet.IP.String()
		}

		for _, intf := range iflist {
			ifAddrs, e := intf.Addrs()
			// skip invalid interfaces
			if e != nil {
				continue
			}
			for _, ifAddr := range ifAddrs {
				if ipnet, ok := ifAddr.(*net.IPNet); ok && ipnet.IP.To4() != nil && ipnet.IP.String() == curr.ipv4 {
					curr.mtu = intf.MTU
					addrlist = append(addrlist, curr)
					break
				}
			}
			if curr.mtu != 0 {
				break
			}
		}
	}

	if len(addrlist) == 0 {
		return addrlist, fmt.Errorf("the host does not have any IPv4 addresses")
	}

	return addrlist, nil
}

// selectConfiguredHostname returns the first Hostname from a preconfigured Hostname list that
// matches any local unicast IPv4
func selectConfiguredHostname(addrlist []*localIPv4Info, configuredList []string) (hostname string, err error) {
	glog.Infof("Selecting one of the configured IPv4 addresses: %s...", configuredList)

	var localList, ipv4 string
	for i, host := range configuredList {
		if net.ParseIP(host) != nil {
			ipv4 = strings.TrimSpace(host)
		} else if ip, err := getFQNIPv4(host); err == nil {
			ipv4 = ip.String()
		} else {
			glog.Errorf("failed to parse hostname %q", host)
			continue
		}
		for _, localaddr := range addrlist {
			if i == 0 {
				localList += " " + localaddr.ipv4
			}
			if localaddr.ipv4 == ipv4 {
				glog.Warningf("Selected IPv4 %s from the configuration file", ipv4)
				return host, nil
			}
		}
	}

	glog.Errorf("Configured Hostname does not match any local one.\nLocal IPv4 list:%s; Configured ip: %s",
		localList, configuredList)
	return "", fmt.Errorf("configured Hostname does not match any local one")
}

// detectLocalIPv4 takes a list of local IPv4s and returns the best fit for a daemon to listen on it
func detectLocalIPv4(addrList []*localIPv4Info) (ip net.IP, err error) {
	if len(addrList) == 0 {
		return nil, fmt.Errorf("no addresses to choose from")
	} else if len(addrList) == 1 {
		glog.Infof("Found only one IPv4: %s, MTU %d", addrList[0].ipv4, addrList[0].mtu)
		if addrList[0].mtu <= 1500 {
			glog.Warningf("IPv4 %s MTU size is small: %d\n", addrList[0].ipv4, addrList[0].mtu)
		}
		if ip = net.ParseIP(addrList[0].ipv4); ip == nil {
			return nil, fmt.Errorf("failed to parse IP address: %s", addrList[0].ipv4)
		}
		return ip, nil
	}

	glog.Warningf("%d IPv4s available", len(addrList))
	for _, addr := range addrList {
		glog.Warningf("    %#v\n", *addr)
	}
	if ip = net.ParseIP(addrList[0].ipv4); ip == nil {
		return nil, fmt.Errorf("failed to parse IP address: %s", addrList[0].ipv4)
	}
	return ip, nil
}

// getNetInfo returns an Hostname for proxy/target to listen on it.
// 1. If there is an Hostname in config - it tries to use it
// 2. If config does not contain Hostname - it chooses one of local IPv4s
func getNetInfo(addrList []*localIPv4Info, proto, configuredIPv4s, port string) (netInfo cluster.NetInfo, err error) {
	var ip net.IP
	if configuredIPv4s == "" {
		ip, err = detectLocalIPv4(addrList)
		if err != nil {
			return netInfo, err
		}
		netInfo = *cluster.NewNetInfo(proto, ip.String(), port)
		return
	}

	configuredList := strings.Split(configuredIPv4s, ",")
	selHostname, err := selectConfiguredHostname(addrList, configuredList)
	if err != nil {
		return netInfo, err
	}

	netInfo = *cluster.NewNetInfo(proto, selHostname, port)
	return
}

func getFQNIPv4(fqn string) (net.IP, error) {
	ips, err := net.LookupIP(fqn)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("failed to get IPv4 for FQN %s", fqn)
}

func validateHostname(hostname string) (err error) {
	if net.ParseIP(hostname) != nil {
		return
	}
	_, err = getFQNIPv4(hostname)
	return
}

/////////////
// helpers //
/////////////

func reMirror(bprops, nprops *cmn.BucketProps) bool {
	if !bprops.Mirror.Enabled && nprops.Mirror.Enabled {
		return true
	}
	if bprops.Mirror.Enabled && nprops.Mirror.Enabled {
		return bprops.Mirror.Copies != nprops.Mirror.Copies
	}
	return false
}

func reEC(bprops, nprops *cmn.BucketProps, bck *cluster.Bck) bool {
	// TODO: xaction to remove all data generated by EC encoder.
	// For now, do nothing if EC is disabled.
	if !nprops.EC.Enabled {
		if bprops.EC.Enabled {
			// kill running ec-encode xact if it is active
			xreg.DoAbort(cmn.ActECEncode, bck)
		}
		return false
	}
	if !bprops.EC.Enabled {
		return true
	}
	return bprops.EC.DataSlices != nprops.EC.DataSlices ||
		bprops.EC.ParitySlices != nprops.EC.ParitySlices
}

func withRetry(cond func() bool) (ok bool) {
	if ok = cond(); !ok {
		time.Sleep(time.Second)
		ok = cond()
	}
	return
}

func isETLRequest(query url.Values) bool {
	return query.Get(cmn.URLParamUUID) != ""
}

func deploymentType() string {
	if k8s.Detect() == nil {
		if k8s.AllowOneNodeManyETLs {
			return "K8s dev"
		}
		return "K8s"
	} else if cmn.GCO.Get().TestingEnv() {
		return "dev"
	}
	return runtime.GOOS
}
