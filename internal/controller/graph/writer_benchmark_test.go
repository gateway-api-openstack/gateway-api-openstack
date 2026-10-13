/*
Copyright 2026 The gateway-api-openstack Authors.

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

package graph

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkWriterCoordination measures only local writer and lock overhead.
// Sources and the provider are in-memory fakes, so it is not a cloud throughput
// or latency measurement.
func BenchmarkWriterCoordination(b *testing.B) {
	for _, mode := range []string{"serial", "same_gateway", "different_gateways"} {
		b.Run(mode, func(b *testing.B) {
			w := writerFixture()
			var lockWait, observed atomic.Int64
			w.Observer = stageObserverFunc(func(stage Stage, duration time.Duration, _ bool) {
				if stage == StageLockWait {
					lockWait.Add(duration.Nanoseconds())
					observed.Add(1)
				}
			})
			request := writerRequest()
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			if mode == "serial" {
				for b.Loop() {
					if _, err := w.ReconcileGraph(ctx, request); err != nil {
						b.Fatal(err)
					}
				}
			} else {
				var worker atomic.Int64
				b.RunParallel(func(pb *testing.PB) {
					local := request
					if mode == "different_gateways" {
						local.Identity.GatewayUID = strconv.FormatInt(worker.Add(1), 10)
					}
					for pb.Next() {
						if _, err := w.ReconcileGraph(ctx, local); err != nil {
							b.Error(err)
							return
						}
					}
				})
			}
			b.StopTimer()
			b.ReportMetric(float64(lockWait.Load())/float64(observed.Load()), "lock-wait-ns/op")
		})
	}
}
