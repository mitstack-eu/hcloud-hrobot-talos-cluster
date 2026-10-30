// Package robotworker writes talos-cluster-kit's metal image onto an adopted Robot server from its rescue system.
package robotworker

import (
	"context"
	_ "embed"
	"fmt"
	"net/netip"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/rescuessh"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
)

//go:embed scripts/rescue.sh
var rescueScript []byte

// ImageServerImage carries docker, zstd and python3.
const ImageServerImage = "docker-ce"

type Worker struct {
	Server   int64
	Name     string
	VSwitch  string
	Hostname string
	// Address lies on the vSwitch subnet, whose first address is Hetzner's vSwitch gateway.
	Address netip.Prefix
	// Rules are the firewall input rules the cluster needs beyond the replies to the node's own traffic and ICMP.
	Rules []robot.Rule
}

type Installer struct {
	Robot   *robot.Client
	Cloud   robot.Cloud
	Cluster nodeconfig.Cluster
	// From is the public IPv4 address the rescue session leaves from, the one address the firewall admits SSH from.
	From          netip.Addr
	RescueAddress func(ip string) string
	DialInterval  time.Duration
}

// Install runs block-kit's Robot install with the image server as its short-lived server, which holds every key of
// the cluster in its user data.
func (i Installer) Install(ctx context.Context, w Worker) error {
	install := robot.Installer{Robot: i.Robot, Cloud: i.Cloud, Dial: i.dial, From: i.From, RescueAddress: i.RescueAddress}
	target := robot.Target{Server: w.Server, Name: w.Name, VSwitch: w.VSwitch, Address: w.Address, Firewall: Firewall(i.Cluster.Network, w)}
	return install.Install(ctx, target, func(hw robot.Hardware, vlan int, location string) (robot.HandOff, error) {
		return i.imageServer(w, hw, vlan, location)
	})
}

// Firewall is the worker's firewall: the replies to its own traffic, ICMP from the cluster's network, and w's rules.
func Firewall(network netip.Prefix, w Worker) robot.Firewall {
	rules := append(robot.ReplyRules(), robot.Rule{Name: "icmp from network", IPVersion: "ipv4", SrcIP: network.String(), Protocol: "icmp", Action: "accept"})
	return robot.Firewall{Status: "active", FilterIPv6: true, WhitelistHOS: true, Input: append(rules, w.Rules...)}
}

func (i Installer) dial(ctx context.Context, addr string, key robot.RescueKey, s robot.RescueSession) (robot.RescueShell, error) {
	session, err := rescuessh.Config{Script: rescueScript, Interval: i.DialInterval}.Dial(ctx, addr, key.PrivateKey(), s.Server, s.HostKeys)
	if err != nil {
		// A nil *rescuessh.Session inside the interface would read as a live shell.
		return nil, err
	}
	return session, nil
}

// imageServer refuses a server that did not boot by UEFI, the only boot the trial proved, and otherwise renders the
// worker configuration for the hardware read into the image server.
func (i Installer) imageServer(w Worker, hw robot.Hardware, vlan int, location string) (robot.HandOff, error) {
	if !hw.EFI {
		return robot.HandOff{}, handler.Refuse("firmware-not-uefi", "the server's firmware did not boot the rescue system by UEFI, and Talos's unified kernel image is written only for a UEFI boot")
	}
	vswitch := &nodeconfig.VSwitch{MAC: hw.MAC, VLAN: uint16(vlan), Address: w.Address}
	cluster := i.Cluster
	cluster.Profile = nodeconfig.Metal
	config, err := nodeconfig.RenderWorker(cluster, nodeconfig.Node{Hostname: w.Hostname, VSwitch: vswitch})
	if err != nil {
		return robot.HandOff{}, fmt.Errorf("rendering the worker configuration of server %d: %w", w.Server, err)
	}
	key, err := newServeKey()
	if err != nil {
		return robot.HandOff{}, err
	}
	userData, err := imageServerUserData(config, key, i.Cluster.Network, w.Address.Addr())
	if err != nil {
		return robot.HandOff{}, err
	}
	server := robot.ShortLivedServer{Name: w.Name + "-image", Location: location, Image: ImageServerImage, UserData: userData}
	return robot.HandOff{Server: server, Port: servePort, Pin: key.Pin}, nil
}
