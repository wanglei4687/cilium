// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package types

import (
	"os"
	"sync"

	"github.com/cilium/cilium/pkg/defaults"
	k8sConsts "github.com/cilium/cilium/pkg/k8s/constants"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/option"
)

var (
	nodeName             = "localhost"
	absoluteNodeName     = nodeName
	absoluteNodeNameOnce sync.Once
)

// SetName sets the name of the local node. This will overwrite the value that
// is automatically retrieved with `os.Hostname()`.
//
// Note: This function is currently designed to only be called during the
// bootstrapping procedure of the agent where no parallelism exists. If you
// want to use this function in later stages, a mutex must be added first.
func SetName(name string) {
	nodeName = name
	absoluteNodeName = getAbsoluteNodeName()
}

// GetName returns the name of the local node. The value returned was either
// previously set with SetName(), retrieved via `os.Hostname()`, or as a last
// resort is hardcoded to "localhost".
func GetName() string {
	return nodeName
}

// GetAbsoluteNodeName returns the absolute node name combined of both
// (prefixed)cluster name and the local node name in case of
// clustered environments otherwise returns the name of the local node.
func GetAbsoluteNodeName() string {
	absoluteNodeNameOnce.Do(func() {
		absoluteNodeName = getAbsoluteNodeName()
	})

	return absoluteNodeName
}

func getAbsoluteNodeName() string {
	if clusterName := GetClusterName(); clusterName != "" {
		return clusterName + "/" + nodeName
	} else {
		return nodeName
	}
}

func GetClusterName() string {
	if option.Config.ClusterName != "" &&
		option.Config.ClusterName != defaults.ClusterName {
		return option.Config.ClusterName
	} else {
		return ""
	}
}

func init() {
	// Give priority to the environment variable available in the Cilium agent
	if name := os.Getenv(k8sConsts.EnvNodeNameSpec); name != "" {
		nodeName = name
		return
	}
	if h, err := os.Hostname(); err != nil {
		// slogloggercheck: it's safe to use the default logger as it's for a warning unlikely to happen.
		logging.DefaultSlogLogger.Warn("Unable to retrieve local hostname", logfields.Error, err)
	} else {
		// slogloggercheck: it's safe to use the default logger as it's for a debug message.
		logging.DefaultSlogLogger.Debug("os.Hostname() returned", logfields.NodeName, h)
		nodeName = h
	}
}
