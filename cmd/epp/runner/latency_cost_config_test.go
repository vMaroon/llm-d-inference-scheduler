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

package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/llm-d/llm-d-router/pkg/epp/config/loader"
	"github.com/llm-d/llm-d-router/pkg/epp/datalayer"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	testutils "github.com/llm-d/llm-d-router/test/utils"
)

// TestLatencyCostSampleConfig loads the shipped latency-cost sample config
// through the real loader pipeline: plugin instantiation, default injection,
// auto-creation of missing data producers, and DAG validation. It gates that
// the sample stays loadable and that the latency-cost scorer's dependency
// chain auto-wires the producer stack.
func TestLatencyCostSampleConfig(t *testing.T) {
	configBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "config", "epp-latency-cost-config.yaml"))
	require.NoError(t, err, "sample config must exist")

	runner := NewRunner()
	runner.registerInTreePlugins()

	logger := zap.New(zap.UseDevMode(true))
	rawConfig, _, err := loader.LoadRawConfig(configBytes, logger)
	require.NoError(t, err, "sample config must parse")

	ctx := context.Background()
	handle := testutils.NewTestHandle(ctx)
	_, err = loader.InstantiateAndConfigure(rawConfig, handle, logger)
	require.NoError(t, err, "sample config must instantiate")

	require.NoError(t,
		datalayer.CreateMissingDataProducers(ctx, fwkplugin.DefaultProducerRegistry, fwkplugin.Registry, handle),
		"missing default producers must auto-create")

	_, err = datalayer.ValidateAndOrderDataDependencies(handle.GetAllPlugins())
	require.NoError(t, err, "data dependency DAG must validate")

	for _, name := range []string{
		"latency-cost-producer",
		"latency-cost-scorer",
		"token-producer",
		"approx-prefix-cache-producer",
		"inflight-load-producer",
	} {
		require.NotNil(t, handle.Plugin(name), "plugin %q must be instantiated", name)
	}
}
