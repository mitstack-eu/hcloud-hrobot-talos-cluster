// Package robotworker writes talos-cluster-kit's metal image onto an adopted Robot server from its rescue system.
package robotworker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
)

// Locations are where a Robot server's vSwitch reaches the cluster's Cloud network.
var Locations = []string{"fsn1", "nbg1", "hel1"}

// ImageServerImage carries docker, zstd and python3.
const ImageServerImage = "docker-ce"

const cleanupTimeout = 10 * time.Minute

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

type ImageServer struct {
	Name, Location, Image string
	// UserData holds every key of the cluster, so the server is deleted after use.
	UserData string
}

// Cloud places the image server in the cluster's Cloud network, whose subnet is coupled to the vSwitch.
type Cloud interface {
	CreateImageServer(ctx context.Context, s ImageServer) (int64, error)
	ImageServerAddress(ctx context.Context, id int64) (netip.Addr, error)
	DeleteImageServer(ctx context.Context, id int64) error
}

type Installer struct {
	Robot   *robot.Client
	Cloud   Cloud
	Cluster nodeconfig.Cluster
	// From is the public IPv4 address the rescue session leaves from, the one address the firewall admits SSH from.
	From          netip.Addr
	RescueAddress func(ip string) string
	DialInterval  time.Duration
}

// Install runs once from adoption to the reset that boots the written disk, taking longer than a subscribe waits,
// so a block runs it in the background. It refuses a server outside Locations before any write.
func (i Installer) Install(ctx context.Context, w Worker) (err error) {
	server, err := i.Robot.GetServer(ctx, w.Server)
	if err != nil && robot.ErrorCode(err) != "SERVER_NOT_FOUND" {
		return fmt.Errorf("reading dedicated server %d: %w", w.Server, err)
	}
	if err == nil && !slices.Contains(Locations, server.Location()) {
		return handler.Refuse("location-unsupported", "dedicated server %d is in %s, and a Robot worker is installed only in fsn1, nbg1 or hel1, where its vSwitch reaches the cluster's Cloud network, so nothing was changed",
			w.Server, server.DC)
	}
	if server, err = i.Robot.Adopt(ctx, w.Server, w.Name); err != nil {
		return err
	}
	vs, err := i.Robot.Couple(ctx, w.VSwitch, w.Server)
	if err != nil {
		return fmt.Errorf("coupling server %d to vSwitch %s: %w", w.Server, w.VSwitch, err)
	}
	if err := i.Robot.WriteFirewall(ctx, w.Server, Firewall(i.Cluster.Network, w)); err != nil {
		return fmt.Errorf("writing the worker firewall of server %d: %w", w.Server, err)
	}
	restore, err := i.Robot.OpenRescueWindow(ctx, w.Server, i.From)
	if err != nil {
		return err
	}
	job := &install{Installer: i, worker: w, server: server, vlan: vs.VLAN}
	defer func() { err = errors.Join(err, job.cleanUp(ctx, restore)) }()
	return job.run(ctx)
}

// Firewall is the worker's firewall: the replies to its own traffic, ICMP from the cluster's network, and w's rules.
func Firewall(network netip.Prefix, w Worker) robot.Firewall {
	rules := append(robot.ReplyRules(), robot.Rule{Name: "icmp from network", IPVersion: "ipv4", SrcIP: network.String(), Protocol: "icmp", Action: "accept"})
	return robot.Firewall{Status: "active", FilterIPv6: true, WhitelistHOS: true, Input: append(rules, w.Rules...)}
}

type install struct {
	Installer
	worker  Worker
	server  robot.Server
	vlan    int
	session *robot.RescueSession
	imageID int64
}

func (j *install) run(ctx context.Context) error {
	keyName := j.worker.Name + "-rescue"
	// An install that died before its cleanup leaves its key behind.
	if err := j.Robot.DeleteKeys(ctx, keyName); err != nil {
		return err
	}
	key, err := robot.NewRescueKey()
	if err != nil {
		return err
	}
	session, err := j.Robot.BootRescue(ctx, j.worker.Server, keyName, key)
	if session.Fingerprint != "" {
		j.session = &session
	}
	if err != nil {
		return err
	}
	rescue, err := dialRescue(ctx, j.rescueAddress(), key, session, j.dialInterval())
	if err != nil {
		return err
	}
	defer rescue.Close()
	hw, err := readHardware(ctx, rescue)
	if err != nil {
		return err
	}
	vswitch := &nodeconfig.VSwitch{MAC: hw.mac, VLAN: uint16(j.vlan), Address: j.worker.Address}
	cluster := j.Cluster
	cluster.Profile = nodeconfig.Metal
	config, err := nodeconfig.RenderWorker(cluster, nodeconfig.Node{Hostname: j.worker.Hostname, VSwitch: vswitch})
	if err != nil {
		return fmt.Errorf("rendering the worker configuration of server %d: %w", j.worker.Server, err)
	}
	serve, err := j.startImageServer(ctx, config)
	if err != nil {
		return err
	}
	if err := writeImage(ctx, rescue, j.vlan, j.worker.Address, serve, hw.disk); err != nil {
		return fmt.Errorf("writing the image onto server %d: %w", j.worker.Server, err)
	}
	return nil
}

func (j *install) startImageServer(ctx context.Context, config []byte) (imageFetch, error) {
	key, err := newServeKey()
	if err != nil {
		return imageFetch{}, err
	}
	userData, err := imageServerUserData(config, key, j.Cluster.Network, j.worker.Address.Addr())
	if err != nil {
		return imageFetch{}, err
	}
	id, err := j.Cloud.CreateImageServer(ctx, ImageServer{Name: j.worker.Name + "-image", Location: j.server.Location(), Image: ImageServerImage, UserData: userData})
	if err != nil {
		return imageFetch{}, fmt.Errorf("creating the image server of server %d: %w", j.worker.Server, err)
	}
	j.imageID = id
	addr, err := j.Cloud.ImageServerAddress(ctx, id)
	if err != nil {
		return imageFetch{}, fmt.Errorf("reading the address of image server %d: %w", id, err)
	}
	return imageFetch{server: addr, pin: key.pin}, nil
}

// cleanUp runs after every run, written or failed: it deletes the image server and the rescue key, restores the
// worker firewall and, once the rescue boot began, resets the server so it boots its disk.
func (j *install) cleanUp(ctx context.Context, restore func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	var errs []error
	if j.imageID != 0 {
		errs = append(errs, j.Cloud.DeleteImageServer(ctx, j.imageID))
	}
	if j.session != nil {
		errs = append(errs, j.Robot.LeaveRescue(ctx, *j.session))
	}
	errs = append(errs, restore(ctx))
	if j.session != nil {
		errs = append(errs, j.Robot.Reset(ctx, j.worker.Server))
	}
	return errors.Join(errs...)
}

func (j *install) rescueAddress() string {
	if j.RescueAddress != nil {
		return j.RescueAddress(j.server.IP)
	}
	return net.JoinHostPort(j.server.IP, "22")
}

func (j *install) dialInterval() time.Duration {
	if j.DialInterval == 0 {
		return 10 * time.Second
	}
	return j.DialInterval
}
