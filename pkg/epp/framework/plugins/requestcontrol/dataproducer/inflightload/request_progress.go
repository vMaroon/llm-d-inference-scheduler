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

package inflightload

import "sync"

type requestProgress struct {
	awaitingFirstResponse bool
	lastProgressUnixMilli int64
}

type requestProgressSnapshot struct {
	observableRequests            int64
	awaitingFirstResponse         int64
	progressTimestampSumUnixMilli int64
	oldestProgressUnixMilli       int64
}

type requestProgressTracker struct {
	mu        sync.RWMutex
	endpoints map[string]map[string]requestProgress
}

func newRequestProgressTracker() *requestProgressTracker {
	return &requestProgressTracker{endpoints: make(map[string]map[string]requestProgress)}
}

func (t *requestProgressTracker) add(endpointID, requestKey string, observable bool, nowUnixMilli int64) {
	if t == nil || !observable {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	requests := t.endpoints[endpointID]
	if requests == nil {
		requests = make(map[string]requestProgress)
		t.endpoints[endpointID] = requests
	}
	requests[requestKey] = requestProgress{
		awaitingFirstResponse: true,
		lastProgressUnixMilli: nowUnixMilli,
	}
}

func (t *requestProgressTracker) markProgress(endpointID, requestKey string, nowUnixMilli int64) {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	requests := t.endpoints[endpointID]
	progress, ok := requests[requestKey]
	if !ok {
		return
	}
	progress.awaitingFirstResponse = false
	progress.lastProgressUnixMilli = nowUnixMilli
	requests[requestKey] = progress
}

func (t *requestProgressTracker) delete(endpointID, requestKey string) {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	requests := t.endpoints[endpointID]
	delete(requests, requestKey)
	if len(requests) == 0 {
		delete(t.endpoints, endpointID)
	}
}

func (t *requestProgressTracker) deleteEndpoint(endpointID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.endpoints, endpointID)
	t.mu.Unlock()
}

func (t *requestProgressTracker) snapshot(endpointID string) requestProgressSnapshot {
	if t == nil {
		return requestProgressSnapshot{}
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	requests := t.endpoints[endpointID]
	snapshot := requestProgressSnapshot{observableRequests: int64(len(requests))}
	for _, progress := range requests {
		if progress.awaitingFirstResponse {
			snapshot.awaitingFirstResponse++
		}
		snapshot.progressTimestampSumUnixMilli += progress.lastProgressUnixMilli
		if snapshot.oldestProgressUnixMilli == 0 || progress.lastProgressUnixMilli < snapshot.oldestProgressUnixMilli {
			snapshot.oldestProgressUnixMilli = progress.lastProgressUnixMilli
		}
	}
	return snapshot
}

func (t *requestProgressTracker) count(endpointID string) int64 {
	return t.snapshot(endpointID).observableRequests
}
