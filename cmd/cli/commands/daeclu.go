// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This file handles cluster and daemon operations.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmd/cli/templates"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/ios"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/xaction"
	"github.com/urfave/cli"
	"golang.org/x/sync/errgroup"
)

type (
	targetDiskStats struct {
		targetID string
		stats    map[string]*ios.SelectedDiskStats
	}

	targetRebStats struct {
		targetID string
		stats    *xaction.BaseXactStatsExt
	}
)

var (
	proxy  = make(map[string]*stats.DaemonStatus)
	target = make(map[string]*stats.DaemonStatus)
)

// Displays smap of single daemon
func clusterSmap(c *cli.Context, primarySmap *cluster.Smap, daemonID string, useJSON bool) error {
	var (
		smap = primarySmap
		err  error
	)

	if daemonID != "" {
		smap, err = api.GetNodeClusterMap(defaultAPIParams, daemonID)
		if err != nil {
			return err
		}
	}

	extendedURLs := false
	for _, m := range []cluster.NodeMap{smap.Tmap, smap.Pmap} {
		for _, v := range m {
			if v.PublicNet != v.IntraControlNet || v.PublicNet != v.IntraDataNet {
				extendedURLs = true
			}
		}
	}

	body := templates.SmapTemplateHelper{
		Smap:         smap,
		ExtendedURLs: extendedURLs,
	}
	return templates.DisplayOutput(body, c.App.Writer, templates.SmapTmpl, useJSON)
}

// Displays the status of the cluster or daemon
func clusterDaemonStatus(c *cli.Context, smap *cluster.Smap, daemonID string, useJSON, hideHeader, verbose bool) error {
	body := templates.StatusTemplateHelper{
		Smap: smap,
		Status: templates.DaemonStatusTemplateHelper{
			Pmap: proxy,
			Tmap: target,
		},
	}

	if res, proxyOK := proxy[daemonID]; proxyOK {
		return templates.DisplayOutput(res, c.App.Writer, templates.NewProxyTable(res, smap).Template(hideHeader), useJSON)
	} else if res, targetOK := target[daemonID]; targetOK {
		return templates.DisplayOutput(res, c.App.Writer, templates.NewTargetTable(res).Template(hideHeader), useJSON)
	} else if daemonID == cmn.Proxy {
		template := templates.NewProxiesTable(&body.Status, smap, true, verbose).Template(hideHeader)
		return templates.DisplayOutput(body, c.App.Writer, template, useJSON)
	} else if daemonID == cmn.Target {
		return templates.DisplayOutput(body, c.App.Writer,
			templates.NewTargetsTable(&body.Status, true, verbose).Template(hideHeader), useJSON)
	} else if daemonID == "" {
		template := templates.NewProxiesTable(&body.Status, smap, false, verbose).Template(false) + "\n" +
			templates.NewTargetsTable(&body.Status, false, verbose).Template(false) + "\n" +
			templates.ClusterSummary
		return templates.DisplayOutput(body, c.App.Writer, template, useJSON)
	}
	return fmt.Errorf("%s is not a valid DAEMON_ID nor DAEMON_TYPE", daemonID)
}

// Displays the disk stats of a target
func daemonDiskStats(c *cli.Context, daemonID string, useJSON, hideHeader bool) error {
	if _, ok := proxy[daemonID]; ok {
		return fmt.Errorf("daemon with ID %q is a proxy, but \"%s %s %s\" works only for targets", daemonID, cliName, commandShow, subcmdShowDisk)
	}
	if _, ok := target[daemonID]; daemonID != "" && !ok {
		return fmt.Errorf("target ID %q invalid - no such target", daemonID)
	}

	targets := map[string]*stats.DaemonStatus{daemonID: {}}
	if daemonID == "" {
		targets = target
	}

	diskStats, err := getDiskStats(targets)
	if err != nil {
		return err
	}

	template := chooseTmpl(templates.DiskStatBodyTmpl, templates.DiskStatsFullTmpl, hideHeader)
	err = templates.DisplayOutput(diskStats, c.App.Writer, template, useJSON)
	if err != nil {
		return err
	}

	return nil
}

