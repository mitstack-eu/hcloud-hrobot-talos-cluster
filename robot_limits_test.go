package main

// A settle stays inside Robot's hourly request limits, and a rate-limited Robot read waits instead of failing the build.

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot"
	"github.com/mitstack-eu/block-kit/capabilities/hetzner/robot/robottest"
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// redriveEvery is how often the platform re-drives a settling subscribe.
const redriveEvery = 9 * time.Second

// robotLimits are the hourly limits Robot's documentation gives the reads the block makes, at
// https://robot.hetzner.com/doc/webservice/en.html; the server's is the block's own budget, under Robot's 200.
var robotLimits = map[string]int{"server": 120, "vswitch list": 500, "vswitch": 500, "firewall": 500}

// robotReads counts the block's Robot reads by the limit each falls under.
func robotReads(e *env) map[string]int {
	out := map[string]int{
		"server":       e.robot.Calls(http.MethodGet, fmt.Sprintf("/server/%d", testRobotServer)),
		"vswitch list": e.robot.Calls(http.MethodGet, "/vswitch"),
		"firewall":     e.robot.Calls(http.MethodGet, fmt.Sprintf("/firewall/%d", testRobotServer)),
	}
	for _, v := range e.robot.VSwitches() {
		out["vswitch"] += e.robot.Calls(http.MethodGet, fmt.Sprintf("/vswitch/%d", v.ID))
	}
	return out
}

func TestSubscribe_ASettleReDrivenEveryNineSecondsStaysInsideRobotsHourlyRequestLimits(t *testing.T) {
	e := newEnv(t)
	swap(t, &newRobotClient, func(user, password string) *robot.Client {
		c := robot.NewAt(e.robot.URL, user, password)
		c.PollInterval, c.Now = time.Millisecond, func() time.Time { return e.now }
		return c
	})
	p := e.engine()
	perHour := int(time.Hour / redriveEvery)
	var tallies []map[string]int

	for start := e.now; e.now.Sub(start) < settleBudget; e.now = e.now.Add(redriveEvery) {
		e.progress()
		require.NoError(t, p.post("subscribe").err(), "the subscribe at %s", e.now.Format(time.Kitchen))
		require.NoError(t, p.post("validate").err(), "the validate at %s", e.now.Format(time.Kitchen))
		tallies = append(tallies, robotReads(e))
	}

	require.Equal(t, contract.Present, e.observe().State, "the cluster after the settle: %s", e.observe().StatusDetail)
	require.Positive(t, tallies[len(tallies)-1]["server"], "reads of the dedicated server")
	for read, limit := range robotLimits {
		most := 0
		for i, tally := range tallies {
			inHour := tally[read]
			if i >= perHour {
				inHour -= tallies[i-perHour][read]
			}
			most = max(most, inHour)
		}
		assert.LessOrEqual(t, most, limit, "the most %s reads in any hour of the settle", read)
	}
}

func TestSubscribe_ARateLimitedRobotReadAnswersProgressAndWritesNothingToRobot(t *testing.T) {
	cases := []struct {
		name string
		// before brings the subscription to where Robot's limit is reached.
		before func(e *env)
	}{
		{name: "before the worker's install", before: func(*env) {}},
		{name: "after the worker is written and has lost its name", before: func(e *env) {
			require.Equal(e.t, contract.Present, e.subscribe().State, "the subscribe")
			e.robot.EditServer(testRobotServer, func(_ *robottest.Server, st *robottest.State) { st.Name = "" })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tc.before(e)
			writes := len(e.robot.Writes())
			e.robot.SetRateLimited(true)

			sub := e.engine().post("subscribe")
			val := e.engine().post("validate")
			robotObs := e.observeRobot()

			assert.Equal(t, http.StatusOK, sub.status, "the subscribe: %s", sub.detail)
			assert.True(t, sub.pending, "the subscribe answered settled: %s", sub.detail)
			assert.Contains(t, sub.detail, "request limit", "the subscribe's detail")
			assert.Equal(t, http.StatusOK, val.status, "the validate: %s", val.detail)
			assert.False(t, val.redrive, "the validate asked for a subscribe while Robot's limit holds")
			for name, obs := range robotObs {
				assert.Equal(t, contract.Unknown, obs.State, "the %s observe", name)
				assert.Contains(t, obs.StatusDetail, "request limit", "the %s observe's detail", name)
			}
			assert.Len(t, e.robot.Writes(), writes, "Robot writes while its limit holds")
		})
	}
}

func TestSubscribe_ARateLimitedReadAfterTheStepAnswersProgressAndWritesNothingToRobot(t *testing.T) {
	e := newEnv(t)
	limited := robottest.New(t)
	limited.SetRateLimited(true)
	logins := 0
	swap(t, &newRobotClient, func(string, string) *robot.Client {
		if logins++; logins == 1 {
			return e.robot.Client()
		}
		return limited.Client()
	})

	sub := e.engine().post("subscribe")

	require.Equal(t, 2, logins, "the Robot logins the subscribe made: the step's, then the read of what it converged")
	assert.Equal(t, http.StatusOK, sub.status, "the subscribe: %s", sub.detail)
	assert.True(t, sub.pending, "the subscribe answered settled: %s", sub.detail)
	assert.Contains(t, sub.detail, "request limit", "the subscribe's detail")
	assert.Empty(t, e.robot.Writes(), "Robot writes")
	assert.Empty(t, limited.Writes(), "Robot writes while its limit holds")
}

func TestObserve_TheClusterIsUnknownNamingRobotsRequestLimitWhileItHolds(t *testing.T) {
	e := newEnv(t)
	require.Equal(t, contract.Present, e.subscribe().State, "the subscribe")
	e.robot.SetRateLimited(true)

	obs := e.observe()

	assert.Equal(t, contract.Unknown, obs.State, "the cluster: %s", obs.StatusDetail)
	assert.Contains(t, obs.StatusDetail, "request limit")
	assert.False(t, obs.Redrive, "the observe asked for a subscribe while Robot's limit holds")
}
