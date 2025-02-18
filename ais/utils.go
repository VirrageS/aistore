// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/k8s"
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

// Network access of handlers (Public, IntraControl, & IntraData)
type netAccess int

func (na netAccess) isSet(flag netAccess) bool {
	return na&flag == flag
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
		err = fmt.Errorf("failed to get host unicast IPs, err: %w", e)
		return
	}
	iflist, e := net.Interfaces()
	if e != nil {
		err = fmt.Errorf("failed to get interface list: %w", e)
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
		} else {
			glog.Warningf("failed to parse IP for hostname %q", host)
			ip, err := resolveHostIPv4(host)
			if err != nil {
				glog.Errorf("failed to get IPv4 for host=%q; err %v", host, err)
				continue
			}
			ipv4 = ip.String()
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
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("%d IPv4s:", len(addrList))
		for _, addr := range addrList {
			glog.Infof("    %#v\n", *addr)
		}
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

func resolveHostIPv4(hostName string) (net.IP, error) {
	ips, err := net.LookupIP(hostName)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("failed to find non-empty IPv4 in list %v (hostName=%q)", ips, hostName)
}

func validateHostname(hostname string) (err error) {
	if net.ParseIP(hostname) != nil {
		return
	}
	_, err = resolveHostIPv4(hostname)
	return
}

/////////////
// helpers //
/////////////

func deploymentType() string {
	if k8s.Detect() == nil {
		return apc.DeploymentK8s
	} else if cmn.GCO.Get().TestingEnv() {
		return apc.DeploymentDev
	}
	return runtime.GOOS
}

// for AIS metadata filenames (constants), see `cmn/fname` package
func cleanupConfigDir(name string, keepInitialConfig bool) {
	if !keepInitialConfig {
		// remove plain-text (initial) config
		cos.RemoveFile(daemon.cli.globalConfigPath)
		cos.RemoveFile(daemon.cli.localConfigPath)
	}
	config := cmn.GCO.Get()
	filepath.Walk(config.ConfigDir, func(path string, finfo os.FileInfo, err error) error {
		if strings.HasPrefix(finfo.Name(), ".ais.") {
			if err := cos.RemoveFile(path); err != nil {
				glog.Errorf("%s: failed to cleanup %q, err: %v", name, path, err)
			}
		}
		return nil
	})
}
