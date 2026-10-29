package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/block-kit/handler"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
)

// observers judge every declared resource by the labels, names and topics it carries, never by a recorded identity,
// so an unsubscribe of a subscription the engine never recorded still settles from what the providers hold.
func observers() map[string]handler.Observer {
	gw := controlplane.GatewayLocation
	out := map[string]handler.Observer{
		resNetwork:  labelledObserver{kind: "network", list: func(st controlplane.State) []int64 { return controlplane.IDs(st.Networks, networkID) }},
		resFirewall: labelledObserver{kind: "firewall", list: func(st controlplane.State) []int64 { return controlplane.IDs(st.Firewalls, firewallID) }},
		resGateway: labelledObserver{kind: "gateway", list: func(st controlplane.State) []int64 {
			return controlplane.IDs(st.Servers[controlplane.RoleGateway], controlplane.ServerID)
		}},
		located(resPrivateLB, gw): labelledObserver{kind: gw + " API load balancer", list: loadBalancersIn(controlplane.RoleAPI, gw)},
		located(resPublicLB, gw):  labelledObserver{kind: gw + " ingress load balancer", list: loadBalancersIn(controlplane.RoleIngress, gw)},
		resRepository:             repositoryObserver{},
		resSeedCommit:             seedCommitObserver{},
		resCluster:                clusterObserver{},
		resDedicatedServer:        robotObserver{judge: judgeDedicatedServer},
		resVSwitch:                robotObserver{judge: judgeVSwitch},
		resRobotFirewall:          robotObserver{judge: judgeRobotFirewall},
	}
	for _, loc := range controlplane.Locations {
		out[located(resNode, loc)] = labelledObserver{kind: loc + " node", list: func(st controlplane.State) []int64 {
			return controlplane.IDs(controlplane.ServersIn(st.Servers[controlplane.RoleNode], loc), controlplane.ServerID)
		}}
	}
	return out
}

func located(resource, loc string) string { return resource + "-" + loc }

func networkID(n controlplane.Network) int64 { return n.ID }

func firewallID(f controlplane.Firewall) int64 { return f.ID }

func loadBalancersIn(r controlplane.Role, loc string) func(controlplane.State) []int64 {
	return func(st controlplane.State) []int64 {
		return controlplane.IDs(controlplane.LoadBalancersIn(st.LoadBalancers[r], loc), controlplane.LoadBalancerID)
	}
}

// labelledObserver reads one kind: present when exactly one resource carries the labels, absent when none does.
type labelledObserver struct {
	kind string
	list func(controlplane.State) []int64
}

func (o labelledObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	p, refused, ok := hetznerReader(r.Desired)
	if !ok {
		return refused, nil
	}
	st, err := p.Read(ctx)
	if err != nil {
		return contract.Observation{}, err
	}
	switch ids := o.list(st); len(ids) {
	case 0:
		return contract.Observation{State: contract.Absent, StatusDetail: "no " + o.kind + " carries this subscription's labels"}, nil
	case 1:
		return contract.Observation{State: contract.Present, StatusDetail: fmt.Sprintf("%s %d carries this subscription's labels", o.kind, ids[0])}, nil
	default:
		return unknown(controlplane.DuplicatesDetail(o.kind, ids)), nil
	}
}

// robotObserver reads the worker's Robot resources, which carry the subscription in their name.
type robotObserver struct {
	judge func(t robotTarget, st robotState) contract.Observation
}

func (o robotObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	t, refused, ok := robotReader(r.Desired)
	if !ok {
		return refused, nil
	}
	st, err := t.read(ctx)
	switch {
	case robot.ErrorCode(err) == "SERVER_NOT_FOUND":
		return contract.Observation{State: contract.Absent, StatusDetail: fmt.Sprintf("Robot holds no dedicated server %d", t.server)}, nil
	case errors.Is(err, robot.ErrRateLimited):
		return unknown(robotLimitDetail), nil
	case err != nil:
		return contract.Observation{}, err
	}
	return o.judge(t, st), nil
}

