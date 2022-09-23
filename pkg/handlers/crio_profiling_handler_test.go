package handlers

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/openshift/node-observability-agent/pkg/connectors"
	"github.com/openshift/node-observability-agent/pkg/runs"
)

func TestProfileCrioViaUnixSocket(t *testing.T) {
	testCases := []struct {
		name      string
		connector connectors.CmdWrapper
		expected  runs.ProfilingRun
	}{
		{
			name:      "Curl command successful, OK",
			connector: &connectors.FakeConnector{Flag: connectors.NoError},
			expected: runs.ProfilingRun{
				Type:       runs.CrioRun,
				Successful: true,
				Error:      "",
			},
		},
		{
			name:      "Network error on curl, ProfilingRun contains error",
			connector: &connectors.FakeConnector{Flag: connectors.SocketErr},
			expected: runs.ProfilingRun{
				Type:       runs.CrioRun,
				Successful: false,
				Error:      fmt.Sprintf("error running CRIO profiling :\n%s", "curl: (7) Couldn't connect to server"),
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandlers("faketoken", x509.NewCertPool(), "/tmp", "/tmp/fakeSocket", "127.0.0.1", false)

			pr := h.profileCrioViaUnixSocket("1234", tc.connector)
			if tc.expected.Type != pr.Type {
				t.Errorf("Expecting a ProfilingRun of type %s but was %s", tc.expected.Type, pr.Type)
			}
			if pr.BeginTime.After(pr.EndTime) {
				t.Errorf("Expecting the registered beginDate %v to be before the profiling endDate %v but was not", pr.BeginTime, pr.EndTime)
			}
			if tc.expected.Successful != pr.Successful {
				t.Errorf("Expecting ProfilingRun to be successful=%t but was %t", tc.expected.Successful, pr.Successful)
			}
		})
	}
}

func TestProfileCrioViaHTTP(t *testing.T) {
	testCases := []struct {
		name             string
		client           HTTPClient
		expectedRun      runs.ProfilingRun
		expectedContents string
	}{
		{
			name: "Nominal",
			client: &fakeHTTPClient{
				response: newFakeResponse([]byte("pprof data"), nil, http.StatusOK),
			},
			expectedRun: runs.ProfilingRun{
				Type:       runs.CrioRun,
				Successful: true,
				Error:      "",
			},
			expectedContents: "pprof data",
		},
		{
			name: "HTTP query error",
			client: &fakeHTTPClient{
				response: newFakeResponse([]byte("pprof data"), nil, http.StatusOK),
				err:      fmt.Errorf("fake error"),
			},
			expectedRun: runs.ProfilingRun{
				Type:       runs.CrioRun,
				Successful: false,
				Error:      "failed sending profiling request to crio: fake error",
			},
		},
		{
			name: "HTTP response status not OK",
			client: &fakeHTTPClient{
				response: newFakeResponse([]byte("pprof data"), nil, http.StatusForbidden),
			},
			expectedRun: runs.ProfilingRun{
				Type:       runs.CrioRun,
				Successful: false,
				Error:      "error status code received from crio: 403",
			},
		},
		{
			name: "Writing to file error",
			client: &fakeHTTPClient{
				response: newFakeResponse([]byte("pprof data"), fmt.Errorf("fake error"), http.StatusOK),
			},
			expectedRun: runs.ProfilingRun{
				Type:       runs.CrioRun,
				Successful: false,
				Error:      `failed writing crio profiling data into file: failed to write to file .+?: fake error`,
			},
		},
	}
	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			id, dir := strconv.Itoa(i), t.TempDir()

			h := NewHandlers("faketoken", x509.NewCertPool(), dir, "/tmp/fakeSocket", "10.10.10.10", true)
			pr := h.profileCrioViaHTTP(id, tc.client)
			if tc.expectedRun.Type != pr.Type {
				t.Errorf("Expecting ProfilingRun type %q but got %q", tc.expectedRun.Type, pr.Type)
			}
			if tc.expectedRun.Successful != pr.Successful {
				t.Errorf("Expecting ProfilingRun successful to be %t but got %t", tc.expectedRun.Successful, pr.Successful)
			}
			if matched, err := regexp.Match(tc.expectedRun.Error, []byte(pr.Error)); err != nil {
				t.Errorf("Failed to match ProfilingRun error: %v", err)
			} else if !matched {
				t.Errorf("Expecting ProfilingRun error %q to match %q regexp", pr.Error, tc.expectedRun.Error)
			}
			if pr.BeginTime.After(pr.EndTime) {
				t.Errorf("Expecting begin time %v to be before the end time %v", pr.BeginTime, pr.EndTime)
			}
			if tc.expectedContents != "" {
				if contents, err := os.ReadFile(dir + "/crio-" + id + ".pprof"); err != nil {
					t.Errorf("Failed to read the contents of crio pprof data: %v", err)
				} else {
					if tc.expectedContents != string(contents) {
						t.Errorf("Expecting pprof contents: %q, but got %q", tc.expectedContents, string(contents))
					}
				}
			}
		})
	}

}

type fakeHTTPClient struct {
	response *http.Response
	err      error
}

func (c *fakeHTTPClient) Get(u string) (*http.Response, error) {
	return c.response, c.err
}

type fakeReadCloser struct {
	*bytes.Reader
	err error
}

func (r *fakeReadCloser) WriteTo(w io.Writer) (int64, error) {
	// writeToFile uses io.Copy which in turn uses WriteTo method if it's present.
	// Since we embed bytes.Reader, WriteTo is the one to be hooked up.
	if r.err != nil {
		return 0, r.err
	}
	return r.Reader.WriteTo(w)
}

func (r *fakeReadCloser) Close() error {
	return nil
}

func newFakeResponse(body []byte, err error, statusCode int) *http.Response {
	return &http.Response{
		Body: &fakeReadCloser{
			Reader: bytes.NewReader(body),
			err:    err,
		},
		StatusCode: statusCode,
	}
}
