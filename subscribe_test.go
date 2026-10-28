package main

// Subscribe builds the kit's Cloud control plane a step a call and installs the adopted Robot server as its worker in
// the background, from the call that renders the cluster's keys.

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribe_BuildsTheCloudControlPlaneAndInstallsTheRobotServerAsItsWorker(t *testing.T) {
	e := newEnv(t)

	obs := e.subscribe()

	require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
	assert.Len(t, e.byRole(controlplane.RoleNode), 3, "Cloud nodes")
	assert.Len(t, e.byRole(controlplane.RoleGateway), 1, "gateways")
	installs := e.installed()
	require.Len(t, installs, 1, "installs")
	name := robotName(testSub)
	assert.Equal(t, "mitstack-sub-0123456789ab", name)
	w := installs[0].worker
	assert.Equal(t, int64(testRobotServer), w.Server, "the worker's server")
	assert.Equal(t, name, w.Name, "the worker's Robot name")
	assert.Equal(t, name, w.VSwitch, "the worker's vSwitch")
	assert.Equal(t, testPlane().Name("worker"), w.Hostname, "the worker's hostname")
	assert.Equal(t, netip.MustParsePrefix("10.0.1.2/24"), w.Address, "the worker's vSwitch address")
	assert.Equal(t, []robot.Rule{{Name: "cluster network", IPVersion: "ipv4", SrcIP: "10.0.0.0/16", Action: "accept"}}, w.Rules)
	in := installs[0].installer
	assert.Equal(t, netip.MustParseAddr(testEgress), in.From, "the address the rescue window admits")
	assert.True(t, in.Cluster.AppsOnWorkers, "the cluster keeps app pods off its control plane")
	assert.NotNil(t, in.Cluster.Secrets, "the cluster the worker is rendered from carries its keys")
	assert.Equal(t, name, e.robot.State(testRobotServer).Name, "the Robot server's name")
	assert.True(t, holdsWorkerRules(e.robot.Firewall(testRobotServer), robotTarget{server: testRobotServer, plane: testPlane()}.firewall()),
		"the Robot firewall: %+v", e.robot.Firewall(testRobotServer))
	vswitches := e.subscriptionVSwitches()
	require.Len(t, vswitches, 1, "vSwitches")
	assert.Equal(t, []robot.VSwitchServer{{Number: testRobotServer, Status: "ready"}}, vswitches[0].Servers)
	network := e.network()
	assert.Equal(t, map[int64]string{vswitches[0].ID: "10.0.1.0/24"}, network.VSwitchSubnets, "the network's vSwitch subnets")
	assert.True(t, network.ExposeRoutesToVSwitch, "the network's routes reach the vSwitch")
	assert.Equal(t, workerWritten, network.Labels[keyWorkerInstall], "the worker's progress")
	assert.Empty(t, e.byRole(roleImageServer), "an image server outlived the install")
	assert.Equal(t, testPlane().Labels(roleImageServer), e.cloud.SpecFor(t, roleImageServer).Labels, "the image server's labels")
	assert.Equal(t, []int64{network.ID}, e.cloud.SpecFor(t, roleImageServer).NetworkIDs, "the image server's networks")
}

func TestSubscribe_TheImageServerWaitsForTheGatewayToHoldItsAddress(t *testing.T) {
	e := newEnv(t)
	e.cloud.InstantBuild = true
	for e.queuedJobs() == 0 {
		require.NoError(t, e.ensure())
	}
	require.False(t, e.gatewayHolds(controlplane.GatewayAddress), "the gateway held its address when the install began")
	run := e.takeJobs()[0]
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
	}()

	require.NoError(t, e.converge())
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the install never finished once the gateway held its address")
	}

	installs := e.installed()
	require.Len(t, installs, 1, "installs")
	assert.True(t, installs[0].gatewayHeld, "the image server was created before the gateway held %s", controlplane.GatewayAddress)
	assert.NotEqual(t, controlplane.GatewayAddress, installs[0].imageServer, "the image server's address")
}

