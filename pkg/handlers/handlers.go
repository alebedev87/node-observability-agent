package handlers

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/openshift/node-observability-agent/pkg/connectors"
	"github.com/openshift/node-observability-agent/pkg/runs"
	"github.com/openshift/node-observability-agent/pkg/statelocker"
)

const (
	ready                 = "Service is ready"
	httpRespErrMsg        = "unable to send response"
	timeout        int    = 35
	logFileExt     string = "log"
	errorFileExt   string = "err"
	pprofFileExt   string = "pprof"
	crioFilePrefix string = "crio"
)

var (
	hlog = logrus.WithField("module", "handler")
)

// Handlers holds the parameters necessary for running the CRIO and Kubelet profiling
type Handlers struct {
	Token                string
	NodeIP               string
	StorageFolder        string
	CrioUnixSocket       string
	CrioPreferUnixSocket bool
	CACerts              *x509.CertPool
	stateLocker          statelocker.StateLocker
}

// NewHandlers creates a new instance of Handlers from the given parameters
func NewHandlers(token string, caCerts *x509.CertPool, storageFolder string, crioUnixSocket string, nodeIP string, crioPreferUnixSocket bool) *Handlers {
	h := &Handlers{
		Token:                token,
		CACerts:              caCerts,
		NodeIP:               nodeIP,
		StorageFolder:        storageFolder,
		CrioUnixSocket:       crioUnixSocket,
		CrioPreferUnixSocket: crioPreferUnixSocket,
	}
	h.stateLocker = statelocker.NewStateLock(h.errorOutputFilePath())
	return h
}

// Status is called when the agent receives an HTTP request on endpoint /status.
// It returns:
// * HTTP 500 if the agent is in error,
// * HTTP 409 if a previous profiling is still ongoing,
// * HTTP 200 if the agent is ready
func (h *Handlers) Status(w http.ResponseWriter, r *http.Request) {
	id, state, err := h.stateLocker.LockInfo()
	if err != nil {
		http.Error(w, "error retrieving service status",
			http.StatusInternalServerError)
		hlog.Errorf("Error retrieving service status : %v", err)
		return
	}
	switch state {
	case statelocker.InError:
		err = respondBusyOrError(id.String(), w, true)
		if err != nil {
			http.Error(w, httpRespErrMsg,
				http.StatusInternalServerError)
			hlog.Error(httpRespErrMsg)
			return
		}
	case statelocker.Taken:
		err := respondBusyOrError(id.String(), w, false)
		if err != nil {
			http.Error(w, httpRespErrMsg,
				http.StatusInternalServerError)
			hlog.Error(httpRespErrMsg)
			return
		}
	case statelocker.Free:
		_, err := w.Write([]byte(ready))
		if err != nil {
			hlog.Errorf("could not send response busy : %v", err)
		}
	}
}

// HandleProfiling is called when the agent receives an HTTP request on endpoint /pprof
// After checking the agent is not in error, and that no previous profiling is still ongoing,
// it triggers the kubelet and CRIO profiling in separate goroutines, and launches a separate
// function to process the results in a goroutine as well
func (h *Handlers) HandleProfiling(w http.ResponseWriter, r *http.Request) {
	uid, state, err := h.stateLocker.Lock()
	if err != nil {
		http.Error(w, "service is either busy or in error, try again",
			http.StatusInternalServerError)
		hlog.Error(err)
		return
	}

	switch state {
	case statelocker.InError:
		{
			err := respondBusyOrError(uid.String(), w, true)
			if err != nil {
				http.Error(w, httpRespErrMsg,
					http.StatusInternalServerError)
				hlog.Error(httpRespErrMsg)
				return
			}
			return
		}
	case statelocker.Taken:
		{
			err := respondBusyOrError(uid.String(), w, false)
			if err != nil {
				http.Error(w, httpRespErrMsg,
					http.StatusInternalServerError)
				hlog.Error(httpRespErrMsg)
				return
			}
			return
		}
	case statelocker.Free:
		{

			// Channel for collecting results of profiling
			runResultsChan := make(chan runs.ProfilingRun)

			// Launch both profilings in parallel as well as the routine to wait for results
			go func() {
				client := http.DefaultClient
				client.Transport = http.DefaultTransport
				client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: h.CACerts, MinVersion: tls.VersionTLS12}
				hlog.Info("caCertPool loaded in HTTP client Config")
				runResultsChan <- h.profileKubelet(uid.String(), client)
			}()

			go func() {
				if h.CrioPreferUnixSocket {
					runResultsChan <- h.profileCrioViaUnixSocket(uid.String(), &connectors.Connector{})
				} else {
					runResultsChan <- h.profileCrioViaHTTP(uid.String(), newDefaultHTTPClient())
				}

			}()

			go h.processResults(uid, runResultsChan)
			// Send a HTTP 200 straight away
			err := sendUID(w, uid)
			if err != nil {
				hlog.Error(err)
				return
			}
		}
	}

}

