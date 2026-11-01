package main

// An upgrade gives a running hosting cluster the release's hosting policies through its repository, and settles once
// the node report reads each one back.

import (
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/rollout"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ssmPolicy = testutil.ObjectRef{Group: "cilium.io", Kind: "CiliumNetworkPolicy", Namespace: "external-secrets", Name: "mitstack-external-secrets-ssm"}

// builtBeforeTheRoute is a running hosting cluster whose repository holds no hosting policies and whose Argo CD runs
// no Application for them, as a release before the route left it.
func builtBeforeTheRoute(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, hostingCluster)
	require.Equal(t, contract.Present, e.hostingSubscribe().State, "the cluster before its upgrade")
	e.github.removeFile(e.repo() + "/contents/" + rollout.PoliciesPath)
	e.github.removeFile(e.repo() + "/contents/" + rollout.ApplicationPath)
	e.argo = testutil.BuildCluster(t, components.HostingPolicies()[0].Bytes)
	return e
}

func releaseFiles(t *testing.T, e *env) []rollout.File {
	t.Helper()
	files, err := rollout.Current().Files(e.repo())
	require.NoError(t, err)
	return files
}

func TestUpgrade_GivesAHostingClusterBuiltBeforeTheRouteTheReleasesPoliciesAndSettlesOnceArgoCDAppliedEach(t *testing.T) {
	e := builtBeforeTheRoute(t)
	p := e.engine()
	puts := len(e.github.puts)

	upgrade := p.post("upgrade")
	e.progress()
	settled := p.post("validate")

	require.Equal(t, http.StatusOK, upgrade.status, "the upgrade: %s", upgrade.detail)
	assert.True(t, upgrade.pending, "the upgrade answered settled before Argo CD applied the policies: %s", upgrade.detail)
	var committed []string
	for _, f := range releaseFiles(t, e) {
		content, ok := e.repoFile(f.Path)
		assert.True(t, ok, "the repository holds no %s", f.Path)
		assert.Equal(t, string(f.Bytes), content, "the repository's %s", f.Path)
		committed = append(committed, e.repo()+"/contents/"+f.Path)
	}
	assert.Equal(t, committed, e.github.puts[puts:], "what the upgrade wrote, in order")
	require.Equal(t, http.StatusOK, settled.status, "the settle check: %s", settled.detail)
	assert.False(t, settled.pending, "the settle check once the node report read each policy back: %s", settled.detail)
	assert.NotNil(t, e.argo.Object(ssmPolicy), "the SSM policy the cluster's API serves")
	assert.Contains(t, e.observe().StatusDetail, rollout.Application, "what the cluster's observe read back")
}

func TestUpgrade_ReadsTheClusterUnknownUntilTheNodeReportReadsTheReleaseBack(t *testing.T) {
	e := builtBeforeTheRoute(t)
	e.argoBehind = true

	e.engine().post("upgrade")
	e.progress()
	obs := e.observe()

	assert.Equal(t, contract.Unknown, obs.State, "detail: %s", obs.StatusDetail)
	assert.Contains(t, obs.StatusDetail, "lists no Argo CD Application "+rollout.Application, "why the cluster is not present")
}

func TestUpgrade_ChangesNoCloudOrRobotResourceOfARunningCluster(t *testing.T) {
	e := builtBeforeTheRoute(t)
	p := e.engine()
	cloud, robotWrites, installs := e.cloud.Resources(), len(e.robot.Writes()), len(e.installed())
	cloudWrites := e.cloud.Writes()

	p.post("upgrade")
	e.progress()
	p.post("validate")

	assert.True(t, maps.Equal(cloud, e.cloud.Resources()), "the Cloud resources the upgrade left")
	assert.Equal(t, cloudWrites, e.cloud.Writes(), "Cloud writes the upgrade made")
	assert.Equal(t, robotWrites, len(e.robot.Writes()), "Robot writes the upgrade made")
	assert.Equal(t, installs, len(e.installed()), "worker installs the upgrade started")
}

func TestValidate_AHostingClusterBuiltBeforeTheRouteReadsPresentAndAsksForNoRedrive(t *testing.T) {
	e := builtBeforeTheRoute(t)

	v := e.engine().post("validate")

	require.Equal(t, http.StatusOK, v.status, "the validate: %s", v.detail)
	assert.False(t, v.pending, "the validate of a cluster whose repository holds no hosting policies: %s", v.detail)
	assert.False(t, v.redrive, "a redrive would roll the policies out without an upgrade")
	_, ok := e.repoFile(rollout.ApplicationPath)
	assert.False(t, ok, "a validate committed the hosting Application")
}

func TestValidate_AHostingClusterHoldingAnEarlierReleasesPoliciesReadsPresentUntilItsUpgrade(t *testing.T) {
	e := newEnv(t, hostingCluster)
	e.hostingSubscribe()
	_, ok := e.repoFile(rollout.ApplicationPath)
	require.True(t, ok, "a hosting subscribe committed no hosting Application")
	e.github.writeFile(e.repo()+"/contents/"+rollout.PoliciesPath, string(components.HostingPolicies()[0].Bytes))
	e.progress()

	v := e.engine().post("validate")

	assert.False(t, v.pending, "the validate of a cluster Argo CD holds an earlier release on: %s", v.detail)
	assert.False(t, v.redrive, "a redrive would roll the release out without an upgrade")
}

func TestUpgrade_AnUpToDateHostingClusterSettlesAtOnceAndWritesNoFile(t *testing.T) {
	e := newEnv(t, hostingCluster)
	e.hostingSubscribe()
	writes := e.github.writes

	upgrade := e.engine().post("upgrade")

	assert.False(t, upgrade.pending, "the upgrade of a cluster already on the release: %s", upgrade.detail)
	assert.Equal(t, writes, e.github.writes, "GitHub writes the upgrade made")
	for _, f := range releaseFiles(t, e) {
		_, ok := e.repoFile(f.Path)
		assert.True(t, ok, "a hosting subscribe committed no %s", f.Path)
	}
}

func TestSubscribe_AClusterThatHostsNoPlatformGetsNoHostingPolicies(t *testing.T) {
	e := newEnv(t)

	require.Equal(t, contract.Present, e.subscribe().State)

	for _, path := range []string{rollout.PoliciesPath, rollout.ApplicationPath} {
		assert.False(t, slices.Contains(e.github.puts, e.repo()+"/contents/"+path), "the repository took %s", path)
	}
}