func TestSubscribe_RefusesBeforeAnyChange(t *testing.T) {
	cases := []struct {
		name  string
		opts  []envOption
		setup func(t *testing.T, e *env)
		want  string
	}{
		{name: "a server type the image cannot run on is refused", opts: []envOption{withParam("serverType", "cax21")}, want: `params["serverType"] is "cax21"`},
		{name: "a cluster that hosts the platform without a recipient key is refused", opts: []envOption{withParam("hostsPlatform", "true")}, want: paramRecipient},
		{
			name: "a build against a kit whose tools image is unreleased is refused",
			setup: func(t *testing.T, _ *env) {
				swap(t, &toolsImage, func() string {
					return "ghcr.io/mitstack-eu/talos-cluster-kit-tools:unreleased@sha256:0000000000000000000000000000000000000000000000000000000000000000"
				})
			},
			want: "tools-image-unreleased",
		},
		{name: "a subscribe naming no Robot server is refused", opts: []envOption{withParam(paramRobotServer, "")}, want: "refused:missing-param"},
		{name: "a Robot server number that is no number is refused", opts: []envOption{withParam(paramRobotServer, "ax42")}, want: "refused:invalid-param"},
		{name: "a connection without the Robot login is refused", setup: func(_ *testing.T, e *env) { e.dropRobotLogin = true }, want: "refused:missing-robot-access"},
		{name: "an operator without its egress address is refused", setup: func(t *testing.T, _ *env) { t.Setenv(egressEnv, "") }, want: "refused:egress-address-unset"},
		{name: "an egress address that is not IPv4 is refused", setup: func(t *testing.T, _ *env) { t.Setenv(egressEnv, "2001:db8::7") }, want: "refused:egress-address-unset"},
		{name: "a server Robot does not hold is refused", opts: []envOption{withParam(paramRobotServer, "2100999")}, want: "refused:server-not-found"},
		{
			name: "a cancelled server is refused",
			setup: func(_ *testing.T, e *env) {
				e.robot.EditServer(testRobotServer, func(s *robottest.Server, _ *robottest.State) { s.Cancelled = true })
			},
			want: "refused:server-cancelled",
		},
		{
			name: "a server outside the cluster's locations is refused",
			setup: func(_ *testing.T, e *env) {
				e.robot.EditServer(testRobotServer, func(s *robottest.Server, _ *robottest.State) { s.DC = "ASH-DC1" })
			},
			want: "refused:location-unsupported",
		},
		{
			name: "a server another subscription adopted is refused",
			setup: func(_ *testing.T, e *env) {
				e.robot.EditServer(testRobotServer, func(_ *robottest.Server, st *robottest.State) { st.Name = "mitstack-sub-another" })
			},
			want: "refused:server-taken",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts...)
			if tc.setup != nil {
				tc.setup(t, e)
			}

			err := e.ensure()

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Zero(t, e.count(), "Hetzner resources after the refusal")
			assert.Empty(t, e.robot.Writes(), "Robot writes before the refusal")
			assert.False(t, e.github.exists(e.repo()), "the repository after the refusal")
		})
	}
}

func TestSubscribe_RefusesAServerTypeTheBlockDoesNotListAsAnInvalidParam(t *testing.T) {
	cases := []struct{ name, serverType string }{
		{name: "an x86 type Hetzner sells that the block does not list is refused", serverType: "cpx42"},
		{name: "an Arm type the amd64 image cannot boot is refused", serverType: "cax21"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withParam("serverType", tc.serverType))

			err := e.ensure()

			var refused *handler.Refused
			require.ErrorAs(t, err, &refused)
			assert.Equal(t, "invalid-param", refused.Class, "serverType %q refused as %q; want invalid-param", tc.serverType, refused.Class)
			assert.Zero(t, e.count(), "Hetzner resources after the refusal")
		})
	}
}

func TestSubscribe_CreatesAndMarksTheClusterRepository(t *testing.T) {
	e := newEnv(t)

	e.subscribe()

	assert.ElementsMatch(t, []string{"mitstack-hcloud-hrobot-talos-cluster", subscriptionRepoTopic(testSub)}, e.github.topics(e.repo()))
	assert.Contains(t, e.github.files, e.repo()+"/contents/"+seedMarkerFile, "the seed")
}

func TestSubscribe_RefusesARepositoryWithoutTheSubscriptionsMark(t *testing.T) {
	e := newEnv(t)
	e.github.repos[e.repo()] = []string{"someone-elses"}

	err := e.ensure()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository-not-marked")
	assert.Zero(t, e.count(), "Hetzner resources after the refusal")
}

