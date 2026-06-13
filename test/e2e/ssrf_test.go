package e2e

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	testutils "github.com/llm-d/llm-d-router/test/utils"
)

const ssrfTestLabel = "SSRF"

// createModelServersPDSSRF creates model servers with SSRF protection enabled on the sidecar.
func createModelServersPDSSRF(prefillReplicas, decodeReplicas int) []string {
	return createModelServersFromKustomize(pdDisaggDir, map[string]string{
		"${KV_CACHE_ENABLED}":     "false",
		"${CONNECTOR_TYPE}":       "nixlv2",
		"${VLLM_REPLICA_COUNT_D}": strconv.Itoa(decodeReplicas),
		"${VLLM_REPLICA_COUNT_P}": strconv.Itoa(prefillReplicas),
		// Enable SSRF protection on the sidecar
		"${SIDECAR_EXTRA_ARGS}": "--enable-ssrf-protection=true --inference-pool=" + poolName,
	})
}

// sendRequestWithPrefillHeader sends a chat completion request with the X-Prefiller-Host-Port header
// set to the specified value. Returns the HTTP status code.
func sendRequestWithPrefillHeader(prefillHeader string) int {
	body := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"test"}],"max_tokens":10}`, simModelName)
	req, err := http.NewRequest(http.MethodPost,
		"http://localhost:"+port+"/v1/chat/completions",
		strings.NewReader(body))
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	req.Header.Set("Content-Type", "application/json")
	if prefillHeader != "" {
		req.Header.Set("X-Prefiller-Host-Port", prefillHeader)
	}

	resp, err := http.DefaultClient.Do(req)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	defer func() {
		_ = resp.Body.Close()
	}()

	_, err = io.ReadAll(resp.Body)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

	return resp.StatusCode
}

var _ = ginkgo.Describe("SSRF Protection", ginkgo.Label(ssrfTestLabel), func() {
	ginkgo.When("Sidecar has SSRF protection enabled", func() {
		ginkgo.It("should allow requests with valid prefill header targeting an allowed pod", func() {
			infPoolObjects = createInferencePool(1, true)

			prefillReplicas := 1
			decodeReplicas := 1
			modelServers := createModelServersPDSSRF(prefillReplicas, decodeReplicas)

			epp := createEndPointPicker(pdConfig)

			prefillPods, decodePods := getModelServerPods(podSelector, prefillSelector, decodeSelector)
			gomega.Expect(prefillPods).Should(gomega.HaveLen(prefillReplicas))
			gomega.Expect(decodePods).Should(gomega.HaveLen(decodeReplicas))

			// Send a request with a valid prefill header targeting an allowed pod
			// The header format is "host:port" where host is the pod IP and port is from the InferencePool targetPorts
			validHeader := prefillPods[0] + ":8000"
			statusCode := sendRequestWithPrefillHeader(validHeader)
			gomega.Expect(statusCode).Should(gomega.Equal(http.StatusOK),
				"Request with valid prefill header should be allowed")

			testutils.DeleteObjects(testConfig, epp)
			testutils.DeleteObjects(testConfig, modelServers)
		})

		ginkgo.It("should block requests with invalid port in prefill header", func() {
			infPoolObjects = createInferencePool(1, true)

			prefillReplicas := 1
			decodeReplicas := 1
			modelServers := createModelServersPDSSRF(prefillReplicas, decodeReplicas)

			epp := createEndPointPicker(pdConfig)

			prefillPods, _ := getModelServerPods(podSelector, prefillSelector, decodeSelector)
			gomega.Expect(prefillPods).Should(gomega.HaveLen(prefillReplicas))

			// Send a request with an invalid port (9999) that is not in the InferencePool targetPorts
			invalidPortHeader := prefillPods[0] + ":9999"
			statusCode := sendRequestWithPrefillHeader(invalidPortHeader)
			gomega.Expect(statusCode).Should(gomega.Equal(http.StatusForbidden),
				"Request with invalid port should be blocked")

			testutils.DeleteObjects(testConfig, epp)
			testutils.DeleteObjects(testConfig, modelServers)
		})

		ginkgo.It("should block requests with invalid host in prefill header", func() {
			infPoolObjects = createInferencePool(1, true)

			prefillReplicas := 1
			decodeReplicas := 1
			modelServers := createModelServersPDSSRF(prefillReplicas, decodeReplicas)

			epp := createEndPointPicker(pdConfig)

			// Send a request with an invalid host that is not in the allowlist
			invalidHostHeader := "192.168.99.99:8000"
			statusCode := sendRequestWithPrefillHeader(invalidHostHeader)
			gomega.Expect(statusCode).Should(gomega.Equal(http.StatusForbidden),
				"Request with invalid host should be blocked")

			testutils.DeleteObjects(testConfig, epp)
			testutils.DeleteObjects(testConfig, modelServers)
		})
	})
})