func judgeDedicatedServer(t robotTarget, st robotState) contract.Observation {
	if st.server.Name != t.name {
		return contract.Observation{State: contract.Absent, StatusDetail: fmt.Sprintf("dedicated server %d is named %q, not %s", t.server, st.server.Name, t.name)}
	}
	return contract.Observation{State: contract.Present, StatusDetail: fmt.Sprintf("dedicated server %d in %s is named %s", t.server, st.server.DC, t.name)}
}

func judgeVSwitch(t robotTarget, st robotState) contract.Observation {
	switch {
	case st.vswitches == 0:
		return contract.Observation{State: contract.Absent, StatusDetail: "no uncancelled vSwitch is named " + t.name}
	case st.vswitches > 1:
		return unknown(fmt.Sprintf("%d vSwitches are named %s", st.vswitches, t.name))
	case !st.onSwitch:
		return unknown(fmt.Sprintf("vSwitch %d does not carry dedicated server %d", st.vswitch.ID, t.server))
	}
	return contract.Observation{State: contract.Present, StatusDetail: fmt.Sprintf("vSwitch %d on VLAN %d carries dedicated server %d", st.vswitch.ID, st.vswitch.VLAN, t.server)}
}

func judgeRobotFirewall(t robotTarget, st robotState) contract.Observation {
	switch {
	case st.server.Name != t.name:
		return contract.Observation{State: contract.Absent, StatusDetail: fmt.Sprintf("dedicated server %d is not named %s, so its firewall is not the cluster's", t.server, t.name)}
	case !holdsWorkerRules(st.firewall, t.firewall()):
		return unknown(fmt.Sprintf("the firewall of dedicated server %d is %s and does not admit the cluster's network", t.server, st.firewall.Status))
	}
	return contract.Observation{State: contract.Present, StatusDetail: fmt.Sprintf("the firewall of dedicated server %d is active and admits the cluster's network", t.server)}
}

// robotReader answers Unknown, before any call, for a request the worker's Robot resources cannot be read with.
func robotReader(d contract.Desired) (robotTarget, contract.Observation, bool) {
	rb, err := robotLogin(d)
	if err != nil {
		return robotTarget{}, unknown("the dedicated server cannot be read: " + refusalDetail(err)), false
	}
	number, err := robotServerNumber(d)
	if err != nil {
		return robotTarget{}, unknown("the dedicated server cannot be named: " + refusalDetail(err)), false
	}
	if !controlplane.ValidLabelValue(d.SubscriptionID) {
		return robotTarget{}, unknown("the subscription id is not a valid label value, so the worker's Robot names cannot be formed"), false
	}
	p := newPlane(nil, d.SubscriptionID, d.Params["appName"])
	return robotTarget{robot: rb, server: number, name: robotName(d.SubscriptionID), plane: p}, contract.Observation{}, true
}

// repositoryObserver reads the cluster repository from GitHub alone; one without the subscription's mark is not the cluster's.
type repositoryObserver struct{}

func (repositoryObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	repo, err := observeRepo(ctx, r.Desired)
	if err != nil {
		return contract.Observation{}, err
	}
	switch repo.state {
	case repoPresent:
		if hostsPlatform(r.Desired) {
			keys, err := repo.gh.listDeployKeys(ctx)
			if err != nil {
				return contract.Observation{}, err
			}
			if wrong := deployKeyProblems(repo.name, keys); len(wrong) > 0 {
				return contract.Observation{State: contract.Absent, StatusDetail: strings.Join(wrong, "; ")}, nil
			}
		}
		return contract.Observation{State: contract.Present, Outputs: map[string]string{"repo": repo.name}, StatusDetail: repo.describe()}, nil
	case repoGone, repoUnmarked:
		return contract.Observation{State: contract.Absent, StatusDetail: repo.describe()}, nil
	default:
		return unknown(repo.describe()), nil
	}
}

// seedCommitObserver reads the seed present while the marked cluster repository holds it.
type seedCommitObserver struct{}

