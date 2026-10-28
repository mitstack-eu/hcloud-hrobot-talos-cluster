package main

import (
	"cmp"
	"context"
	"errors"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

// blockResource keeps no state between calls but the worker installs this process runs, and finds every resource by
// its labels or its Robot name, so each Ensure takes the cluster on from wherever an earlier call left it.
type blockResource struct{}

var _ handler.ResourceReporter = (*blockResource)(nil)

// clock is a variable so a test can move the time a builder's deadline is judged against.
var clock = time.Now

// newCloudAPI and newRobotClient are variables so a test can stand in Hetzner's fakes.
var (
	newCloudAPI    = func(token string) controlplane.API { return controlplane.NewAPI(token, blockID) }
	newRobotClient = robot.New
)

// toolsImage is a variable so a test can stand in the placeholder an unreleased kit carries.
var toolsImage = func() string { return tools.Image }

const (
	resNetwork          = "hetzner.network"
	resFirewall         = "hetzner.firewall"
	resPrivateLB        = "api-load-balancer"
	resPublicLB         = "ingress-load-balancer"
	resRepository       = "github.repository"
	resSeedCommit       = "github.gitops-commit"
	resGateway          = "gateway"
	resNode             = "node"
	resCluster          = "hetzner.kubernetes-cluster"
	resDedicatedServer  = "dedicated-server"
	resVSwitch          = "vswitch"
	resRobotFirewall    = "robot-firewall"
	typeServer          = "hetzner.server"
	typeLoadBalancer    = "hetzner.load-balancer"
	typeRepository      = "github.gitops-repository"
	typeDedicatedServer = "hetzner.dedicated-server"
	typeVSwitch         = "hetzner.vswitch"
	typeRobotFirewall   = "hetzner.robot-firewall"
	paramHostsPlatform  = "hostsPlatform"
	paramRobotServer    = "robotServerNumber"
	// platformURL is where every node enrols and reports, as the manifest's endpoint names the platform.
	platformURL      = "https://api.mitstack.dev"
	userDataBoundary = "mitstack-hcloud-hrobot-talos-cluster"
	// unreleasedToolsTag is the tag of the zero-digest placeholder tools.Image carries between the kit's releases.
	unreleasedToolsTag = ":unreleased@"
	// egressEnv names the operator's public IPv4 address, the one address the rescue session's firewall window admits.
	egressEnv = "MITSTACK_EGRESS_IPV4"
)

type clusterParams struct {
	appName, serverType string
	robotServer         int64
	// hosting is whether the cluster hosts the platform, and recipient the key its recovery files are encrypted to.
	hosting   bool
	recipient []byte
}

func hostsPlatform(d contract.Desired) bool { return d.Params[paramHostsPlatform] == "true" }

func readParams(d contract.Desired) (clusterParams, error) {
	p := clusterParams{appName: d.Params["appName"], serverType: cmp.Or(d.Params["serverType"], controlplane.DefaultServerType)}
	if p.appName == "" {
		return p, handler.Refuse("missing-param", `the app name is required: params["appName"] is empty`)
	}
	if !slices.Contains(controlplane.ServerTypes, p.serverType) {
		return p, handler.Refuse("invalid-param", `params["serverType"] is %q; the cluster runs on %s`,
			p.serverType, strings.Join(controlplane.ServerTypes, ", "))
	}
	if p.hosting = hostsPlatform(d); p.hosting {
		var err error
		if p.recipient, err = readRecipient(d.Params[paramRecipient]); err != nil {
			return p, err
		}
	}
	number, err := robotServerNumber(d)
	p.robotServer = number
	return p, err
}

func robotServerNumber(d contract.Desired) (int64, error) {
	raw := d.Params[paramRobotServer]
	if raw == "" {
		return 0, handler.Refuse("missing-param",
			`the dedicated server's number is required: params["%s"] is empty, and the cluster's worker runs on that Robot server`, paramRobotServer)
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, handler.Refuse("invalid-param", `params["%s"] is %q, and a Robot server number is a positive whole number`, paramRobotServer, raw)
	}
	return n, nil
}

func hetznerToken(d contract.Desired) (string, error) {
	token := d.Secrets["hetzner"]["api_token"]
	if token == "" {
		return "", handler.Refuse("missing-hetzner-access",
			"the Hetzner connection is missing: secrets.hetzner.api_token is empty, so the cluster cannot be reached; connect a Hetzner Cloud project token for this app and retry")
	}
	return token, nil
}

func robotLogin(d contract.Desired) (*robot.Client, error) {
	user, password := d.Secrets["hetzner"]["robot_user"], d.Secrets["hetzner"]["robot_password"]
	if user == "" || password == "" {
		return nil, handler.Refuse("missing-robot-access",
			"the Hetzner connection holds no Robot login: secrets.hetzner.robot_user or robot_password is empty, so the dedicated server cannot be adopted; add the Robot webservice login to the connection and retry")
	}
	return newRobotClient(user, password), nil
}

func subscriptionID(d contract.Desired) (string, error) {
	if !controlplane.ValidLabelValue(d.SubscriptionID) {
		return "", handler.Refuse("invalid-subscription",
			"the subscription id is not a valid Hetzner label value, so the cluster's resources cannot be labelled or found by it")
	}
	return d.SubscriptionID, nil
}

func egressAddress() (netip.Addr, error) {
	addr, err := netip.ParseAddr(os.Getenv(egressEnv))
	if err != nil || !addr.Is4() {
		return netip.Addr{}, handler.Refuse("egress-address-unset",
			"the operator's %s holds no IPv4 address, and the dedicated server's firewall admits the rescue session from that address alone, so nothing was changed; the operator's host must set it", egressEnv)
	}
	return addr, nil
}

type ensureRequest struct {
	token, subID string
	params       clusterParams
	gh           *githubAccess
	robot        *robot.Client
	egress       netip.Addr
	secrets      []string
}

func readEnsureRequest(d contract.Desired) (ensureRequest, error) {
	req := ensureRequest{secrets: requestSecrets(d)}
	var err error
	if req.token, err = hetznerToken(d); err != nil {
		return req, err
	}
	if req.subID, err = subscriptionID(d); err != nil {
		return req, err
	}
	if req.params, err = readParams(d); err != nil {
		return req, err
	}
	if strings.Contains(toolsImage(), unreleasedToolsTag) {
		return req, handler.Refuse("tools-image-unreleased",
			"the block is built against a talos-cluster-kit whose tools image %s is a placeholder, so the node report could not run and nothing was changed; build it against a released kit", toolsImage())
	}
	if req.robot, err = robotLogin(d); err != nil {
		return req, err
	}
	if req.egress, err = egressAddress(); err != nil {
		return req, err
	}
	req.gh, err = dialGitHub(d, req.params.appName)
	return req, err
}

// plane installs the Robot worker from the cluster the control plane's install renders, in the one call its keys exist.
func (req ensureRequest) plane(api controlplane.API) controlplane.Plane {
	p := newPlane(api, req.subID, req.params.appName)
	p.ServerType, p.RepoURL = req.params.serverType, req.gh.repo.url()
	if req.params.hosting {
		// OpenBao's members keep their data on the control-plane nodes, and neither they nor Traefik tolerate a taint.
		p.Hosting, p.AppsOnWorkers = req.hosting(), false
	}
	p.Installed = func(ctx context.Context, network controlplane.Network, cluster nodeconfig.Cluster) error {
		return startWorkerInstall(ctx, req, p, network, cluster)
	}
	return p
}

type convergence struct {
	plane controlplane.Plane
	state controlplane.State
	// installing is whether this process ran the subscription's install while state was read.
	installing bool
	req        ensureRequest
	repo       repoRead
}

// Ensure takes one step that waits on Hetzner, so each call ends well inside the platform's two-minute verb timeout, and
// refuses before any change a cluster it cannot act on unambiguously, a repository without the subscription's mark, or
// a dedicated server it may not adopt. The network and the repository need no wait, so they come first.
func (r *blockResource) Ensure(ctx context.Context, d contract.Desired) error {
	return waitOutRobotLimit(ensure(ctx, d))
}

func ensure(ctx context.Context, d contract.Desired) error {
	req, err := readEnsureRequest(d)
	if err != nil {
		return err
	}
	c, err := readConvergence(ctx, req.plane(newCloudAPI(req.token)), req)
	if err != nil {
		return err
	}
	network, err := c.plane.EnsureNetwork(ctx, c.state)
	if err != nil {
		return err
	}
	if err := ensureRepository(ctx, c); err != nil {
		return err
	}
	s, ok, err := c.nextStep(ctx, network)
	if err != nil || !ok {
		return err
	}
	return refusal(s.Take(ctx, blockID, req.subID))
}

func readConvergence(ctx context.Context, plane controlplane.Plane, req ensureRequest) (convergence, error) {
	st, installing, err := readCluster(ctx, plane, req.subID)
	if err != nil {
		return convergence{}, err
	}
	repo, err := req.gh.readRepo(ctx, req.subID)
	if err != nil {
		return convergence{}, err
	}
	c := convergence{plane: plane, state: st, installing: installing, req: req, repo: repo}
	return c, c.refusal(ctx)
}

func (c convergence) refusal(ctx context.Context) error {
	if err := c.plane.Refusal(c.state); err != nil {
		return refusal(err)
	}
	if c.repo.exists && !c.repo.marked {
		return handler.Refuse("repository-not-marked",
			"the repository %s exists but does not carry this subscription's mark, the topics %s, so nothing was changed. Add both topics if it is this cluster's repository, or move it out of the way, and retry",
			c.req.gh.repo.fullName(), strings.Join(repositoryMark(c.req.subID), " and "))
	}
	return c.workerRefusal(ctx)
}

// nextStep is the control plane's next step, and once the worker is written, the step that puts back a Robot
// resource it lost.
func (c convergence) nextStep(ctx context.Context, network controlplane.Network) (controlplane.Step, bool, error) {
	s, ok, err := c.plane.NextStep(ctx, c.state, network)
	if err != nil || ok || workerProgress(network) != workerWritten {
		return s, ok, refusal(err)
	}
	return c.robotTarget().repairStep(ctx)
}

// refusal answers the plane's refusal as the block framework's, keeping its class.
func refusal(err error) error {
	var r *controlplane.Refusal
	if errors.As(err, &r) {
		return handler.Refuse(r.Class, "%s", r.Message)
	}
	return err
}

// ensureRepository creates and marks the cluster repository only when none exists, since Ensure has already
// refused an unmarked one, and seeds whatever of the seed is missing.
func ensureRepository(ctx context.Context, c convergence) error {
	if !c.repo.exists {
		if err := c.req.gh.createRepo(ctx); err != nil {
			return err
		}
		if err := c.req.gh.markRepo(ctx, c.req.subID); err != nil {
			return err
		}
	}
	return c.req.gh.seedRepo(ctx, seedFiles)
}
