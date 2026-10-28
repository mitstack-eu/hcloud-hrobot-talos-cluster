package main

// The block holds the kit's block guards, served as the engine reaches it, over fakes whose builder finishes at once.

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/mitstack-eu/block-kit/verify/kittest"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/type-snapshot.json
var typeSnapshotJSON []byte

// clusterProvider lists what carries the subscription's labels, topics or Robot name, and loses a declared resource
// from under the block.
type clusterProvider struct{ e *env }

func (p clusterProvider) ListTagged(ctx context.Context, subID string) ([]observe.Tagged, error) {
	st, err := newPlane(p.e.cloud, subID, testApp).Read(ctx)
	if err != nil {
		return nil, err
	}
	var tagged []observe.Tagged
	add := func(typ string, ids []int64) {
		for _, id := range ids {
			tagged = append(tagged, observe.Tagged{Type: typ, ID: strconv.FormatInt(id, 10)})
		}
	}
	add(resNetwork, controlplane.IDs(st.Networks, networkID))
	add(resFirewall, controlplane.IDs(st.Firewalls, firewallID))
	for _, lbs := range st.LoadBalancers {
		add(typeLoadBalancer, controlplane.IDs(lbs, controlplane.LoadBalancerID))
	}
	for _, servers := range st.Servers {
		add(typeServer, controlplane.IDs(servers, controlplane.ServerID))
	}
	// The snapshot is the cluster's own part, which the cluster's absence statement names.
	add(resCluster, controlplane.IDs(st.Snapshots, func(s controlplane.Snapshot) int64 { return s.ID }))
	if carriesMark(p.e.github.topics(p.e.repo()), subID) {
		tagged = append(tagged, observe.Tagged{Type: typeRepository, ID: p.e.repo()}, observe.Tagged{Type: resSeedCommit, ID: p.e.repo()})
	}
	if p.e.robot.State(testRobotServer).Name == robotName(subID) {
		add(typeDedicatedServer, []int64{testRobotServer})
		add(typeRobotFirewall, []int64{testRobotServer})
	}
	for _, v := range p.e.subscriptionVSwitches() {
		add(typeVSwitch, []int64{v.ID})
	}
	return tagged, nil
}

func (p clusterProvider) Lose(ctx context.Context, r contract.CreatedResource) error {
	id := func(key string) int64 {
		n, _ := strconv.ParseInt(r.Identity[key], 10, 64)
		return n
	}
	c := p.e.cloud
	switch r.Type {
	case resNetwork:
		return c.DeleteNetwork(ctx, id("networkId"))
	case resFirewall:
		return c.DeleteFirewall(ctx, id("firewallId"))
	case typeLoadBalancer:
		return c.DeleteLoadBalancer(ctx, id("loadBalancerId"))
	case typeServer:
		return c.DeleteServer(ctx, id("serverId"))
	case typeRepository:
		p.e.github.remove(r.Identity["repo"])
		return nil
	case resSeedCommit:
		p.e.github.removeFile(r.Identity["repo"] + "/contents/" + seedMarkerFile)
		return nil
	case resCluster:
		snaps, err := c.ListSnapshots(ctx, testPlane().Selector())
		for _, s := range snaps {
			err = c.DeleteSnapshot(ctx, s.ID)
		}
		return err
	case typeDedicatedServer:
		p.e.robot.EditServer(id("serverNumber"), func(_ *robottest.Server, st *robottest.State) { st.Name = "" })
		return nil
	case typeRobotFirewall:
		p.e.robot.SetFirewall(id("serverNumber"), robot.Firewall{Status: "disabled"})
		return nil
	case typeVSwitch:
		rb := p.e.robot.Client()
		if err := rb.Uncouple(ctx, id("vswitchId"), testRobotServer); err != nil {
			return err
		}
		return rb.CancelVSwitch(ctx, id("vswitchId"))
	}
	return fmt.Errorf("no declared resource is of type %q", r.Type)
}

// convergingBlock sends Ensure until it takes no step, as the engine's re-drives do, since each guard sends one
// subscribe, and runs the worker's install once the gateway can serve it.
type convergingBlock struct {
	*blockResource
	e *env
}

func (b convergingBlock) Ensure(ctx context.Context, d contract.Desired) error {
	for before := -1; before != b.e.changes(); {
		before = b.e.changes()
		if err := b.blockResource.Ensure(ctx, d); err != nil {
			return err
		}
		b.e.runJobs()
	}
	return nil
}

func (b convergingBlock) EnsureResources(ctx context.Context, d contract.Desired) ([]contract.CreatedResource, error) {
	if err := b.Ensure(ctx, d); err != nil {
		return nil, err
	}
	return b.blockResource.EnsureResources(ctx, d)
}

func (b convergingBlock) Delete(ctx context.Context, d contract.Desired) (map[string]string, error) {
	b.e.releaseJobs()
	return b.blockResource.Delete(ctx, d)
}

func blockUnderTest(t *testing.T) kittest.Block {
	t.Helper()
	e := newEnv(t)
	e.cloud.InstantBuild = true
	e.reports.report(testStart.Add(time.Hour), everyNode()...)
	snap, err := contract.ParseTypeSnapshot(typeSnapshotJSON)
	require.NoError(t, err)
	d := e.desired()
	d.BlockID, d.OrgID, d.AppID = blockID, "org_0123456789ab", "app_0123456789ab"
	cfg := handlerConfig("https://api.mitstack.test/.well-known/jwks.json", nil)
	cfg.Resource, cfg.Types = convergingBlock{blockResource: e.block, e: e}, snap
	return kittest.Block{
		Config:   cfg,
		Provider: clusterProvider{e: e},
		Desired:  d,
		Upgrade:  &d,
	}
}

// logRecorder keeps what AssertBlock logs about the guards it ran.
type logRecorder struct {
	testing.TB
	logs []string
}

func (r *logRecorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
	r.TB.Logf(format, args...)
}

func TestDeclaredResources(t *testing.T) {
	rec := &logRecorder{TB: t}

	kittest.AssertBlock(rec, blockUnderTest(t))

	ran := strings.Join(rec.logs, "\n")
	for _, guard := range []string{"tagging", "observe-asks-the-provider", "replacement", "absence-after-unsubscribe", "required-outputs"} {
		require.Contains(t, ran, guard, "guard %q did not run", guard)
	}
}