func (seedCommitObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	repo, err := observeRepo(ctx, r.Desired)
	if err != nil {
		return contract.Observation{}, err
	}
	switch repo.state {
	case repoGone, repoUnmarked:
		return contract.Observation{State: contract.Absent, StatusDetail: repo.describe()}, nil
	case repoUnknown:
		return unknown(repo.describe()), nil
	}
	held, err := repo.gh.holdsSeed(ctx)
	if err != nil {
		return contract.Observation{}, err
	}
	if !held {
		return contract.Observation{State: contract.Absent, StatusDetail: "the cluster repository " + repo.name + " holds no " + seedMarkerFile}, nil
	}
	return contract.Observation{State: contract.Present, StatusDetail: "the cluster repository " + repo.name + " holds the seed"}, nil
}

type clusterObserver struct{}

func (clusterObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	return observeCluster(ctx, r.Desired)
}

// hetznerReader answers Unknown, before any call, for a request the subscription's Cloud resources cannot be read with.
func hetznerReader(d contract.Desired) (controlplane.Plane, contract.Observation, bool) {
	token := d.Secrets["hetzner"]["api_token"]
	if token == "" {
		return controlplane.Plane{}, unknown("no Hetzner token on this call (secrets.hetzner.api_token is empty), so the cluster cannot be read"), false
	}
	if !controlplane.ValidLabelValue(d.SubscriptionID) {
		return controlplane.Plane{}, unknown("the subscription id is not a valid Hetzner label value, so the cluster's resources cannot be found by it"), false
	}
	return newPlane(newCloudAPI(token), d.SubscriptionID, d.Params["appName"]), contract.Observation{}, true
}

// Observe reads the whole cluster: present once the control plane stands as the block makes it, the worker is written
// and the newest node report since the last node's creation reads every node Ready, the worker among them; absent when
// no labelled resource or marked repository is left; unknown, naming what is missing, in between.
func (r *blockResource) Observe(ctx context.Context, d contract.Desired) (contract.Observation, error) {
	return observeCluster(ctx, d)
}

func observeCluster(ctx context.Context, d contract.Desired) (contract.Observation, error) {
	p, refused, ok := hetznerReader(d)
	if !ok {
		return refused, nil
	}
	st, installing, err := readCluster(ctx, p, d.SubscriptionID)
	if err != nil {
		return contract.Observation{}, err
	}
	repo, err := observeRepo(ctx, d)
	if err != nil {
		return contract.Observation{}, err
	}
	if st.Empty() && (repo.state == repoGone || repo.state == repoUnmarked) {
		return contract.Observation{
			State:        contract.Absent,
			StatusDetail: "no server, load balancer, firewall, network or snapshot carries this subscription's labels, and " + repo.describe(),
		}, nil
	}
	problems := p.Problems(ctx, st)
	if repo.state != repoPresent {
		problems = append(problems, repo.describe())
	}
	if network, ok := st.Network(); ok && workerProgress(network) != workerWritten {
		problems = append(problems, workerDetail(network))
	}
	if len(problems) == 0 {
		if problem := reportProblem(ctx, d, st.Servers[controlplane.RoleNode], p.Name("worker")); problem != "" {
			problems = append(problems, problem)
		}
	}
	var recovery map[string]string
	if len(problems) == 0 && hostsPlatform(d) {
		var gap string
		if recovery, gap, err = repo.gh.hostingOutputs(ctx); err != nil {
			return contract.Observation{}, err
		}
		if gap != "" {
			problems = append(problems, gap)
		}
	}
	redrive, err := stepLeft(ctx, d, st, installing, repo)
	if errors.Is(err, robot.ErrRateLimited) {
		problems = append(problems, robotLimitDetail)
	} else if err != nil {
		return contract.Observation{}, err
	}
	if len(problems) > 0 || redrive {
		outputs, gap := enrolment(st, repo)
		if gap != "" {
			problems = append(problems, gap)
		}
		if len(problems) == 0 {
			problems = append(problems, "a Robot resource of the worker needs putting back")
		}
		obs := unknown(strings.Join(problems, "; "))
		obs.Outputs, obs.Redrive = outputs, redrive
		return obs, nil
	}
	outputs, detail := p.Outputs(st)
	outputs["clusterRepoUrl"] = repo.url
	maps.Copy(outputs, recovery)
	return contract.Observation{
		State:        contract.Present,
		Outputs:      outputs,
		StatusDetail: detail + ", the Robot worker is written, and the newest node report reads every node Ready",
	}, nil
}

