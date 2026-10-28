package main

// The shared fakes: Hetzner Cloud and Robot in memory, GitHub over HTTP, the platform's node reports, the worker's
// install, and the platform's own settle check sending signed requests to the block's handler.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/github/ghapi"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/block-kit/helpers/events"
	"github.com/mitstack-eu/block-kit/observe"
	"github.com/mitstack-eu/block-kit/verify/kittest"
	"github.com/mitstack-eu/block-kit/webhook"
	"github.com/mitstack-eu/hcloud-hrobot-talos-cluster/robotworker"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/stretchr/testify/require"
)

const (
	testSub         = "sub_0123456789ab"
	testApp         = "shop"
	testOwner       = "acme"
	testHetznerKey  = "hetzner-token-for-tests"
	testGitHubToken = "github-token-for-tests"
	testOrigin      = "https://api.mitstack.test"
	testRobotServer = 2100001
	testEgress      = "198.51.100.7"
)

var testStart = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// fakeReports plays the platform's node-reports read.
type fakeReports struct {
	reports []observe.NodeReport
}

func (f *fakeReports) NodeReports(context.Context, contract.Desired, string) ([]observe.NodeReport, error) {
	return f.reports, nil
}

// report adds a node report received at at, naming each node with its readiness.
func (f *fakeReports) report(at time.Time, nodes ...reportedNode) {
	body, _ := json.Marshal(map[string]any{"subscriptionId": testSub, "nodes": nodes})
	f.reports = append(f.reports, observe.NodeReport{ReceivedAt: at, Body: body})
}

// everyNode is the three Cloud nodes and the Robot worker, all Ready.
func everyNode() []reportedNode {
	return []reportedNode{{Name: "node-fsn1", Ready: true}, {Name: "node-nbg1", Ready: true}, {Name: "node-hel1", Ready: true},
		{Name: testPlane().Name("worker"), Ready: true}}
}

func testPlane() controlplane.Plane { return newPlane(nil, testSub, testApp) }

// installCall is one install the block started, with what Hetzner held when it created the image server.
type installCall struct {
	installer robotworker.Installer
	worker    robotworker.Worker
	// gatewayHeld is whether the gateway held its address when the image server was created, and imageServer the
	// image server's address.
	gatewayHeld bool
	imageServer string
}

// env is one block under test with every fake it calls, and the clock its verbs read.
type env struct {
	t       *testing.T
	block   *blockResource
	cloud   *cloudtest.Fake
	robot   *robottest.Fake
	github  *fakeGitHub
	reports *fakeReports
	now     time.Time
	params  map[string]string
	// buildTakes and imageTakes are how long the builder and the snapshot run once started; a zero build never ends.
	buildTakes, imageTakes time.Duration
	// quietNode holds back the nodes' report, so the cluster waits on it.
	quietNode bool
	eng       *engine

	mu sync.Mutex
	// queued holds each install the block started and the harness has not yet run.
	queued   []func()
	installs []installCall
	// installErr, when set, fails every install after its image server is deleted.
	installErr error
	// dropRobotLogin leaves the Robot login out of the hetzner connection.
	dropRobotLogin bool
}

type envOption func(*env)

func withParam(key, value string) envOption {
	return func(e *env) { e.params[key] = value }
}

func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	e := &env{
		t: t, now: testStart, reports: &fakeReports{}, block: &blockResource{}, buildTakes: 10 * time.Minute, imageTakes: 3 * time.Minute,
		params: map[string]string{"appName": testApp, paramRobotServer: fmt.Sprint(testRobotServer)},
	}
	e.cloud = cloudtest.New(func() time.Time { return e.now })
	e.robot = robottest.New(t)
	e.robot.AddServer(robottest.Server{Number: testRobotServer, IP: "203.0.113.50", DC: "HEL1-DC2"})
	e.github = newFakeGitHub(t)
	for _, opt := range opts {
		opt(e)
	}
	t.Setenv(egressEnv, testEgress)
	swap(t, &clock, func() time.Time { return e.now })
	swap(t, &newCloudAPI, func(string) controlplane.API { return e.cloud })
	swap(t, &newRobotClient, func(string, string) *robot.Client { return e.robot.Client() })
	swap(t, &newGitHubClient, func(_, token string) *ghapi.Client {
		return ghapi.NewClient(e.github.server.URL, token, e.github.server.Client())
	})
	var reports observe.NodeReportsAPI = e.reports
	swap(t, &nodeReports, reports)
	swap(t, &installWorker, e.install)
	swap(t, &startJob, func(run func()) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.queued = append(e.queued, run)
	})
	swap(t, &imageServerPoll, time.Millisecond)
	t.Cleanup(e.stopJobs)
	return e
}

