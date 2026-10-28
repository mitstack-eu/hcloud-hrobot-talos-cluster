package main

// Unsubscribe deletes every Cloud resource the block made, the snapshot that holds every key of the cluster among them,
// cancels the vSwitch and releases the Robot server without cancelling it.

import (
	"testing"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnsubscribeDeletes_EveryResourceItMadeTheSnapshotIncludedAndReleasesTheRobotServer(t *testing.T) {
	e := newEnv(t)
	require.Equal(t, contract.Present, e.subscribe().State)
	vswitch := e.subscriptionVSwitches()[0].ID

	err := e.unsubscribe(e.desired())

	require.NoError(t, err)
	assert.Zero(t, e.count(), "Hetzner resources under the subscription's labels or name after unsubscribe")
	assert.False(t, e.github.exists(e.repo()), "the cluster repository after unsubscribe")
	assert.Equal(t, contract.Absent, e.observe().State)
	assert.Empty(t, e.robot.State(testRobotServer).Name, "the released server's name")
	for _, v := range e.robot.VSwitches() {
		if v.ID == vswitch {
			assert.True(t, v.Cancelled, "the subscription's vSwitch is cancelled")
			assert.Empty(t, v.Servers, "the cancelled vSwitch's servers")
		}
	}
	for _, w := range e.robot.Writes() {
		assert.NotContains(t, w.Path, "/cancellation", "a Robot write cancelled the server")
	}
}

func TestUnsubscribe_MidBuildStopsTheInstallAndDeletesEverything(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.converge())
	require.Equal(t, 1, e.queuedJobs(), "the worker's install in flight")

	err := e.unsubscribe(e.desired())

	require.NoError(t, err)
	assert.Zero(t, e.count(), "Hetzner resources after an unsubscribe during the build")
	assert.False(t, jobRunning(testSub), "the install outlived the unsubscribe")
}

func TestUnsubscribe_KeepsARepositoryWithoutTheSubscriptionsMark(t *testing.T) {
	e := newEnv(t)
	e.subscribe()
	e.github.repos[e.repo()] = []string{"someone-elses"}

	err := e.unsubscribe(e.desired())

	require.NoError(t, err)
	assert.True(t, e.github.exists(e.repo()), "an unmarked repository is kept")
	assert.Zero(t, e.count(), "Hetzner resources")
}

func TestUnsubscribe_WithoutAConnectionDeletesWhatItCanReachAndRefusesNamingWhatItLeft(t *testing.T) {
	cases := []struct {
		name, connection, class string
		left                    int
	}{
		{name: "without GitHub", connection: "github", class: "repository-left-in-place"},
		{name: "without the Robot login", connection: "robot", class: "robot-left-in-place", left: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.subscribe()
			d := e.desired()
			if tc.connection == "github" {
				d.Secrets["github"] = map[string]string{}
			} else {
				d.Secrets["hetzner"] = map[string]string{"api_token": testHetznerKey}
			}

			err := e.unsubscribe(d)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.class)
			assert.Zero(t, e.cloud.Count(), "Hetzner Cloud resources")
			assert.Equal(t, tc.left, e.count(), "the Robot server and vSwitch left in place")
		})
	}
}