func workerDetail(network controlplane.Network) string {
	switch workerProgress(network) {
	case workerRunning:
		return "the Robot worker's install has run since " + workerProgressAt(network)
	case workerFailed:
		return "the Robot worker's install failed at " + workerProgressAt(network)
	}
	return "the Robot worker's install has not begun"
}

// stepLeft reports whether a subscribe sent now would take a step or refuse, read from what Hetzner, Robot and GitHub
// hold, so the engine re-drives subscribe once the step in flight has finished, and never while it runs. A request the
// subscribe refuses before reading the cluster asks for none, since its own subscribe was refused already.
func stepLeft(ctx context.Context, d contract.Desired, st controlplane.State, installing bool, repo repoObservation) (bool, error) {
	req, err := readEnsureRequest(d)
	if err != nil || repo.state == repoUnknown {
		return false, nil
	}
	c := convergence{plane: req.plane(newCloudAPI(req.token)), state: st, installing: installing, req: req, repo: repoRead{exists: repo.state != repoGone, marked: repo.state == repoPresent}}
	network, ok := st.Network()
	var refused *handler.Refused
	if err := c.refusal(ctx); errors.Is(err, robot.ErrRateLimited) {
		return false, err
	} else if errors.As(err, &refused) || !ok || !c.repo.exists {
		return true, nil
	} else if err != nil {
		return false, err
	}
	_, ok, err = c.nextStep(ctx, network)
	if errors.As(err, &refused) {
		return true, nil
	}
	return ok, err
}

// enrolment gives a cluster not yet present the values a node's enrolment is judged against, once the gateway carries
// its digest and the marked repository exists, so the platform holds them before any node can report.
func enrolment(st controlplane.State, repo repoObservation) (map[string]string, string) {
	digest, gap := st.EnrolmentDigest()
	if digest == "" || repo.state != repoPresent {
		return nil, gap
	}
	return map[string]string{"enrolmentTokenSha256": digest, "clusterRepoUrl": repo.url}, ""
}

type repoState int

const (
	repoUnknown repoState = iota
	repoPresent
	repoUnmarked
	repoGone
)

// repoObservation is what reading the cluster repository found; detail says why its state is unknown.
type repoObservation struct {
	state     repoState
	name, url string
	detail    string
	gh        *githubAccess
}

func (o repoObservation) describe() string {
	switch o.state {
	case repoPresent:
		return "the cluster repository " + o.name + " exists"
	case repoUnmarked:
		return "the repository " + o.name + " does not carry this subscription's mark, so it is not the cluster's"
	case repoGone:
		return "the cluster repository " + o.name + " does not exist"
	default:
		return o.detail
	}
}

// observeRepo leaves the repository unknown, with no GitHub call, when the request cannot name it or reach GitHub.
func observeRepo(ctx context.Context, d contract.Desired) (repoObservation, error) {
	appName := d.Params["appName"]
	if appName == "" {
		return repoObservation{name: "(unnamed)", detail: `the cluster repository cannot be named: params["appName"] is empty`}, nil
	}
	obs := repoObservation{name: repoFor(d.Secrets["github"]["owner"], appName).fullName()}
	gh, err := dialGitHub(d, appName)
	if err != nil {
		obs.detail = "the cluster repository cannot be read: " + refusalDetail(err)
		return obs, nil
	}
	obs.name, obs.url, obs.gh = gh.repo.fullName(), gh.repo.url(), gh
	read, err := gh.readRepo(ctx, d.SubscriptionID)
	if err != nil {
		return obs, err
	}
	switch {
	case !read.exists:
		obs.state = repoGone
	case !read.marked:
		obs.state = repoUnmarked
	default:
		obs.state = repoPresent
	}
	return obs, nil
}

func unknown(detail string) contract.Observation {
	return contract.Observation{State: contract.Unknown, StatusDetail: detail}
}

// refusalDetail drops the refused:<class> prefix, which a validate answer does not carry.
func refusalDetail(err error) string {
	var refused *handler.Refused
	if errors.As(err, &refused) {
		return refused.Detail
	}
	return err.Error()
}
