package metrics

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The aliases below keep the OpenTelemetry observable API in one place, so the
// samplers and collectors read in the vocabulary of this package.

type metricObserver = metric.Observer

func metricDescription(instrument Instrument) metric.InstrumentOption {
	return metric.WithDescription(instrument.Description)
}

func metricUnit(instrument Instrument) metric.InstrumentOption {
	return metric.WithUnit(instrument.Unit)
}

func observeAttributes(attrs ...attribute.KeyValue) metric.ObserveOption {
	return metric.WithAttributes(attrs...)
}
