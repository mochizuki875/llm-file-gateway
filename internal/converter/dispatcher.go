package converter

import "github.com/mochizuki875/llm-file-gateway/internal/converter/extractor"

type Dispatcher struct {
	registry *Registry
	fallback DocumentConverter
}

func NewDispatcher(registry *Registry) *Dispatcher {
	if registry == nil {
		panic("converter registry must not be nil")
	}
	return &Dispatcher{
		registry: registry,
		fallback: newTextConverter("", "text/plain", extractor.PlainText),
	}
}

func (dispatcher *Dispatcher) ResolveConverter(path string) (DocumentConverter, error) {
	documentConverter, err := dispatcher.registry.ForPath(path)
	if err != nil && dispatcher.fallback != nil {
		return dispatcher.fallback, nil
	}
	return documentConverter, err
}