func (h *Handlers) processResults(uid uuid.UUID, runResultsChan chan runs.ProfilingRun) {
	arun := runs.Run{
		ID:            uid,
		ProfilingRuns: []runs.ProfilingRun{},
	}
	// unlock as soon as finished processing
	defer func() {
		err := h.stateLocker.Unlock()
		if err != nil {
			hlog.Fatal(err)
		}
		close(runResultsChan)
	}()
	// wait for the results
	arun.ProfilingRuns = []runs.ProfilingRun{}
	isTimeout := false
	for nb := 0; nb < 2 && !isTimeout; {
		select {
		case pr := <-runResultsChan:
			nb++
			arun.ProfilingRuns = append(arun.ProfilingRuns, pr)
		case <-time.After(time.Second * time.Duration(timeout)):
			//timeout! dont wait anymore
			prInTimeout := runs.ProfilingRun{
				Type:       runs.UnknownRun,
				Successful: false,
				BeginTime:  time.Now(),
				EndTime:    time.Now(),
				Error:      fmt.Sprintf("timeout after waiting %ds", timeout),
			}
			prInTimeout.Error = fmt.Sprintf("timeout after waiting %ds", timeout)
			arun.ProfilingRuns = append(arun.ProfilingRuns, prInTimeout)
			isTimeout = true
		}
	}

	// Process the results
	var errorMessage bytes.Buffer
	var logMessage bytes.Buffer
	for _, profRun := range arun.ProfilingRuns {
		if profRun.Error != "" {
			errorMessage.WriteString("errors encountered while running " + string(profRun.Type) + ":\n")
			errorMessage.WriteString(profRun.Error + "\n")
		}
		logMessage.WriteString(string(profRun.Type) + " - " + arun.ID.String() + ": " + profRun.BeginTime.String() + " -> " + profRun.EndTime.String() + "\n")
	}

	if errorMessage.Len() > 0 {
		hlog.Error(errorMessage.String())
		err := h.stateLocker.SetError(arun)
		if err != nil {
			hlog.Fatal(err)
		}
	} else {
		// no errors : simply log the results
		hlog.Info(logMessage.String())
		if err := writeRunToFile(arun, h.runLogOutputFilePath(arun)); err != nil {
			hlog.Fatal(err)
		}
	}
}

// outputFilePath returns the full file path from the storage folder.
func (h *Handlers) outputFilePath(prefix, id, ext string) string {
	if prefix != "" {
		prefix = prefix + "-"
	}
	return filepath.Join(h.StorageFolder, prefix+id+"."+ext)
}

// crioPprofOutputFilePath returns the full file path for CRIO pprof output.
func (h *Handlers) crioPprofOutputFilePath(id string) string {
	return h.outputFilePath(crioFilePrefix, id, pprofFileExt)
}

// runLogOutputFilePath returns the full file path for CRIO pprof output.
func (h *Handlers) runLogOutputFilePath(r runs.Run) string {
	return h.outputFilePath("", r.ID.String(), logFileExt)
}

// errorOutputFilePath returns the full file path for error file.
func (h *Handlers) errorOutputFilePath() string {
	return h.outputFilePath("", "agent", errorFileExt)
}

func sendUID(w http.ResponseWriter, runID uuid.UUID) error {
	response := runs.Run{
		ID: runID,
	}

	jsResponse, err := json.Marshal(response)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(jsResponse)
	if err != nil {
		hlog.Errorf("Unable to send HTTP response : %v", err)
	}
	return nil
}

func respondBusyOrError(uid string, w http.ResponseWriter, isError bool) error {

	message := ""

	if isError {
		w.WriteHeader(http.StatusInternalServerError)
		message = uid + " failed."
	} else {
		w.WriteHeader(http.StatusConflict)
		message = uid + " still running"
	}
	_, err := w.Write([]byte(message))
	if err != nil {
		hlog.Errorf("Unable to send HTTP response : %v", err)
		return err
	}
	return nil
}

// writeRunToFile writes the contents of the run into the given file.
func writeRunToFile(run runs.Run, filePath string) error {
	bytes, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("unable to marshal run %q into json: %w", run.ID.String(), err)
	}
	if err := os.WriteFile(filePath, bytes, 0600); err != nil {
		return fmt.Errorf("error writing into file %q: %w", filePath, err)
	}
	return nil
}

// writeToFile writes the contents of the reader into the given file.
func writeToFile(reader io.ReadCloser, filePath string) error {
	out, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", filePath, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, reader); err != nil {
		return fmt.Errorf("failed to write to file %s: %w", filePath, err)
	}
	return nil
}