func swap[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

// install does to Robot and Cloud what the rescue install does around the image write, which robotworker's own tests
// cover.
func (e *env) install(ctx context.Context, i robotworker.Installer, w robotworker.Worker) error {
	call := installCall{installer: i, worker: w}
	defer func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.installs = append(e.installs, call)
	}()
	server, err := i.Robot.Adopt(ctx, w.Server, w.Name)
	if err != nil {
		return err
	}
	if _, err := i.Robot.Couple(ctx, w.VSwitch, w.Server); err != nil {
		return err
	}
	if err := i.Robot.WriteFirewall(ctx, w.Server, robotworker.Firewall(i.Cluster.Network, w)); err != nil {
		return err
	}
	id, err := i.Cloud.CreateImageServer(ctx, robotworker.ImageServer{Name: w.Name + "-image", Location: server.Location(), Image: robotworker.ImageServerImage})
	if err != nil {
		return err
	}
	call.gatewayHeld = e.gatewayHolds(controlplane.GatewayAddress)
	addr, err := i.Cloud.ImageServerAddress(ctx, id)
	if err != nil {
		return err
	}
	call.imageServer = addr.String()
	if err := i.Cloud.DeleteImageServer(ctx, id); err != nil {
		return err
	}
	return e.installErr
}

func (e *env) gatewayHolds(addr string) bool {
	gateways := e.byRole(controlplane.RoleGateway)
	networks, err := e.cloud.ListNetworks(context.Background(), testPlane().Selector())
	return err == nil && len(gateways) == 1 && len(networks) == 1 && gateways[0].PrivateIPs[networks[0].ID] == addr
}

// runJobs runs each queued install once the gateway holds its address, as the install's own wait would.
func (e *env) runJobs() {
	if !e.gatewayHolds(controlplane.GatewayAddress) {
		return
	}
	for _, run := range e.takeJobs() {
		run()
	}
}

// releaseJobs starts each queued install on its own, so an unsubscribe that stops it finds it running.
func (e *env) releaseJobs() {
	for _, run := range e.takeJobs() {
		go run()
	}
}

// stopJobs stops every install the test left, so none outlives it.
func (e *env) stopJobs() {
	e.releaseJobs()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(e.t, stopJob(ctx, testSub), "an install the test left")
}

func (e *env) queuedJobs() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.queued)
}

func (e *env) takeJobs() []func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	queued := e.queued
	e.queued = nil
	return queued
}

func (e *env) installed() []installCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.installs)
}

// restart drops the installs this process runs, as a restart of the operator would.
func (e *env) restart() {
	e.takeJobs()
	jobs.Lock()
	delete(jobs.running, testSub)
	jobs.Unlock()
}

func (e *env) byRole(r controlplane.Role) []controlplane.Server {
	return e.cloud.Selected(e.t, testPlane().RoleSelector(r))
}

func (e *env) network() controlplane.Network {
	return e.cloud.OnlyNetwork(e.t, testPlane().Selector())
}

// subscriptionVSwitches are the uncancelled vSwitches named for the subscription.
func (e *env) subscriptionVSwitches() []robot.VSwitch {
	return slices.DeleteFunc(e.robot.VSwitches(), func(v robot.VSwitch) bool { return v.Name != robotName(testSub) || v.Cancelled })
}

// count is every Cloud resource under the subscription's labels and every Robot resource under its name.
func (e *env) count() int {
	n := e.cloud.Count() + len(e.subscriptionVSwitches())
	if e.robot.State(testRobotServer).Name == robotName(testSub) {
		n++
	}
	return n
}

func (e *env) desired() contract.Desired {
	d := contract.Desired{
		Identity: contract.Identity{SubscriptionID: testSub},
		Params:   maps.Clone(e.params),
		Secrets: map[string]map[string]string{
			"hetzner": {"api_token": testHetznerKey, "robot_user": robottest.User, "robot_password": robottest.Password},
			"github":  {"token": testGitHubToken, "owner": testOwner},
		},
	}
	if e.dropRobotLogin {
		d.Secrets["hetzner"] = map[string]string{"api_token": testHetznerKey}
	}
	return d
}

// observeRobot is what each Robot resource's observer reads.
func (e *env) observeRobot() map[string]contract.Observation {
	e.t.Helper()
	out := map[string]contract.Observation{}
	for _, name := range []string{resDedicatedServer, resVSwitch, resRobotFirewall} {
		obs, err := observers()[name].ObserveResource(e.ctx(), contract.ResourceRef{Desired: e.desired()})
		require.NoError(e.t, err, name)
		out[name] = obs
	}
	return out
}

