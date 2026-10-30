package main

// A cluster that hosts the platform gets its repository's deploy keys from the install, sealed to the keys it mints,
// and hands those keys out only as PGP messages to the owner's recipient key.

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
	"golang.org/x/crypto/ssh"
)

const fakeInitFile = "OpenBao root token:\n"

func hostingCluster(e *env) {
	_, recipient := testutil.Recipient(e.t)
	e.params[paramHostsPlatform], e.params[paramRecipient] = "true", string(recipient)
}

func (e *env) repoFile(name string) (string, bool) {
	return e.github.file(e.repo() + "/contents/" + name)
}

// initCommitted plays OpenBao's init Job, which commits the init file once the cluster runs.
func (e *env) initCommitted() string {
	e.t.Helper()
	_, recipient := testutil.Recipient(e.t)
	message, err := encryptTo(recipient, []byte("s.root-token-for-tests"))
	require.NoError(e.t, err)
	e.github.writeFile(e.repo()+"/contents/"+openbaoInitFile, fakeInitFile+message)
	return fakeInitFile + message
}

// hostingSubscribe settles a hosting subscription, with the init Job committing its file once every node exists.
func (e *env) hostingSubscribe() contract.Observation {
	e.t.Helper()
	e.engine().settle(func() bool {
		return len(e.byRole(controlplane.RoleNode)) == len(controlplane.Locations) && len(e.reports.reports) > 0
	})
	e.initCommitted()
	return e.subscribe()
}

type cloudFile struct {
	Path, Content, Encoding string
}