func TestSubscribe_AnInstallTheOperatorLostOrThatFailedIsRefusedAndObservedSo(t *testing.T) {
	cases := []struct {
		name         string
		lose         func(e *env)
		class, state string
	}{
		{name: "an install a restart of the operator lost", lose: func(e *env) { e.restart() }, class: "worker-install-lost", state: "has run since"},
		{name: "an install that failed", lose: func(e *env) { e.installErr = errors.New("the rescue system never answered"); e.runJobs() },
			class: "worker-install-failed", state: "failed at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.cloud.InstantBuild = true
			require.NoError(t, e.converge())
			require.True(t, e.gatewayHolds(controlplane.GatewayAddress), "the gateway's address")
			tc.lose(e)

			err := e.ensure()
			obs := e.observe()

			var refused *handler.Refused
			require.ErrorAs(t, err, &refused)
			assert.Equal(t, tc.class, refused.Class)
			assert.Contains(t, refused.Detail, "unsubscribe and subscribe again")
			assert.Equal(t, contract.Unknown, obs.State)
			assert.Contains(t, obs.StatusDetail, tc.state)
			assert.True(t, obs.Redrive, "an observe that a subscribe would refuse asks for the subscribe that names the refusal")
		})
	}
}

// finishDuringRead answers the cluster's network as Hetzner held it, then lets the queued install finish before the
// caller goes on, as an install that ends while a call reads the cluster does.
type finishDuringRead struct {
	controlplane.API
	once   sync.Once
	finish func()
}

func (f *finishDuringRead) ListNetworks(ctx context.Context, labelSelector string) ([]controlplane.Network, error) {
	networks, err := f.API.ListNetworks(ctx, labelSelector)
	f.once.Do(f.finish)
	return networks, err
}

func TestSubscribe_AnInstallThatFinishesWhileTheCallReadsTheClusterIsNotRefusedAsLost(t *testing.T) {
	e := newEnv(t)
	e.cloud.InstantBuild = true
	require.NoError(t, e.converge())
	require.Equal(t, 1, e.queuedJobs(), "the install waiting to run")
	require.Equal(t, workerRunning, e.network().Labels[keyWorkerInstall], "the worker's progress before the call")
	read := &finishDuringRead{API: e.cloud, finish: e.runJobs}
	swap(t, &newCloudAPI, func(string) controlplane.API { return read })

	err := e.ensure()

	require.NoError(t, err)
	assert.Equal(t, workerWritten, e.network().Labels[keyWorkerInstall], "the worker's progress after the call")
}

