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

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestProgressTrackerConcurrentLifecycle(t *testing.T) {
	t.Parallel()

	tracker := newRequestProgressTracker()
	const requests = 100
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("request-%d", i)
			tracker.add("endpoint", key, true, int64(1_000+i))
			tracker.markProgress("endpoint", key, int64(2_000+i))
		}(i)
	}
	wg.Wait()

	snapshot := tracker.snapshot("endpoint")
	require.Equal(t, int64(requests), snapshot.observableRequests)
	require.Equal(t, int64(0), snapshot.awaitingFirstResponse)
	require.Equal(t, int64(2_000), snapshot.oldestProgressUnixMilli)

	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tracker.delete("endpoint", fmt.Sprintf("request-%d", i))
		}(i)
	}
	wg.Wait()
	require.Equal(t, requestProgressSnapshot{}, tracker.snapshot("endpoint"))
}
