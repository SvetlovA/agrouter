package main

import "github.com/SvetlovA/agrouter/pkg/router"

// confidenceJSON records Jev's confidence and arithmetic means for reporting only.
type confidenceJSON struct {
	Route             *float64                   `json:"route"`
	RouteAverage      *float64                   `json:"route_average,omitempty"`
	ComplexityAverage *float64                   `json:"complexity_average,omitempty"`
	RoutingChunks     []routingConfidenceJSON    `json:"routing_chunks,omitempty"`
	ComplexityChunks  []complexityConfidenceJSON `json:"complexity_chunks,omitempty"`
}

type routingConfidenceJSON struct {
	Field      string  `json:"field"`
	Index      int     `json:"index"`
	Of         int     `json:"of"`
	Confidence float64 `json:"confidence"`
}

type complexityConfidenceJSON struct {
	Index      int     `json:"index"`
	Of         int     `json:"of"`
	Confidence float64 `json:"confidence"`
}

func recordedConfidence(d router.Decision) *confidenceJSON {
	if d.Answer == nil && d.Pooled == nil && d.Complexity == nil {
		return nil
	}
	out := &confidenceJSON{}
	if d.Answer != nil {
		out.Route = &d.Answer.Confidence
		out.RouteAverage = &d.Answer.Confidence
	}
	if d.Pooled != nil {
		var sum float64
		for _, c := range d.Pooled.Chunks {
			sum += c.Confidence
			out.RoutingChunks = append(out.RoutingChunks, routingConfidenceJSON{
				Field: c.Field, Index: c.Index, Of: c.Of, Confidence: c.Confidence,
			})
		}
		if count := len(d.Pooled.Chunks); count > 0 {
			average := sum / float64(count)
			out.RouteAverage = &average
		}
	}
	if d.Complexity != nil {
		var sum float64
		for _, c := range d.Complexity.Chunks {
			sum += c.Confidence
			out.ComplexityChunks = append(out.ComplexityChunks, complexityConfidenceJSON{
				Index: c.Index, Of: c.Of, Confidence: c.Confidence,
			})
		}
		if count := len(d.Complexity.Chunks); count > 0 {
			average := sum / float64(count)
			out.ComplexityAverage = &average
		}
	}
	return out
}