func TestSubscribe_PutsBackTheRobotResourceAWrittenWorkerLost(t *testing.T) {
	cases := []struct {
		name string
		lose func(e *env)
	}{
		{name: "the server's name", lose: func(e *env) {
			e.robot.EditServer(testRobotServer, func(_ *robottest.Server, st *robottest.State) { st.Name = "" })
		}},
		{name: "the server's place on the vSwitch", lose: func(e *env) {
			require.NoError(e.t, e.robot.Client().Uncouple(e.ctx(), e.subscriptionVSwitches()[0].ID, testRobotServer))
		}},
		{name: "the server's firewall", lose: func(e *env) {
			e.robot.SetFirewall(testRobotServer, robot.Firewall{Status: "active", Input: []robot.Rule{{Name: "allow all", Action: "accept"}}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			require.Equal(t, contract.Present, e.subscribe().State, "the subscribe")
			tc.lose(e)

			lost := e.observe()
			require.NoError(t, e.converge())

			assert.Equal(t, contract.Unknown, lost.State, "the cluster after the loss")
			assert.True(t, lost.Redrive, "the observe after the loss asks for a subscribe")
			assert.Equal(t, contract.Present, e.observe().State, "the cluster after the subscribe: %s", e.observe().StatusDetail)
			assert.Len(t, e.installed(), 1, "installs: the repair installed the worker again")
		})
	}
}

func TestSubscribe_RefusesAWrittenWorkerWhoseVSwitchIsCancelled(t *testing.T) {
	e := newEnv(t)
	require.Equal(t, contract.Present, e.subscribe().State, "the subscribe")
	id := e.subscriptionVSwitches()[0].ID
	require.NoError(t, e.robot.Client().Uncouple(e.ctx(), id, testRobotServer))
	require.NoError(t, e.robot.Client().CancelVSwitch(e.ctx(), id))

	err := e.ensure()

	var refused *handler.Refused
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, "vswitch-lost", refused.Class)
	assert.Empty(t, e.subscriptionVSwitches(), "a vSwitch the refusal made")
}

func TestSubscribe_IsUnknownUntilTheNewestNodeReportReadsEveryNodeAndTheWorkerReady(t *testing.T) {
	e := newEnv(t)
	e.cloud.InstantBuild = true
	require.NoError(t, e.converge())
	e.runJobs()
	require.Len(t, e.byRole(controlplane.RoleNode), 3, "nodes")
	e.reports.report(e.now.Add(-time.Minute), everyNode()...)
	notReady := everyNode()
	notReady[1].Ready = false

	silent := e.observe()
	e.now = e.now.Add(time.Minute)
	e.reports.report(e.now, notReady...)
	reportNotReady := e.observe()
	e.reports.report(e.now.Add(time.Second), everyNode()[:3]...)
	cloudOnly := e.observe()
	e.reports.report(e.now.Add(2*time.Second), append(everyNode()[:3], reportedNode{Name: "a-stranger", Ready: true})...)
	stranger := e.observe()
	e.reports.report(e.now.Add(3*time.Second), everyNode()...)
	ready := e.observe()

	assert.Equal(t, contract.Unknown, silent.State)
	assert.Contains(t, silent.StatusDetail, "no node report since the last node's creation")
	assert.Equal(t, contract.Unknown, reportNotReady.State)
	assert.Contains(t, reportNotReady.StatusDetail, "not Ready")
	assert.Equal(t, contract.Unknown, cloudOnly.State)
	assert.Contains(t, cloudOnly.StatusDetail, "names 3 nodes, and the cluster has 4 with its Robot worker")
	assert.Equal(t, contract.Unknown, stranger.State)
	assert.Contains(t, stranger.StatusDetail, "does not name the Robot worker "+testPlane().Name("worker"))
	assert.Equal(t, contract.Present, ready.State, "detail: %s", ready.StatusDetail)
}

func TestSubscribe_TheClusterCarriesTheEnrolmentDigestAndTheClusterRepositoryWhileItWaitsOnItsNodeReport(t *testing.T) {
	e := newEnv(t)
	e.quietNode = true
	p := e.engine()

	first := p.post("subscribe")
	p.settle(func() bool { return strings.HasPrefix(e.observe().StatusDetail, "the cluster has sent no node report") })
	waiting := p.post("subscribe")
	nodesWhileWaiting := len(e.byRole(controlplane.RoleNode))
	e.quietNode = false
	final := p.settle(nil)

	gateway := e.byRole(controlplane.RoleGateway)[0]
	require.NotEmpty(t, gateway.Labels[controlplane.KeyEnrolmentSHA256], "the gateway's enrolment digest label")
	want := map[string]string{
		"enrolmentTokenSha256": gateway.Labels[controlplane.KeyEnrolmentSHA256],
		"clusterRepoUrl":       "https://github.com/" + e.repo(),
	}
	assert.Nil(t, first.observed(resCluster), "the first subscribe, before the node exists, observed the cluster")
	require.True(t, waiting.pending, "a subscribe before the node reports: %s", waiting.detail)
	assert.Equal(t, 3, nodesWhileWaiting, "nodes while the cluster waits on their report")
	cluster := waiting.observed(resCluster)
	require.NotNil(t, cluster, "the subscribe after the node exists did not observe the cluster")
	assert.Equal(t, contract.Unknown, cluster.State, "the cluster's state")
	assert.Contains(t, cluster.StatusDetail, "no node report", "the cluster's detail")
	assert.Equal(t, want, cluster.Outputs, "the cluster's outputs while it waits on its node report")
	require.False(t, final.pending, "the last answer: %s", final.detail)
	for name, value := range want {
		assert.Equal(t, value, final.observed(resCluster).Outputs[name], "the cluster's output %s once it is present", name)
		assert.Equal(t, value, final.outputs[name], "the cluster's provided output %s", name)
	}
}

func TestSubscribe_TheClusterOutputsTheIssuerItsNodesSignTokensForInAnIssuerBucketOfItsOwn(t *testing.T) {
	e := newEnv(t)

	obs := e.subscribe()

	require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
	bucket := obs.Outputs["issuerBucket"]
	assert.Regexp(t, `^mitstack-oidc-[a-z2-7]{26}$`, bucket, "issuerBucket")
	assert.Equal(t, "https://"+bucket+".s3.eu-central-1.amazonaws.com/"+testSub, obs.Outputs["issuerUrl"], "issuerUrl")
	assert.Equal(t, obs.Outputs["issuerUrl"], e.installed()[0].installer.Cluster.ServiceAccountIssuer.String(), "the issuer the worker signs tokens for")
}

func TestObserve_AClusterWhoseGatewayCarriesNoIssuerBucketReadsUnknownAndSaysWhy(t *testing.T) {
	e := newEnv(t)
	require.Equal(t, contract.Present, e.subscribe().State, "the subscribe")
	delete(e.cloud.Servers[e.byRole(controlplane.RoleGateway)[0].ID].Labels, controlplane.KeyIssuerBucket)

	obs := e.observe()

	assert.Equal(t, contract.Unknown, obs.State, "detail: %s", obs.StatusDetail)
	assert.Contains(t, obs.StatusDetail, "no "+controlplane.KeyIssuerBucket+" label")
	assert.NotContains(t, obs.Outputs, "issuerUrl", "the outputs of a cluster whose gateway names no issuer bucket")
}

func TestObserve_AWaitingClusterWhoseRepositoryCannotBeNamedCarriesNeitherOutputAndSaysWhy(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.converge())
	require.Len(t, e.byRole(controlplane.RoleGateway), 1, "gateways")
	d := e.desired()
	delete(d.Secrets, "github")

	obs, err := observers()[resCluster].ObserveResource(e.ctx(), contract.ResourceRef{Desired: d})

	require.NoError(t, err)
	assert.Equal(t, contract.Unknown, obs.State, "detail: %s", obs.StatusDetail)
	assert.Empty(t, obs.Outputs, "the cluster's outputs")
	assert.Contains(t, obs.StatusDetail, "secrets.github.token is empty")
}

