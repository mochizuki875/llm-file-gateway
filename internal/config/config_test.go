package config

import (
	"testing"
	"time"
)

func TestLoadGatewayListenAddress(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")

	for _, test := range []struct {
		name string
		host string
		port string
		want string
	}{
		{name: "default", want: "127.0.0.1:8080"},
		{name: "custom", host: "0.0.0.0", port: "18080", want: "0.0.0.0:18080"},
		{name: "ipv6", host: "::1", port: "8081", want: "[::1]:8081"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GATEWAY_HOST", test.host)
			t.Setenv("GATEWAY_PORT", test.port)
			settings, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if settings.Address != test.want {
				t.Fatalf("Address = %q, want %q", settings.Address, test.want)
			}
		})
	}

	for _, value := range []string{"0", "65536", "invalid"} {
		t.Run("invalid_port_"+value, func(t *testing.T) {
			t.Setenv("GATEWAY_PORT", value)
			if _, err := Load(); err == nil || err.Error() != "GATEWAY_PORT must be an integer between 1 and 65535" {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

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

func TestLoadWorkers(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")

	t.Run("default", func(t *testing.T) {
		t.Setenv("CONVERSION_WORKERS", "")
		settings, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if settings.Workers != 2 {
			t.Fatalf("Workers = %d, want 2", settings.Workers)
		}
	})

	t.Run("custom", func(t *testing.T) {
		t.Setenv("CONVERSION_WORKERS", "4")
		settings, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if settings.Workers != 4 {
			t.Fatalf("Workers = %d, want 4", settings.Workers)
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

func TestLoadDocumentDPI(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")

	t.Run("default", func(t *testing.T) {
		t.Setenv("DOCUMENT_DPI", "")
		settings, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if settings.DocumentDPI != 300 {
			t.Fatalf("DocumentDPI = %d, want 300", settings.DocumentDPI)
		}
	})

	t.Run("custom", func(t *testing.T) {
		t.Setenv("DOCUMENT_DPI", "600")
		settings, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if settings.DocumentDPI != 600 {
			t.Fatalf("DocumentDPI = %d, want 600", settings.DocumentDPI)
		}
	})

	for _, value := range []string{"0", "1201", "invalid"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("DOCUMENT_DPI", value)
			if _, err := Load(); err == nil || err.Error() != "DOCUMENT_DPI must be an integer between 1 and 1200" {
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
