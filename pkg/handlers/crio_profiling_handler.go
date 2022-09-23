package handlers

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/openshift/node-observability-agent/pkg/connectors"
	"github.com/openshift/node-observability-agent/pkg/runs"
)

const (
	defaultCrioProfileHost = "localhost"
	defaultCrioProfilePort = "6060"
	crioProfilePath        = "debug/pprof/profile"
)

type HTTPClient interface {
	Get(string) (*http.Response, error)
}

func newDefaultHTTPClient() HTTPClient {
	return http.DefaultClient
}

// profileCrioViaUnixSocket calls CRIO's profiling endpoint (/debug/pprof/profile) on the h.NodeIP, through the unix socket,
// thus triggering a profiling on that node.
// This call requires access to the host socket, which is passed to the agent in parameter crioSocket.
func (h *Handlers) profileCrioViaUnixSocket(uid string, cmd connectors.CmdWrapper) runs.ProfilingRun {
	u := url.URL{
		Scheme: "http",
		Host:   defaultCrioProfileHost,
		Path:   crioProfilePath,
	}
	cmd.Prepare("curl", []string{"--unix-socket", h.CrioUnixSocket, u.String(), "--output", h.crioPprofOutputFilePath(uid)})

	run := runs.ProfilingRun{
		Type:      runs.CrioRun,
		BeginTime: time.Now(),
	}

	hlog.Infof("Requesting CRIO profiling from %s via socket %s", u.String(), h.CrioUnixSocket)

	if output, err := cmd.CmdExec(); err != nil {
		run.EndTime = time.Now()
		run.Error = fmt.Sprintf("error sending profiling request to crio: %s", output)
		return run
	}

	run.EndTime = time.Now()
	run.Successful = true

	hlog.Infof("CRIO profiling request via unix socket successfully finished")

	return run
}

// profileCrioViaHTTP calls CRIO's profiling endpoint (/debug/pprof/profile) on the h.NodeIP
// over http thus triggering a profiling on that node.
// This call requires access to the host network namespace.
func (h *Handlers) profileCrioViaHTTP(uid string, client HTTPClient) runs.ProfilingRun {
	u := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(defaultCrioProfileHost, defaultCrioProfilePort),
		Path:   crioProfilePath,
	}

	run := runs.ProfilingRun{
		Type:      runs.CrioRun,
		BeginTime: time.Now(),
	}

	hlog.Infof("Requesting CRIO profiling from %s", u.String())

	resp, err := client.Get(u.String())
	if err != nil {
		run.EndTime = time.Now()
		run.Error = fmt.Sprintf("failed sending profiling request to crio: %v", err)
		return run
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		run.EndTime = time.Now()
		run.Error = fmt.Sprintf("error status code received from crio: %d", resp.StatusCode)
		return run
	}

	if err := writeToFile(resp.Body, h.crioPprofOutputFilePath(uid)); err != nil {
		run.EndTime = time.Now()
		run.Error = fmt.Sprintf("failed writing crio profiling data into file: %v", err)
		return run
	}

	run.EndTime = time.Now()
	run.Successful = true

	hlog.Info("CRIO profiling request via HTTP successfully finished")

	return run
}