func TestObserve_AWaitingClusterWhoseGatewayLacksItsDigestLabelCarriesNeitherOutputAndSaysWhy(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.converge())
	delete(e.cloud.Servers[e.byRole(controlplane.RoleGateway)[0].ID].Labels, controlplane.KeyEnrolmentSHA256)

	obs, err := observers()[resCluster].ObserveResource(e.ctx(), contract.ResourceRef{Desired: e.desired()})

	require.NoError(t, err)
	assert.Equal(t, contract.Unknown, obs.State, "detail: %s", obs.StatusDetail)
	assert.Empty(t, obs.Outputs, "the cluster's outputs")
	assert.Contains(t, obs.StatusDetail, "no "+controlplane.KeyEnrolmentSHA256+" label")
}

func TestObserve_AWaitingClusterWithoutItsRepositoryCarriesNeitherOutput(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.converge())
	e.github.remove(e.repo())

	obs, err := observers()[resCluster].ObserveResource(e.ctx(), contract.ResourceRef{Desired: e.desired()})

	require.NoError(t, err)
	assert.Equal(t, contract.Unknown, obs.State, "detail: %s", obs.StatusDetail)
	assert.Empty(t, obs.Outputs, "the cluster's outputs")
}

func TestObserve_TheRobotResourcesAreReadFromRobotByTheSubscriptionsName(t *testing.T) {
	e := newEnv(t)
	before := e.observeRobot()
	require.Equal(t, contract.Present, e.subscribe().State, "the subscribe")

	present := e.observeRobot()
	require.NoError(t, e.unsubscribe(e.desired()))
	released := e.observeRobot()

	for _, name := range []string{resDedicatedServer, resVSwitch, resRobotFirewall} {
		assert.Equal(t, contract.Absent, before[name].State, "%s before the subscribe: %s", name, before[name].StatusDetail)
		assert.Equal(t, contract.Present, present[name].State, "%s once subscribed: %s", name, present[name].StatusDetail)
		assert.Equal(t, contract.Absent, released[name].State, "%s after the unsubscribe: %s", name, released[name].StatusDetail)
	}
}
