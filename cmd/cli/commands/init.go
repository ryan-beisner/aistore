// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmd/cli/config"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/jsp"
)

func initAuthParams() {
	tokenPath := os.Getenv(authnTokenPath)
	custom := tokenPath != ""
	if tokenPath == "" {
		tokenPath = filepath.Join(config.ConfigDirPath, tokenFile)
	}
	_, err := jsp.LoadMeta(tokenPath, &loggedUserToken)
	if err != nil && custom {
		fmt.Fprintf(os.Stderr, "Failed to read token from %q: %v\n", tokenPath, err)
	}
}

func initClusterParams() {
	initAuthParams()

	clusterURL = determineClusterURL(cfg)
	defaultHTTPClient = cmn.NewClient(cmn.TransportArgs{
		DialTimeout: cfg.Timeout.TCPTimeout,
		Timeout:     cfg.Timeout.HTTPTimeout,
		UseHTTPS:    cos.IsHTTPS(clusterURL),
		SkipVerify:  cfg.Cluster.SkipVerifyCrt,

		IdleConnsPerHost: 100,
		MaxIdleConns:     100,
	})

	if authnURL := cliAuthnURL(cfg); authnURL != "" {
		authnHTTPClient = cmn.NewClient(cmn.TransportArgs{
			DialTimeout: cfg.Timeout.TCPTimeout,
			Timeout:     cfg.Timeout.HTTPTimeout,
			UseHTTPS:    cos.IsHTTPS(authnURL),
			SkipVerify:  cfg.Cluster.SkipVerifyCrt,
		})

		authParams = api.BaseParams{
			Client: authnHTTPClient,
			URL:    authnURL,
			Token:  loggedUserToken.Token,
		}
	}

	defaultAPIParams = api.BaseParams{
		Client: defaultHTTPClient,
		URL:    clusterURL,
		Token:  loggedUserToken.Token,
	}
}

func Init() (err error) {
	unreachableRegex = regexp.MustCompile("dial.*(timeout|refused)")
	cfg, err = config.Load()
	if err != nil {
		return err
	}
	initClusterParams()
	return nil
}
