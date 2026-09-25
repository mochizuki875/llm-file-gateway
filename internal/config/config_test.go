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

func TestLoadMaxDocumentPages(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")

	for _, test := range []struct {
		name  string
		value string
		want  int
	}{
		{name: "default", want: 50},
		{name: "custom", value: "99", want: 99},
		{name: "unlimited", value: "0", want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("MAX_DOCUMENT_PAGES", test.value)
			settings, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if settings.MaxDocumentPages != test.want {
				t.Fatalf("MaxDocumentPages = %d, want %d", settings.MaxDocumentPages, test.want)
			}
		})
	}

	for _, value := range []string{"-1", "invalid"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("MAX_DOCUMENT_PAGES", value)
			if _, err := Load(); err == nil || err.Error() != "MAX_DOCUMENT_PAGES must be a non-negative integer" {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadMaxRequestBodyBytes(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")

	for _, test := range []struct {
		name  string
		value string
		want  int64
	}{
		{name: "default_four_times_file", want: 4 * 50 * 1024 * 1024},
		{name: "custom", value: "104857600", want: 104857600},
		{name: "unlimited", value: "0", want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("MAX_REQUEST_BODY_BYTES", test.value)
			settings, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if settings.MaxRequestBodyBytes != test.want {
				t.Fatalf("MaxRequestBodyBytes = %d, want %d", settings.MaxRequestBodyBytes, test.want)
			}
		})
	}

	for _, value := range []string{"-1", "invalid"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("MAX_REQUEST_BODY_BYTES", value)
			if _, err := Load(); err == nil || err.Error() != "MAX_REQUEST_BODY_BYTES must be a non-negative integer" {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadMaxRequestBodyBytesUsesConfiguredFileLimitByDefault(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")
	t.Setenv("MAX_FILE_BYTES", "104857600")
	t.Setenv("MAX_REQUEST_BODY_BYTES", "")

	settings, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.MaxRequestBodyBytes != 4*104857600 {
		t.Fatalf("MaxRequestBodyBytes = %d, want %d", settings.MaxRequestBodyBytes, 4*104857600)
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

	t.Run("unlimited", func(t *testing.T) {
		t.Setenv("FILE_TTL_SECONDS", "0")
		settings, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if settings.FileTTL != 0 {
			t.Fatalf("FileTTL = %s, want 0 (unlimited)", settings.FileTTL)
		}
	})

	for _, value := range []string{"-1", "invalid"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("FILE_TTL_SECONDS", value)
			if _, err := Load(); err == nil || err.Error() != "FILE_TTL_SECONDS must be a non-negative integer" {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadMaxFileBytes(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")

	for _, test := range []struct {
		name  string
		value string
		want  int64
	}{
		{name: "default", want: 50 * 1024 * 1024},
		{name: "custom", value: "1048576", want: 1048576},
		{name: "unlimited", value: "0", want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("MAX_FILE_BYTES", test.value)
			settings, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if settings.MaxFileBytes != test.want {
				t.Fatalf("MaxFileBytes = %d, want %d", settings.MaxFileBytes, test.want)
			}
		})
	}

	for _, value := range []string{"-1", "invalid"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("MAX_FILE_BYTES", value)
			if _, err := Load(); err == nil || err.Error() != "MAX_FILE_BYTES must be a non-negative integer" {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadMaxDocumentTextChars(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")

	for _, test := range []struct {
		name  string
		value string
		want  int
	}{
		{name: "default", want: 500_000},
		{name: "custom", value: "1000", want: 1000},
		{name: "unlimited", value: "0", want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("MAX_DOCUMENT_TEXT_CHARS", test.value)
			settings, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if settings.MaxDocumentTextChars != test.want {
				t.Fatalf("MaxDocumentTextChars = %d, want %d", settings.MaxDocumentTextChars, test.want)
			}
		})
	}

	for _, value := range []string{"-1", "invalid"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("MAX_DOCUMENT_TEXT_CHARS", value)
			if _, err := Load(); err == nil || err.Error() != "MAX_DOCUMENT_TEXT_CHARS must be a non-negative integer" {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadRequestTimeout(t *testing.T) {
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://vllm.test/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")

	for _, test := range []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "default", want: 300 * time.Second},
		{name: "custom", value: "60", want: 60 * time.Second},
		{name: "unlimited", value: "0", want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("REQUEST_TIMEOUT_SECONDS", test.value)
			settings, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if settings.RequestTimeout != test.want {
				t.Fatalf("RequestTimeout = %s, want %s", settings.RequestTimeout, test.want)
			}
		})
	}

	for _, value := range []string{"-1", "invalid"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("REQUEST_TIMEOUT_SECONDS", value)
			if _, err := Load(); err == nil || err.Error() != "REQUEST_TIMEOUT_SECONDS must be non-negative" {
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
