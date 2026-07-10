/*
Copyright 2026 The Kubernetes Authors.

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

package sessionprefix

import (
	"time"

	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/preadmitter/agentidentity"
)

const (
	defaultSessionTTL    = 30 * time.Minute
	defaultMaxSessions   = 100_000
	defaultCharsPerToken = 4.0

	sweepInterval = time.Minute

	// experimentalDefaultPrefillProfile is a hardcoded profile name for prefill nodes.
	experimentalDefaultPrefillProfile = "prefill"

	// podActiveCheckInterval is the interval at which we check if pods are still active.
	podActiveCheckInterval = 2 * time.Minute
)

// defaultSessionHeaders are the declared-session-id headers read when none
// are configured: the generic default plus the provider headers recognized
// by the agent-identity preadmitter.
var defaultSessionHeaders = []string{
	"x-session-id",
	agentidentity.ClaudeCodeSessionHeader,
	agentidentity.OpenCodeSessionHeader,
	agentidentity.CodexSessionHeader,
	agentidentity.CodexSessionHeaderLegacy,
}

// produceState carries the derived chain from Produce to PreRequest (which
// commits it and fills in the resolved session id) and on to ResponseBody
// (which records usage against it).
type produceState struct {
	chain     []uint64
	declared  []string
	xEst      int64
	sessionID string
}

func (s *produceState) Clone() plugin.StateData {
	clone := &produceState{
		chain:     make([]uint64, len(s.chain)),
		declared:  make([]string, len(s.declared)),
		xEst:      s.xEst,
		sessionID: s.sessionID,
	}
	copy(clone.chain, s.chain)
	copy(clone.declared, s.declared)
	return clone
}
