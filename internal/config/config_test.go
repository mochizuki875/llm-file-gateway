package config

import (
	"testing"
	"time"
)

func TestLoadDocumentTextExtraction(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")

	for _, test := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "default", want: true},
		{name: "enabled", value: "true", want: true},
		{name: "disabled", value: "false", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DOCUMENT_TEXT_EXTRACTION_ENABLED", test.value)
			settings, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if settings.TextExtractionEnabled != test.want {
				t.Fatalf("TextExtractionEnabled = %t, want %t", settings.TextExtractionEnabled, test.want)
			}
		})
	}

	t.Run("invalid", func(t *testing.T) {
		t.Setenv("DOCUMENT_TEXT_EXTRACTION_ENABLED", "invalid")
		if _, err := Load(); err == nil || err.Error() != "DOCUMENT_TEXT_EXTRACTION_ENABLED must be a boolean" {
			t.Fatalf("Load() error = %v", err)
		}
	})
}

func TestLoadConversionWorkers(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")

	t.Run("default", func(t *testing.T) {
		t.Setenv("CONVERSION_WORKERS", "")
		settings, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if settings.ConversionWorkers != 2 {
			t.Fatalf("ConversionWorkers = %d, want 2", settings.ConversionWorkers)
		}
	})

	t.Run("custom", func(t *testing.T) {
		t.Setenv("CONVERSION_WORKERS", "4")
		settings, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if settings.ConversionWorkers != 4 {
			t.Fatalf("ConversionWorkers = %d, want 4", settings.ConversionWorkers)
		}
	})

	for _, value := range []string{"0", "invalid"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("CONVERSION_WORKERS", value)
			if _, err := Load(); err == nil || err.Error() != "CONVERSION_WORKERS must be a positive integer" {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadLogVerbosity(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")

	for _, test := range []struct {
		name  string
		value string
		want  int
	}{
		{name: "default", want: 0},
		{name: "normal", value: "0", want: 0},
		{name: "debug", value: "1", want: 1},
		{name: "trace", value: "2", want: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("LOGLEVEL", test.value)
			settings, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if settings.LogVerbosity != test.want {
				t.Fatalf("LogVerbosity = %d, want %d", settings.LogVerbosity, test.want)
			}
		})
	}

	for _, value := range []string{"-1", "3", "debug"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("LOGLEVEL", value)
			if _, err := Load(); err == nil || err.Error() != "LOGLEVEL must be an integer between 0 and 2" {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadFileTTL(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")

	t.Run("default", func(t *testing.T) {
		t.Setenv("FILE_TTL_SECONDS", "")
		settings, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if settings.FileTTL != 5*time.Minute {
			t.Fatalf("FileTTL = %s, want 5m", settings.FileTTL)
		}
	})

	t.Run("custom", func(t *testing.T) {
		t.Setenv("FILE_TTL_SECONDS", "900")
		settings, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if settings.FileTTL != 15*time.Minute {
			t.Fatalf("FileTTL = %s, want 15m", settings.FileTTL)
		}
	})

	for _, value := range []string{"0", "invalid"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("FILE_TTL_SECONDS", value)
			if _, err := Load(); err == nil || err.Error() != "FILE_TTL_SECONDS must be a positive integer" {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadRequiresVLLMAPIKey(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "")

	if _, err := Load(); err == nil || err.Error() != "VLLM_API_KEY is required" {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRequiresGatewayAPIKeyWhenAuthenticationEnabled(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")
	t.Setenv("GATEWAY_AUTH_REQUIRED", "true")
	t.Setenv("GATEWAY_API_KEY", "")

	if _, err := Load(); err == nil || err.Error() != "GATEWAY_API_KEY is required when GATEWAY_AUTH_REQUIRED=true" {
		t.Fatalf("Load() error = %v", err)
	}
}
