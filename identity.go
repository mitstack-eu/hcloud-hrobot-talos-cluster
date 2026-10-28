package main

// Robot holds no labels, so each Robot resource carries the subscription in its name.

import (
	"strings"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
)

const blockID = "hcloud-hrobot-talos-cluster"

func newPlane(api controlplane.API, subID, appName string) controlplane.Plane {
	return controlplane.Plane{
		API:           api,
		Block:         blockID,
		Subscription:  subID,
		AppName:       appName,
		ServerType:    controlplane.DefaultServerType,
		Boundary:      userDataBoundary,
		PlatformURL:   platformURL,
		Now:           clock,
		AppsOnWorkers: true,
	}
}

// robotName keeps the whole subscription id, so no other subscription's name can match it.
func robotName(subID string) string {
	return robot.NamePrefix + strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, strings.ToLower(subID))
}
