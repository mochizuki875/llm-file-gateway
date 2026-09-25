package converter

import (
	"context"
	"errors"

	"github.com/mochizuki875/llm-file-gateway/internal/converter/extractor"
)

// Dispatcher resolves converters by path and falls back to the plain-text
// converter for unknown extensions.
type Dispatcher struct {
	registry      Registry
	configuration ConverterConfig
	handle        ConverterHandle
	fallback      DocumentConverter
}

// NewDispatcher creates a dispatcher with a plain-text fallback converter.
func NewDispatcher(registry Registry, configuration ConverterConfig, handle ConverterHandle) *Dispatcher {
	if registry == nil {
		panic("converter registry must not be nil")
	}
	return &Dispatcher{
		registry:      registry,
		configuration: configuration,
		handle:        handle,
		fallback:      newTextConverter("", "text/plain", extractor.PlainText),
	}
}

// ResolveConverter returns the converter for the given path, falling back to
// the plain-text converter only when no converter is registered for the
// extension. Factory failures are propagated so that a broken converter is
// not silently replaced by the plain-text fallback.
func (dispatcher *Dispatcher) ResolveConverter(ctx context.Context, path string) (DocumentConverter, error) {
	documentConverter, err := dispatcher.registry.ForPath(ctx, path, dispatcher.configuration, dispatcher.handle)
	if err != nil && dispatcher.fallback != nil && errors.Is(err, ErrConverterNotFound) {
		return dispatcher.fallback, nil
	}
	return documentConverter, err
}