// zippedFile is the gzipped file a cloud-config writes at a path ending in suffix, unzipped.
func zippedFile(t *testing.T, userData, suffix string) []byte {
	t.Helper()
	var config struct {
		WriteFiles []cloudFile `yaml:"write_files"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(strings.TrimPrefix(userData, "#cloud-config\n")), &config))
	for _, f := range config.WriteFiles {
		if strings.HasSuffix(f.Path, suffix) {
			zipped, err := base64.StdEncoding.DecodeString(f.Content)
			require.NoError(t, err)
			r, err := gzip.NewReader(bytes.NewReader(zipped))
			require.NoError(t, err)
			plain, err := io.ReadAll(r)
			require.NoError(t, err)
			return plain
		}
	}
	require.FailNow(t, "the user data writes no file ending in "+suffix)
	return nil
}

// servedHosting is the hosting set the serving server holds, as configuration documents.
func servedHosting(t *testing.T, e *env) []map[string]any {
	t.Helper()
	return testutil.Documents(t, zippedFile(t, e.cloud.SpecFor(t, controlplane.RoleServing).UserData, "/hosting.yaml"))
}

func TestSubscribe_ArgoCDOnAHostingClusterPostsItsVerdictsToThePlatformWithTheEnrolmentsToken(t *testing.T) {
	e := newEnv(t, hostingCluster)

	obs := e.hostingSubscribe()

	require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
	var writable []any
	for _, doc := range testutil.Kind(testutil.Documents(t, zippedFile(t, e.cloud.SpecFor(t, controlplane.RoleBuilder).UserData, "/config.yaml")), "KubeInlineManifestConfig") {
		if testutil.ManifestName(doc["name"].(string)) != "mitstack-cluster-tokens" {
			continue
		}
		for _, role := range testutil.Kind(testutil.Documents(t, []byte(doc["manifest"].(string))), "Role") {
			if testutil.Field(t, role, "metadata", "namespace") != "argocd" {
				continue
			}
			for _, rule := range testutil.Field(t, role, "rules").([]any) {
				names, _ := rule.(map[string]any)["resourceNames"].([]any)
				writable = append(writable, names...)
			}
		}
	}
	assert.Equal(t, []any{"argocd-notifications-secret"}, writable, "the Secrets the enrolment may write in argocd")
	argocd := testutil.Manifest(t, "argocd", components.Default().Served)
	assert.Contains(t, e.cloud.SpecFor(t, controlplane.RoleGateway).UserData, argocd.TalosName()+".yaml "+argocd.SHA256()+" ", "the Argo CD the gateway serves")
	var service struct {
		URL string `yaml:"url"`
	}
	for _, cm := range testutil.Kind(testutil.Documents(t, argocd.Bytes), "ConfigMap") {
		if testutil.Field(t, cm, "metadata", "name") == "argocd-notifications-cm" {
			webhook, _ := testutil.Field(t, cm, "data", "service.webhook.mitstack").(string)
			require.NoError(t, yaml.Unmarshal([]byte(webhook), &service))
		}
	}
	assert.Equal(t, "$platform-url/v1/gitops-notifications", service.URL, "where the served Argo CD posts its verdicts")
}

// openedDeployKeys opens the hosting set's two SealedSecrets with the Sealed Secrets key the recipient decrypts.
func openedDeployKeys(t *testing.T, e *env) map[string]string {
	t.Helper()
	sealedKey, ok := e.repoFile(sealedSecretsKeyFile)
	require.True(t, ok, "the repository holds no %s", sealedSecretsKeyFile)
	keyPEM := testutil.RecipientDecryptions(t, sealedKey)
	require.Len(t, keyPEM, 1)
	opened := map[string]string{}
	for _, doc := range testutil.Kind(servedHosting(t, e), "KubeInlineManifestConfig") {
		for _, manifest := range testutil.Documents(t, []byte(doc["manifest"].(string))) {
			secret := testutil.Unseal(t, []byte(keyPEM[0]), manifest)
			for item, value := range secret.Data {
				opened[secret.Namespace+"/"+secret.Name+"/"+item] = value
			}
		}
	}
	return opened
}

func authorizedKey(t *testing.T, privatePEM string) string {
	t.Helper()
	signer, err := ssh.ParsePrivateKey([]byte(privatePEM))
	require.NoError(t, err)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

func TestSubscribe_HostsThePlatformWithAWriteAndAReadOnlyDeployKeyTheInstallCreatedAndSealed(t *testing.T) {
	e := newEnv(t, hostingCluster)

	obs := e.hostingSubscribe()

	require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
	keys := e.github.deployKeys(e.repo())
	require.Len(t, keys, 2, "deploy keys on the cluster repository")
	opened := openedDeployKeys(t, e)
	write := opened[components.DeployKeyNamespace+"/"+components.DeployKeySecret+"/"+components.DeployKeyItem]
	read := opened[components.RepositoryCredentialNamespace+"/"+components.RepositoryCredentialName+"/"+components.RepositoryCredentialItem]
	assert.Equal(t, []deployKey{
		{ID: keys[0].ID, Title: writeKeyTitle, Key: authorizedKey(t, write), ReadOnly: false},
		{ID: keys[1].ID, Title: readKeyTitle, Key: authorizedKey(t, read), ReadOnly: true},
	}, keys, "each deploy key's public half on GitHub, beside the private half the cluster opens")
	installs := e.installed()
	require.Len(t, installs, 1, "installs")
	assert.True(t, installs[0].installer.Cluster.AppsOnWorkers, "Traefik and the apps run on the worker, and only OpenBao's members on the control plane")
	nodes := []string{testPlane().RoleSelector(controlplane.RoleNode)}
	ingress := e.cloud.OnlyLoadBalancer(t, testPlane().RoleSelector(controlplane.RoleIngress))
	assert.Equal(t, []string{"10.0.1.2"}, ingress.TargetIPs, "the ingress load balancer's address targets")
	assert.Empty(t, ingress.TargetSelectors, "the ingress load balancer's label targets")
	assert.Equal(t, nodes, e.cloud.OnlyLoadBalancer(t, testPlane().RoleSelector(controlplane.RoleAPI)).TargetSelectors,
		"the API load balancer's label targets")
	for _, name := range []string{sealKeyFile, sealedSecretsKeyFile, sealedSecretsCert} {
		_, ok := e.repoFile(name)
		assert.True(t, ok, "the install committed no %s", name)
	}
}

func TestSubscribe_AHostingInstallReplacesTheDeployKeysAnEarlierInstallLeft(t *testing.T) {
	e := newEnv(t, hostingCluster)
	require.NoError(t, e.ensure())
	e.github.addDeployKey(e.repo(), deployKey{Title: writeKeyTitle, Key: "ssh-ed25519 AAAA stale"})
	e.github.addDeployKey(e.repo(), deployKey{Title: "the owner's own key", Key: "ssh-ed25519 AAAA owner", ReadOnly: true})

	require.NoError(t, e.converge())

	var titles []string
	for _, k := range e.github.deployKeys(e.repo()) {
		titles = append(titles, k.Title)
		assert.NotEqual(t, "ssh-ed25519 AAAA stale", k.Key, "the stale key outlived the install")
	}
	assert.ElementsMatch(t, []string{"the owner's own key", writeKeyTitle, readKeyTitle}, titles)
}

func TestObserve_AHostingClusterOutputsItsKeysOnlyAsPGPMessagesToTheRecipient(t *testing.T) {
	e := newEnv(t, hostingCluster)

	obs := e.hostingSubscribe()

	require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
	initFile, _ := e.repoFile(openbaoInitFile)
	assert.Equal(t, initFile, obs.Outputs["openbaoInit"])
	certificate, _ := e.repoFile(sealedSecretsCert)
	assert.Equal(t, certificate, obs.Outputs["sealedSecretsCertificate"])
	assert.Contains(t, certificate, "-----BEGIN CERTIFICATE-----")
	sealKey := testutil.RecipientDecryptions(t, obs.Outputs["openbaoSealKey"])
	require.Len(t, sealKey, 1, "openbaoSealKey holds one PGP message")
	sealedSecretsKey := testutil.RecipientDecryptions(t, obs.Outputs["sealedSecretsKey"])
	require.Len(t, sealedSecretsKey, 1, "sealedSecretsKey holds one PGP message")
	assert.Contains(t, sealedSecretsKey[0], "PRIVATE KEY", "sealedSecretsKey opens to the Sealed Secrets key")
	var served string
	for _, doc := range testutil.Kind(servedHosting(t, e), "KubeInlineManifestConfig") {
		if testutil.ManifestName(doc["name"].(string)) == "mitstack-openbao-seal-key" {
			served = doc["manifest"].(string)
		}
	}
	assert.Contains(t, served, "key: "+sealKey[0], "openbaoSealKey opens to the seal key the cluster unseals with")
	for name, value := range obs.Outputs {
		assert.NotContains(t, value, sealKey[0], "the output %s holds the plain seal key", name)
		assert.NotContains(t, value, sealedSecretsKey[0], "the output %s holds the plain Sealed Secrets key", name)
	}
}

func TestObserve_AHostingClusterIsUnknownUntilOpenBaosInitFileIsCommitted(t *testing.T) {
	e := newEnv(t, hostingCluster)
	e.engine().settle(func() bool {
		return len(e.byRole(controlplane.RoleNode)) == len(controlplane.Locations) && len(e.reports.reports) > 0
	})
	e.progress()

	obs := e.observe()

	assert.Equal(t, contract.Unknown, obs.State)
	assert.Contains(t, obs.StatusDetail, "holds no "+openbaoInitFile+" yet")
}

func TestNoSecretLeak_TheDeployKeysPrivateHalvesAppearOnlySealed(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	e := newEnv(t, hostingCluster)

	obs := e.hostingSubscribe()

	require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
	opened := openedDeployKeys(t, e)
	privates := map[string]string{}
	for _, item := range []string{
		components.DeployKeyNamespace + "/" + components.DeployKeySecret + "/" + components.DeployKeyItem,
		components.RepositoryCredentialNamespace + "/" + components.RepositoryCredentialName + "/" + components.RepositoryCredentialItem,
	} {
		require.Contains(t, opened[item], "OPENSSH PRIVATE KEY", "the cluster opens no private key at %s", item)
		privates[item] = opened[item]
	}
	var seen []string
	for _, a := range e.engine().posted {
		seen = append(seen, a.detail, strings.Join(mapValues(a.outputs), "\n"))
		for _, r := range a.resources {
			if r.Observe != nil {
				seen = append(seen, r.Observe.StatusDetail, strings.Join(mapValues(r.Observe.Outputs), "\n"))
			}
		}
	}
	seen = append(seen, logs.String(), obs.StatusDetail, strings.Join(mapValues(obs.Outputs), "\n"))
	seen = append(seen, e.cloud.SpecFor(t, controlplane.RoleGateway).UserData, e.cloud.SpecFor(t, controlplane.RoleBuilder).UserData)
	e.github.mu.Lock()
	seen = append(seen, mapValues(e.github.files)...)
	e.github.mu.Unlock()
	for item, private := range privates {
		body := strings.Join(strings.Split(private, "\n")[1:4], "\n")
		for _, s := range seen {
			assert.NotContains(t, s, body, "the private half of %s leaked", item)
		}
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestSubscribe_RefusesAHostingClusterWithoutARecipientItCanEncryptTo(t *testing.T) {
	for name, recipient := range map[string]string{"none": "", "not a key": "not a key", "a key that cannot encrypt": string(testutil.SignOnlyRecipient(t))} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, hostingCluster, withParam(paramRecipient, recipient))

			err := e.ensure()

			require.Error(t, err)
			assert.Contains(t, err.Error(), paramRecipient)
			assert.Zero(t, e.changes(), "a refused request changed something")
		})
	}
}

func TestObserve_AHostingClustersRepositoryIsPresentOnlyWithBothDeployKeysAtTheirAccess(t *testing.T) {
	observe := func(e *env) contract.Observation {
		obs, err := repositoryObserver{}.ObserveResource(e.ctx(), contract.ResourceRef{Desired: e.desired()})
		require.NoError(t, err)
		return obs
	}
	cases := []struct {
		name   string
		keys   []deployKey
		state  contract.State
		detail string
	}{
		{name: "no key", state: contract.Absent, detail: writeKeyTitle},
		{name: "only the write key", keys: []deployKey{{Title: writeKeyTitle}}, state: contract.Absent, detail: readKeyTitle},
		{name: "a read-only write key", keys: []deployKey{{Title: writeKeyTitle, ReadOnly: true}, {Title: readKeyTitle, ReadOnly: true}}, state: contract.Absent, detail: "read-only"},
		{name: "a read key that can write", keys: []deployKey{{Title: writeKeyTitle}, {Title: readKeyTitle}}, state: contract.Absent, detail: readKeyTitle},
		{name: "both keys at their access", keys: []deployKey{{Title: writeKeyTitle}, {Title: readKeyTitle, ReadOnly: true}}, state: contract.Present},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, hostingCluster)
			e.github.repos[e.repo()] = repositoryMark(testSub)
			for _, k := range c.keys {
				k.Key = "ssh-ed25519 AAAA " + k.Title
				e.github.addDeployKey(e.repo(), k)
			}

			obs := observe(e)

			assert.Equal(t, c.state, obs.State, obs.StatusDetail)
			assert.Contains(t, obs.StatusDetail, c.detail)
		})
	}
	t.Run("a cluster that hosts no platform", func(t *testing.T) {
		e := newEnv(t)
		e.github.repos[e.repo()] = repositoryMark(testSub)

		assert.Equal(t, contract.Present, observe(e).State, "a repository without deploy keys")
	})
}