func (e *env) ctx() context.Context {
	return events.WithEnvelope(context.Background(), events.Envelope{
		SubscriptionID: testSub,
		Callback:       &events.Callback{URL: testOrigin + operatorEventsPath, Token: "callback-token-for-tests"},
	})
}

func (e *env) ensure() error {
	return e.block.Ensure(e.ctx(), e.desired())
}

func (e *env) unsubscribe(d contract.Desired) error {
	e.releaseJobs()
	_, err := e.block.Delete(e.ctx(), d)
	return err
}

// changes counts every call that changed Hetzner Cloud, Robot or GitHub.
func (e *env) changes() int {
	e.github.mu.Lock()
	defer e.github.mu.Unlock()
	return e.cloud.Writes() + len(e.robot.Writes()) + e.github.writes
}

// converge sends Ensure until one changes nothing, as the engine's re-drives would, and returns the first error.
func (e *env) converge() error {
	e.t.Helper()
	for range 50 {
		before := e.changes()
		if err := e.ensure(); err != nil || e.changes() == before {
			return err
		}
	}
	e.t.Fatal("Ensure still changed the cluster after 50 calls")
	return nil
}

func (e *env) observe() contract.Observation {
	e.t.Helper()
	obs, err := e.block.Observe(e.ctx(), e.desired())
	require.NoError(e.t, err)
	return obs
}

// progress plays what Hetzner, the builder, the install and the nodes do on their own while the platform waits: the
// cluster reports every node Ready once all three Cloud nodes exist and the worker is written.
func (e *env) progress() {
	e.cloud.Advance(e.buildTakes, e.imageTakes)
	e.runJobs()
	written := len(e.installed()) > 0 && e.installErr == nil
	if !e.quietNode && written && len(e.byRole(controlplane.RoleNode)) == len(controlplane.Locations) && len(e.reports.reports) == 0 {
		e.reports.report(e.now, everyNode()...)
	}
}

// building reports whether the builder runs its build, the long step the engine's re-drive resumes after.
func (e *env) building() bool {
	builders := e.byRole(controlplane.RoleBuilder)
	return len(builders) == 1 && builders[0].Status == controlplane.ServerRunning
}

// buildingBuilder settles a subscription until its builder runs a build that never ends.
func (e *env) buildingBuilder() *engine {
	e.t.Helper()
	e.buildTakes = 0
	p := e.engine()
	p.settle(func() bool { return e.building() })
	require.True(e.t, e.building(), "the builder never started its build")
	return p
}

const mib = 1 << 20

// subscribe settles the subscription as the platform does and returns what Observe then reads.
func (e *env) subscribe() contract.Observation {
	e.t.Helper()
	final := e.engine().settle(nil)
	require.Equal(e.t, http.StatusOK, final.status, "the settle's last answer: %s", final.detail)
	return e.observe()
}

func (e *env) repo() string {
	return testOwner + "/" + testApp + "-cluster"
}

// settleBudget is the manifest's spec.lifecycle.settleBudget.
const settleBudget = 75 * time.Minute

// fakeGitHub serves the repository, topics and contents calls the block makes, from memory.
type fakeGitHub struct {
	mu     sync.Mutex
	repos  map[string][]string
	files  map[string]string
	server *httptest.Server
	writes int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	gh := &fakeGitHub{repos: map[string][]string{}, files: map[string]string{}}
	gh.server = httptest.NewServer(http.HandlerFunc(gh.serve))
	t.Cleanup(gh.server.Close)
	return gh
}

