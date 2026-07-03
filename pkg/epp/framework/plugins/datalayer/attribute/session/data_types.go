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

// Package session declares the SessionID attribute that carries per-request
// session identity for affinity scoring and filtering. The value is published
// once per request on the InferenceRequest attribute store.
package session

import (
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	chainidentityconstants "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/chainidentity/constants"
	sessionidconstants "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/sessionid/constants"
)

// SessionIDDataKey identifies the session identifier published on the request
// attribute store. The default producer is the session-id-producer.
var SessionIDDataKey = plugin.NewDataKey("SessionIDDataKey", sessionidconstants.SessionIDProducerType)

// ForkParentDataKey identifies the parent lineage id published by the
// chain-identity producer when a request is discovered to fork from an
// existing lineage: the child shares the parent's KV prefix and adopts its
// coverage on first sight.
var ForkParentDataKey = plugin.NewDataKey("ForkParentDataKey", chainidentityconstants.ChainIdentityProducerType)

// StructuralIdentityDataKey marks requests whose session id is derived from
// content chain hashes. For such requests a rewritten history yields a new
// lineage id by construction, so heuristic rollover detection (prompt-shrink
// ratios) must not fire — a short delta continuation would be mistaken for a
// rewrite.
var StructuralIdentityDataKey = plugin.NewDataKey("StructuralIdentityDataKey", chainidentityconstants.ChainIdentityProducerType)

// SessionID is the session identifier extracted from a request.
type SessionID string

// ReadSessionID returns the SessionID published by the default producer on the
// request attribute store, or "" and false if absent.
//
// Consumers should use this helper rather than reading the attribute directly:
// it encapsulates both the key construction and the type assertion, so a
// future change of storage location or value type does not ripple through
// every reader.
func ReadSessionID(r *fwksched.InferenceRequest) (SessionID, bool) {
	key := SessionIDDataKey.WithNonEmptyProducerName(sessionidconstants.SessionIDProducerType).String()
	return fwksched.ReadRequestAttribute[SessionID](r, key)
}

// ReadDerivedSessionID returns the content-derived lineage id published by
// the chain-identity producer, or "" and false if absent. Derived identity is
// canonical when present: declared client ids alias it inside the producer.
func ReadDerivedSessionID(r *fwksched.InferenceRequest) (SessionID, bool) {
	key := SessionIDDataKey.WithNonEmptyProducerName(chainidentityconstants.ChainIdentityProducerType).String()
	return fwksched.ReadRequestAttribute[SessionID](r, key)
}

// ReadForkParent returns the fork-parent lineage id published by the
// chain-identity producer, or "" and false if absent.
func ReadForkParent(r *fwksched.InferenceRequest) (SessionID, bool) {
	return fwksched.ReadRequestAttribute[SessionID](r, ForkParentDataKey.String())
}

// HasStructuralIdentity reports whether the request's session id is derived
// from content chain hashes (see StructuralIdentityDataKey).
func HasStructuralIdentity(r *fwksched.InferenceRequest) bool {
	v, ok := fwksched.ReadRequestAttribute[bool](r, StructuralIdentityDataKey.String())
	return ok && v
}