func getDiskStats(targets map[string]*stats.DaemonStatus) ([]templates.DiskStatsTemplateHelper, error) {
	var (
		allStats = make([]templates.DiskStatsTemplateHelper, 0, len(targets))
		wg, _    = errgroup.WithContext(context.Background())
		statsCh  = make(chan targetDiskStats, len(targets))
	)

	for targetID := range targets {
		wg.Go(func(targetID string) func() error {
			return func() (err error) {
				diskStats, err := api.GetTargetDiskStats(defaultAPIParams, targetID)
				if err != nil {
					return err
				}

				statsCh <- targetDiskStats{stats: diskStats, targetID: targetID}
				return nil
			}
		}(targetID))
	}

	err := wg.Wait()
	close(statsCh)
	if err != nil {
		return nil, err
	}

	for diskStats := range statsCh {
		targetID := diskStats.targetID
		for diskName, diskStat := range diskStats.stats {
			allStats = append(allStats, templates.DiskStatsTemplateHelper{TargetID: targetID, DiskName: diskName, Stat: diskStat})
		}
	}

	return allStats, nil
}

// Displays the config of a daemon
func getDaemonConfig(c *cli.Context) error {
	var (
		daemonID = c.Args().Get(0)
		section  = c.Args().Get(1)
		useJSON  = flagIsSet(c, jsonFlag)
		node     *cluster.Snode
	)

	if c.NArg() == 0 {
		return missingArgumentsError(c, "daemon ID")
	}

	smap, err := api.GetClusterMap(defaultAPIParams)
	if err != nil {
		return err
	}
	if node = smap.GetNode(daemonID); node == nil {
		return fmt.Errorf("%s does not exist in the cluster (see 'ais show cluster')", daemonID)
	}

	body, err := api.GetDaemonConfig(defaultAPIParams, node)
	if err != nil {
		return err
	}

	if useJSON {
		return templates.DisplayOutput(body, c.App.Writer, "", useJSON)
	}

	flat := flattenConfig(body, section)
	return templates.DisplayOutput(flat, c.App.Writer, templates.DaemonConfTmpl, false)
}

// Sets config of specific daemon or cluster
func cluConfig(c *cli.Context) error {
	daemonID, nvs, err := daemonKeyValueArgs(c)
	if err != nil {
		return err
	}

	if daemonID == "" {
		if err := api.SetClusterConfig(defaultAPIParams, nvs, flagIsSet(c, transientFlag)); err != nil {
			return err
		}

		fmt.Fprintf(c.App.Writer, "config successfully updated\n")
		return nil
	}

	if err := api.SetDaemonConfig(defaultAPIParams, daemonID, nvs, flagIsSet(c, transientFlag)); err != nil {
		return err
	}

	fmt.Fprintf(c.App.Writer, "config for node %q successfully updated\n", daemonID)
	return nil
}

func daemonKeyValueArgs(c *cli.Context) (daemonID string, nvs cos.SimpleKVs, err error) {
	if c.NArg() == 0 {
		return "", nil, missingArgumentsError(c, "attribute name-value pairs")
	}

	args := c.Args()
	daemonID = args.First()
	kvs := args.Tail()

	// Case when DAEMON_ID is not provided by the user:
	// 1. name-value pair separated with '=': `ais set log.level=5`
	// 2. name-value pair separated with space: `ais set log.level 5`. In this case
	//		the first word is looked up in cmn.ConfigPropList
	propList := cmn.ConfigPropList()
	if cos.StringInSlice(args.First(), propList) || strings.Contains(args.First(), keyAndValueSeparator) {
		daemonID = ""
		kvs = args
	} else {
		var smap *cluster.Smap
		smap, err = api.GetClusterMap(defaultAPIParams)
		if err != nil {
			return "", nil, err
		}
		if smap.GetNode(daemonID) == nil {
			var err error
			if c.NArg()%2 == 0 {
				// Even - updating cluster configuration (a few key/value pairs)
				err = fmt.Errorf("option %q does not exist (hint: run 'show config DAEMON_ID --json' to show list of options)", daemonID)
			} else {
				// Odd - updating daemon configuration (daemon ID + a few key/value pairs)
				err = fmt.Errorf("node ID %q does not exist (hint: run 'show cluster' to show nodes)", daemonID)
			}
			return "", nil, err
		}
	}

	if len(kvs) == 0 {
		return "", nil, missingArgumentsError(c, "attribute name-value pairs")
	}

	if nvs, err = makePairs(kvs); err != nil {
		return "", nil, err
	}

	for k := range nvs {
		if !cos.StringInSlice(k, propList) {
			return "", nil, fmt.Errorf("invalid property name %q", k)
		}
	}

	return daemonID, nvs, nil
}