func (gh *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if r.Method != http.MethodGet {
		gh.writes++
	}
	path := strings.TrimPrefix(r.URL.Path, "/repos/")
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/orgs/"):
		var body struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gh.repos[strings.Split(r.URL.Path, "/")[2]+"/"+body.Name] = []string{}
		w.WriteHeader(http.StatusCreated)
	case strings.HasSuffix(path, "/topics"):
		gh.serveTopics(w, r, strings.TrimSuffix(path, "/topics"))
	case strings.Contains(path, "/contents/"):
		gh.serveContents(w, r, path)
	case r.Method == http.MethodDelete:
		delete(gh.repos, path)
		for file := range gh.files {
			if strings.HasPrefix(file, path+"/") {
				delete(gh.files, file)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		if _, ok := gh.repos[path]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"full_name":"` + path + `"}`))
	}
}

func (gh *fakeGitHub) serveTopics(w http.ResponseWriter, r *http.Request, repo string) {
	if _, ok := gh.repos[repo]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method == http.MethodPut {
		var body struct{ Names []string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gh.repos[repo] = body.Names
	}
	_ = json.NewEncoder(w).Encode(map[string][]string{"names": gh.repos[repo]})
}

func (gh *fakeGitHub) serveContents(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method == http.MethodPut {
		var body struct{ Content string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		decoded, _ := base64.StdEncoding.DecodeString(body.Content)
		gh.files[path] = string(decoded)
		w.WriteHeader(http.StatusCreated)
		return
	}
	content, ok := gh.files[path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(content))})
}

func (gh *fakeGitHub) topics(repo string) []string {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return gh.repos[repo]
}

// remove deletes the repository with its files, as an owner deleting it on GitHub would.
func (gh *fakeGitHub) remove(repo string) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	delete(gh.repos, repo)
	for path := range gh.files {
		if strings.HasPrefix(path, repo+"/") {
			delete(gh.files, path)
		}
	}
}

func (gh *fakeGitHub) removeFile(path string) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	delete(gh.files, path)
}

func (gh *fakeGitHub) exists(repo string) bool {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	_, ok := gh.repos[repo]
	return ok
}

// engine plays the platform's lifecycle engine against the block's own handler, as signed requests.
type engine struct {
	e      *env
	h      http.Handler
	signer *kittest.Signer
	seq    int
	sleeps int
	posted []answer
}

// answer is what one request to the handler answered, with how many Hetzner actions it waited on.
type answer struct {
	op               string
	status           int
	pending, redrive bool
	detail           string
	waits            int
	outputs          map[string]string
	resources        map[string]contract.ResourceOutcome
}

// observed is what the answer's observe of the named resource returned, or nil when the call never reached it.
func (a answer) observed(name string) *contract.CallOutcome {
	return a.resources[name].Observe
}

func (a answer) err() error {
	if a.status >= http.StatusBadRequest {
		return fmt.Errorf("%s answered %d: %s", a.op, a.status, a.detail)
	}
	return nil
}

func (e *env) engine() *engine {
	if e.eng != nil {
		return e.eng
	}
	p := &engine{e: e, signer: kittest.NewSigner(e.t, "kid-hcloud-hrobot-talos-cluster", 41)}
	cfg := handlerConfig(p.signer.JWKSServer(e.t), nil)
	cfg.Resource = e.block
	cfg.Sleep = func(context.Context, time.Duration) { p.sleeps++ }
	h, err := handler.New(cfg)
	require.NoError(e.t, err)
	p.h, e.eng = h, p
	return p
}

func (p *engine) post(operation string) answer {
	p.e.t.Helper()
	if operation == "unsubscribe" {
		p.e.releaseJobs()
	}
	p.seq++
	body := hookBody(operation, p.e)
	obo, err := webhook.MintOBO(p.signer.Private, p.signer.Kid,
		webhook.OBOClaims{Audience: blockID, OrgID: body["orgId"].(string), AppID: body["appId"].(string), SubscriptionID: testSub}, time.Hour, time.Now())
	require.NoError(p.e.t, err)
	body["obo"], body["idempotencyKey"] = obo, fmt.Sprintf("%s-%d", operation, p.seq)
	waits := p.e.cloud.Waits()
	w := httptest.NewRecorder()
	p.h.ServeHTTP(w, p.signer.HookRequest(p.e.t, "/hook", kittest.Envelope(fmt.Sprintf("nonce-%d", p.seq), time.Now(), body)))
	var doc struct {
		Pending, Redrive    bool
		StatusDetail, Error string
		Outputs             map[string]string
		Resources           map[string]contract.ResourceOutcome
	}
	require.NoError(p.e.t, json.Unmarshal(w.Body.Bytes(), &doc), "the %s answer", operation)
	a := answer{op: operation, status: w.Code, pending: doc.Pending, redrive: doc.Redrive, detail: doc.StatusDetail + doc.Error, waits: p.e.cloud.Waits() - waits,
		outputs: doc.Outputs, resources: doc.Resources}
	p.posted = append(p.posted, a)
	return a
}

// settle sends one subscribe, then the settle check's validates, parked 15 seconds doubling to two minutes, and a
// re-driven subscribe whenever validate answers redrive. It returns the answer that settled the subscription, or the
// last subscribe's once stop reads true.
func (p *engine) settle(stop func() bool) answer {
	last := p.post("subscribe")
	for park, began := 15*time.Second, p.e.now; last.pending && last.err() == nil && p.e.now.Sub(began) < settleBudget; {
		if stop != nil && stop() {
			return last
		}
		p.e.now = p.e.now.Add(park)
		park = min(2*park, 2*time.Minute)
		p.e.progress()
		v := p.post("validate")
		if !v.pending || v.err() != nil {
			return v
		}
		if v.redrive {
			last, park = p.post("subscribe"), 15*time.Second
		}
	}
	return last
}
