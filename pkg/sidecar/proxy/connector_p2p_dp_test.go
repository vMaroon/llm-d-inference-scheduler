/*
Copyright 2026 The llm-d Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package proxy

import (
	"bytes"
	"io"
	"net/http"

	. "github.com/onsi/ginkgo/v2" // nolint:revive
	. "github.com/onsi/gomega"    // nolint:revive

	"github.com/llm-d/llm-d-router/pkg/common/routing"
)

// Under data parallelism each rank's OffloadingConnector P2P tier binds its
// own port, carried per request by x-kv-cache-source-p2p-port; a valid value
// overrides the --p2p-connector-port flag for the cached-prefix pull and the
// header itself never reaches the upstream engine.
var _ = Describe("P2P KV cache source p2p port header", func() {

	var testInfo *sidecarTestInfo

	const p2pConnectorPort = 7777
	const sourceP2PPort = "7788"
	// A private pod-range peer address (the kind an InferencePool would allow).
	const kvCacheSource = "10.9.9.9:8000"

	BeforeEach(func() {
		testInfo = sidecarConnectionTestSetup(KVConnectorOffloading)
		testInfo.proxy.config.P2PConnectorPort = p2pConnectorPort
		// SSRF allowlist disabled in-test (an enabled one requires a live
		// InferencePool), so a well-formed source passes.
		testInfo.proxy.allowlistValidator = &AllowlistValidator{enabled: false}
	})

	sendRequest := func(proxyBaseAddr string, headers map[string]string) {
		req, err := http.NewRequest(http.MethodPost, proxyBaseAddr+ChatCompletionsPath,
			bytes.NewReader([]byte(chatCompletionsRequestBody)))
		Expect(err).ToNot(HaveOccurred())
		for k, v := range headers {
			req.Header.Add(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		Expect(err).ToNot(HaveOccurred())
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			bp, _ := io.ReadAll(resp.Body) //nolint:errcheck
			Fail(string(bp))
		}
	}

	decodeP2P := func() map[string]any {
		decodeReqs := testInfo.decodeHandler.GetCompletionRequests()
		Expect(decodeReqs).To(HaveLen(1))
		kvParams, ok := decodeReqs[0][requestFieldKVTransferParams].(map[string]any)
		Expect(ok).To(BeTrue())
		p2p, ok := kvParams[requestFieldP2PParams].(map[string]any)
		Expect(ok).To(BeTrue())
		return p2p
	}

	It("pulls from the header port on the decoder-only path and strips the header upstream", func() {
		proxyBaseAddr := testInfo.startProxy()

		sendRequest(proxyBaseAddr, map[string]string{
			routing.KVCacheSourceHeader:        kvCacheSource,
			routing.KVCacheSourceP2PPortHeader: sourceP2PPort,
		})

		p2p := decodeP2P()
		Expect(p2p[requestFieldRemoteHost]).To(Equal("10.9.9.9"))
		Expect(p2p[requestFieldRemotePort]).To(BeNumerically("==", 7788))

		// Neither source header may leak to the upstream engine.
		upstreamHeaders := testInfo.decodeHandler.GetCompletionHeaders()
		Expect(upstreamHeaders).To(HaveLen(1))
		Expect(upstreamHeaders[0].Get(routing.KVCacheSourceHeader)).To(BeEmpty())
		Expect(upstreamHeaders[0].Get(routing.KVCacheSourceP2PPortHeader)).To(BeEmpty())

		testInfo.cancelFn()
		<-testInfo.stoppedCh
	})

	It("falls back to the flag port when the header is absent", func() {
		proxyBaseAddr := testInfo.startProxy()

		sendRequest(proxyBaseAddr, map[string]string{routing.KVCacheSourceHeader: kvCacheSource})

		Expect(decodeP2P()[requestFieldRemotePort]).To(BeNumerically("==", p2pConnectorPort))

		testInfo.cancelFn()
		<-testInfo.stoppedCh
	})

	DescribeTable("falls back to the flag port on a malformed header value",
		func(malformed string) {
			proxyBaseAddr := testInfo.startProxy()

			sendRequest(proxyBaseAddr, map[string]string{
				routing.KVCacheSourceHeader:        kvCacheSource,
				routing.KVCacheSourceP2PPortHeader: malformed,
			})

			Expect(decodeP2P()[requestFieldRemotePort]).To(BeNumerically("==", p2pConnectorPort))

			testInfo.cancelFn()
			<-testInfo.stoppedCh
		},
		Entry("not a number", "not-a-port"),
		Entry("zero", "0"),
		Entry("out of range", "70000"),
	)

	It("scopes the header port to the source pull under P/D disaggregation", func() {
		proxyBaseAddr := testInfo.startProxy()

		prefillHostPort := testInfo.prefillBackend.URL[len("http://"):]
		sendRequest(proxyBaseAddr, map[string]string{
			routing.PrefillEndpointHeader:      prefillHostPort,
			routing.KVCacheSourceHeader:        kvCacheSource,
			routing.KVCacheSourceP2PPortHeader: sourceP2PPort,
		})

		Eventually(func() int {
			return len(testInfo.prefillHandler.GetCompletionRequests())
		}).Should(Equal(1))

		// Prefill leg: the cached-prefix pull targets the source rank's port.
		preq := testInfo.prefillHandler.GetCompletionRequests()[0]
		prefillKVParams, ok := preq[requestFieldKVTransferParams].(map[string]any)
		Expect(ok).To(BeTrue())
		p2p, ok := prefillKVParams[requestFieldP2PParams].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(p2p[requestFieldRemoteHost]).To(Equal("10.9.9.9"))
		Expect(p2p[requestFieldRemotePort]).To(BeNumerically("==", 7788))

		// Decode leg: the pull from the prefiller's own tier keeps the flag
		// port; the header describes the source peer, not the prefiller.
		decodeReqs := testInfo.decodeHandler.GetCompletionRequests()
		Expect(decodeReqs).To(HaveLen(1))
		decodeKVParams, ok := decodeReqs[0][requestFieldKVTransferParams].(map[string]any)
		Expect(ok).To(BeTrue())
		prefillParams, ok := decodeKVParams[requestFieldP2PPrefillParams].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(prefillParams[requestFieldRemotePort]).To(BeNumerically("==", p2pConnectorPort))

		testInfo.cancelFn()
		<-testInfo.stoppedCh
	})
})

// The NIXL + --enable-p2p-pull composition honors the per-rank source port on
// the prefill leg.
var _ = Describe("NIXL Connector with P2P pull and source p2p port", func() {

	var testInfo *sidecarTestInfo

	const p2pConnectorPort = 7777
	const kvCacheSource = "10.9.9.9:8000"

	BeforeEach(func() {
		testInfo = sidecarConnectionTestSetup(KVConnectorNIXLV2)
		testInfo.proxy.config.P2PConnectorPort = p2pConnectorPort
		testInfo.proxy.config.EnableP2PPull = true
		testInfo.proxy.allowlistValidator = &AllowlistValidator{enabled: false}
	})

	It("composes the p2p pull with the header port onto the NIXL prefill leg", func() {
		proxyBaseAddr := testInfo.startProxy()

		prefillHostPort := testInfo.prefillBackend.URL[len("http://"):]
		req, err := http.NewRequest(http.MethodPost, proxyBaseAddr+ChatCompletionsPath,
			bytes.NewReader([]byte(chatCompletionsRequestBody)))
		Expect(err).ToNot(HaveOccurred())
		req.Header.Add(routing.PrefillEndpointHeader, prefillHostPort)
		req.Header.Add(routing.KVCacheSourceHeader, kvCacheSource)
		req.Header.Add(routing.KVCacheSourceP2PPortHeader, "7788")

		resp, err := http.DefaultClient.Do(req)
		Expect(err).ToNot(HaveOccurred())
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body) //nolint:errcheck
		Expect(resp.StatusCode).To(Equal(http.StatusOK), string(body))

		reqs := testInfo.prefillHandler.GetCompletionRequests()
		Expect(reqs).To(HaveLen(1))
		kv, ok := reqs[0][requestFieldKVTransferParams].(map[string]any)
		Expect(ok).To(BeTrue())
		p2p, ok := kv[requestFieldP2PParams].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(p2p[requestFieldRemoteHost]).To(Equal("10.9.9.9"))
		Expect(p2p[requestFieldRemotePort]).To(BeNumerically("==", 7788))

		testInfo.cancelFn()
		<-testInfo.stoppedCh
	})
})
