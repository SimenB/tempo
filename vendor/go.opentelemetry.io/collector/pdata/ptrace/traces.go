// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ptrace // import "go.opentelemetry.io/collector/pdata/ptrace"

import (
	"go.opentelemetry.io/collector/pdata/internal"
	otlpcollectortrace "go.opentelemetry.io/collector/pdata/internal/data/protogen/collector/trace/v1"
)

// Traces is the top-level struct that is propagated through the traces pipeline.
// Use NewTraces to create new instance, zero-initialized instance is not valid for use.
type Traces internal.Traces

func newTraces(orig *otlpcollectortrace.ExportTraceServiceRequest) Traces {
	return Traces(internal.NewTraces(orig))
}

func (ms Traces) getOrig() *otlpcollectortrace.ExportTraceServiceRequest {
	return internal.GetOrigTraces(internal.Traces(ms))
}

// NewTraces creates a new Traces struct.
func NewTraces() Traces {
	return newTraces(&otlpcollectortrace.ExportTraceServiceRequest{})
}

// CopyTo copies the Traces instance overriding the destination.
func (ms Traces) CopyTo(dest Traces) {
	ms.ResourceSpans().CopyTo(dest.ResourceSpans())
}

// SpanCount calculates the total number of spans.
func (ms Traces) SpanCount() int {
	spanCount := 0
	rss := ms.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		rs := rss.At(i)
		ilss := rs.ScopeSpans()
		for j := 0; j < ilss.Len(); j++ {
			spanCount += ilss.At(j).Spans().Len()
		}
	}
	return spanCount
}

// ResourceSpans returns the ResourceSpansSlice associated with this Metrics.
func (ms Traces) ResourceSpans() ResourceSpansSlice {
	return newResourceSpansSlice(&ms.getOrig().ResourceSpans)
}