func showRebalance(c *cli.Context, keepMonitoring bool, refreshRate time.Duration) error {
	var (
		latestAborted  bool = false
		latestFinished bool = false
	)

	tw := &tabwriter.Writer{}
	tw.Init(c.App.Writer, 0, 8, 2, ' ', 0)

	// run until rebalance is completed
	xactArgs := api.XactReqArgs{Kind: cmn.ActRebalance}
	for {
		rebStats, err := api.QueryXactionStats(defaultAPIParams, xactArgs)
		if err != nil {
			switch err := err.(type) {
			case *cmn.ErrHTTP:
				if err.Status == http.StatusNotFound {
					fmt.Fprintln(c.App.Writer, "Rebalance has not started yet.")
					return nil
				}
				return err
			default:
				return err
			}
		}

		allStats := make([]*targetRebStats, 0, 100)
		for daemonID, daemonStats := range rebStats {
			for _, sts := range daemonStats {
				allStats = append(allStats, &targetRebStats{
					targetID: daemonID,
					stats:    sts,
				})
			}
		}
		sort.Slice(allStats, func(i, j int) bool {
			if allStats[i].stats.ID() != allStats[j].stats.ID() {
				return allStats[i].stats.ID() > allStats[j].stats.ID()
			}
			return allStats[i].targetID < allStats[j].targetID
		})

		// NOTE: If changing header do not forget to change `colCount` couple
		//  lines below and `displayRebStats` logic.
		fmt.Fprintln(tw, "REB ID\t DAEMON ID\t OBJECTS RECV\t SIZE RECV\t OBJECTS SENT\t SIZE SENT\t START TIME\t END TIME\t ABORTED")
		prevID := ""
		for _, sts := range allStats {
			if flagIsSet(c, allXactionsFlag) {
				if prevID != "" && sts.stats.ID() != prevID {
					fmt.Fprintln(tw, strings.Repeat("\t ", 9 /*colCount*/))
				}
				displayRebStats(tw, sts)
			} else {
				if prevID != "" && sts.stats.ID() != prevID {
					break
				}
				latestAborted = latestAborted || sts.stats.AbortedX
				latestFinished = latestFinished || !sts.stats.EndTime().IsZero()
				displayRebStats(tw, sts)
			}
			prevID = sts.stats.ID()
		}
		tw.Flush()

		if !flagIsSet(c, allXactionsFlag) {
			if latestFinished && latestAborted {
				fmt.Fprintln(c.App.Writer, "\nRebalance aborted.")
				break
			} else if latestFinished {
				fmt.Fprintln(c.App.Writer, "\nRebalance completed.")
				break
			}
		}

		if !keepMonitoring {
			break
		}

		time.Sleep(refreshRate)
	}

	return nil
}

func displayRebStats(tw *tabwriter.Writer, st *targetRebStats) {
	extRebStats := &stats.ExtRebalanceStats{}
	if err := cos.MorphMarshal(st.stats.Ext, &extRebStats); err != nil {
		return
	}

	endTime := "-"
	if !st.stats.EndTime().IsZero() {
		endTime = st.stats.EndTime().Format("01-02 15:04:05")
	}
	startTime := st.stats.StartTime().Format("01-02 15:04:05")

	fmt.Fprintf(tw,
		"%s\t %s\t %d\t %s\t %d\t %s\t %s\t %s\t %t\n",
		st.stats.ID(), st.targetID,
		extRebStats.RebRxCount, cos.B2S(extRebStats.RebRxSize, 2),
		extRebStats.RebTxCount, cos.B2S(extRebStats.RebTxSize, 2),
		startTime, endTime, st.stats.Aborted(),
	)
}
