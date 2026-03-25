package scorer

import "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"

// PrefixCacheHitState communicates cache-hit information from the
// PrecisePrefixCacheScorer to downstream plugins (e.g. NoHitLRU).
//
// Unlike the IGW SchedulingContextState (which was designed for estimation-based
// prefix scoring), this type carries the precise signal needed by cold-request
// detection: whether any endpoint had a non-zero cache match.
type PrefixCacheHitState struct {
	// HasCacheHit is true when at least one endpoint scored > 0 matching blocks.
	HasCacheHit bool
	// MaxRawScore is the highest raw (un-normalized) score across all endpoints.
	// A value of 0 means no endpoint had any cached blocks for this request.
	MaxRawScore float64
}

// Clone implements plugin.StateData.
func (s *PrefixCacheHitState) Clone() plugin.StateData {
	return &PrefixCacheHitState{
		HasCacheHit: s.HasCacheHit,
		MaxRawScore: s.MaxRawScore,
	}
}
