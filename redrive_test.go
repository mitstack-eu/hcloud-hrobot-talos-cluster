package main

// One subscribe reaches the three running nodes and the written worker through the engine's re-drives, each call
// waiting on one Hetzner action at most.

import (
	"errors"
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/verify/kittest"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribe_OneSubscribeCarriesTheClusterToItsThreeNodesAndItsWorkerThroughTheEnginesRedrives(t *testing.T) {
	e := newEnv(t)
	p := e.engine()

	final := p.settle(nil)

	require.Equal(t, http.StatusOK, final.status, "the last answer: %s", final.detail)
	require.False(t, final.pending, "the last answer: %s", final.detail)
	assert.Len(t, e.byRole(controlplane.RoleNode), 3, "nodes")
	assert.Len(t, e.byRole(controlplane.RoleGateway), 1, "gateways")
	assert.Empty(t, e.byRole(controlplane.RoleBuilder), "a builder outlived its snapshot")
	assert.Len(t, e.installed(), 1, "worker installs")
	var subscribes int
	for _, a := range p.posted {
		if a.op == "subscribe" {
			subscribes++
			assert.True(t, a.pending, "a subscribe answered settled while the cluster was unfinished: %s", a.detail)
		}
	}
	assert.Greater(t, subscribes, 1, "subscribes: the engine never re-drove one")
}

func TestSubscribe_EachCallWaitsOnOneHetznerActionAtMostAndHoldsNoPoll(t *testing.T) {
	e := newEnv(t)
	p := e.engine()

	p.settle(nil)

	for _, a := range p.posted {
		assert.LessOrEqual(t, a.waits, 1, "the %s answering %q waited on %d Hetzner actions", a.op, a.detail, a.waits)
	}
	assert.Zero(t, p.sleeps, "the handler slept inside a call, holding it open")
}

func TestSubscribe_EachCallCreatesOneNodeAtMost(t *testing.T) {
	e := newEnv(t)

	e.engine().settle(nil)

	require.Len(t, e.byRole(controlplane.RoleNode), 3, "nodes")
	for i, nodes := range e.cloud.NodesPerCreate {
		assert.LessOrEqual(t, nodes, 1, "server create %d made %d nodes", i, nodes)
	}
}

func TestValidate_AsksForARedriveExactlyWhenASubscribeWouldTakeAStep(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.ensure())

	for start := e.now; e.observe().State != contract.Present && e.now.Sub(start) < settleBudget; e.now = e.now.Add(time.Minute) {
		e.progress()
		v := e.engine().post("validate")
		before := e.changes()
		err := e.ensure()

		assert.Equal(t, e.changes() != before || err != nil, v.redrive, "validate at %s: %s", e.now.Format(time.Kitchen), v.detail)
	}
	assert.Equal(t, contract.Present, e.observe().State)
}

func TestSubscribe_ARedriveSentTwiceAtEachSignalMakesEveryResourceOnce(t *testing.T) {
	e := newEnv(t)
	p := e.engine()
	require.True(t, p.post("subscribe").pending)

	for e.observe().State != contract.Present && e.now.Sub(testStart) < settleBudget {
		e.now = e.now.Add(time.Minute)
		e.progress()
		if p.post("validate").redrive {
			p.post("subscribe")
			p.post("subscribe")
		}
	}

	assert.Len(t, e.byRole(controlplane.RoleNode), 3, "nodes")
	assert.Len(t, e.byRole(controlplane.RoleGateway), 1, "gateways")
	assert.Empty(t, e.byRole(controlplane.RoleBuilder), "builders")
	assert.Len(t, e.installed(), 1, "worker installs")
	assert.Len(t, e.subscriptionVSwitches(), 1, "vSwitches")
	assert.Equal(t, 9, e.cloud.Count(), "four servers, two load balancers, a network, a firewall and a snapshot")
}

func TestVerbSemantics_ARedrivenSubscribeResumesTheBuildAndChangesNothingMadeBefore(t *testing.T) {
	e := newEnv(t)
	p := e.engine()

	kittest.AssertVerbSemantics(t, kittest.VerbSeams{
		Subscribe: func() (int, error) {
			err := p.settle(nil).err()
			return e.count(), err
		},
		Unsubscribe: func() (int, error) {
			err := p.post("unsubscribe").err()
			return e.count(), err
		},
		UnsubscribeWhenAbsent: func() error { return p.post("unsubscribe").err() },
		Upgrade: func() (bool, error) {
			before := e.cloud.Resources()
			err := p.post("upgrade").err()
			return maps.Equal(before, e.cloud.Resources()), err
		},
		ValidateWhileConverging: func() (bool, bool) {
			reports := e.reports.reports
			e.reports.reports = nil
			defer func() { e.reports.reports = reports }()
			v := p.post("validate")
			return v.pending, v.err() != nil
		},
		ValidateWhenDead: func() (bool, bool) {
			e.cloud.FailNext = errors.New("hetzner answered 401: the token was revoked")
			v := p.post("validate")
			return v.pending, v.err() != nil
		},
		Redrive: func() (kittest.RedriveRun, error) {
			e.reports.reports = nil
			if err := p.post("unsubscribe").err(); err != nil {
				return kittest.RedriveRun{}, err
			}
			e.buildTakes = 0
			first := p.settle(func() bool { return e.building() })
			e.cloud.PowerOff(e.byRole(controlplane.RoleBuilder)[0].ID)
			v := p.post("validate")
			before := e.cloud.Resources()
			last := p.settle(nil)
			changed := 0
			for id, was := range before {
				if e.cloud.Resources()[id] != was {
					changed++
				}
			}
			return kittest.RedriveRun{FirstPending: first.pending, Redrive: v.pending && v.redrive, Settled: !last.pending, Changed: changed}, last.err()
		},
	})
}
