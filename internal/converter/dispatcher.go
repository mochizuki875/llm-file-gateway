package converter

import (
	"context"
)

type Dispatcher struct {
	registry *Registry
	fallback DocumentConverter
}

func NewDispatcher(registry *Registry) *Dispatcher {
	if registry == nil {
		panic("converter registry must not be nil")
	}
	return &Dispatcher{registry: registry}
}

var defaultDispatcher = &Dispatcher{
	registry: defaultRegistry,
	fallback: newTextConverter("", "text/plain", extractPlainText),
}

func GetConverter(extension string) (DocumentConverter, error) {
	return defaultRegistry.Converter(extension)
}

func Supported(extension string) bool {
	_, err := GetConverter(extension)
	return err == nil
}

func MediaType(extension string) (string, error) {
	documentConverter, err := GetConverter(extension)
	if err != nil {
		return defaultDispatcher.fallback.MediaType(), nil
	}
	return documentConverter.MediaType(), nil
}

func Validate(source string) error {
	return defaultDispatcher.Validate(source)
}

func (dispatcher *Dispatcher) Validate(source string) error {
	documentConverter, err := dispatcher.converterForPath(source)
	if err != nil {
		return err
	}
	return documentConverter.Validate(source)
}

func Convert(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	return defaultDispatcher.Convert(ctx, source, outputDir, options)
}

func (dispatcher *Dispatcher) Convert(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	documentConverter, err := dispatcher.converterForPath(source)
	if err != nil {
		return Result{}, err
	}
	if err := documentConverter.Validate(source); err != nil {
		return Result{}, err
	}
	return documentConverter.Convert(ctx, source, outputDir, options)
}

func (dispatcher *Dispatcher) converterForPath(path string) (DocumentConverter, error) {
	documentConverter, err := dispatcher.registry.ForPath(path)
	if err != nil && dispatcher.fallback != nil {
		return dispatcher.fallback, nil
	}
	return documentConverter, err
}
