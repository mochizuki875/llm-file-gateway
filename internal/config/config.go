package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultFileTTL is the default file retention period used when
// FILE_TTL_SECONDS is not set.
const defaultFileTTL = 5 * time.Minute

// Config holds all validated gateway settings loaded from environment variables.
type Config struct {
	Address               string
	VLLMModel             string
	VLLMBaseURL           *url.URL
	GatewayAuthRequired   bool
	GatewayAPIKey         string
	VLLMAPIKey            string
	DataDir               string
	FileTTL               time.Duration
	MaxFileBytes          int64
	MaxDocumentPages      int
	MaxDocumentImages     int
	MaxDocumentTextChars  int
	DocumentDPI           int
	TextExtractionEnabled bool
	Workers               int
	RequestTimeout        time.Duration
	LogVerbosity          int
}

// Load reads and validates the gateway configuration from environment
// variables, applying defaults where a variable is unset.
func Load() (Config, error) {
	model := os.Getenv("VLLM_MODEL")
	if model == "" {
		return Config{}, fmt.Errorf("VLLM_MODEL is required")
	}

	baseURL, err := url.Parse(os.Getenv("VLLM_BASE_URL"))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || strings.TrimRight(baseURL.Path, "/") != "/v1" {
		return Config{}, fmt.Errorf("VLLM_BASE_URL must be an absolute URL ending with /v1")
	}

	authRequired, err := envBool("GATEWAY_AUTH_REQUIRED", false)
	if err != nil {
		return Config{}, err
	}
	gatewayHost := envString("GATEWAY_HOST", "127.0.0.1")
	gatewayPort, err := envInt("GATEWAY_PORT", 8080)
	if err != nil || gatewayPort < 1 || gatewayPort > 65535 {
		return Config{}, fmt.Errorf("GATEWAY_PORT must be an integer between 1 and 65535")
	}

	fileTTLSeconds, err := envInt64("FILE_TTL_SECONDS", int64(defaultFileTTL/time.Second))
	if err != nil || fileTTLSeconds < 1 {
		return Config{}, fmt.Errorf("FILE_TTL_SECONDS must be a positive integer")
	}

	maxFileBytes, err := envInt64("MAX_FILE_BYTES", 50*1024*1024)
	if err != nil || maxFileBytes < 1 {
		return Config{}, fmt.Errorf("MAX_FILE_BYTES must be a positive integer")
	}
	maxPages, err := envInt("MAX_DOCUMENT_PAGES", 50)
	if err != nil || maxPages < 1 {
		return Config{}, fmt.Errorf("MAX_DOCUMENT_PAGES must be a positive integer")
	}
	maxImages, err := envInt("MAX_DOCUMENT_IMAGES", 8)
	if err != nil || maxImages < 1 {
		return Config{}, fmt.Errorf("MAX_DOCUMENT_IMAGES must be a positive integer")
	}
	maxTextChars, err := envInt("MAX_DOCUMENT_TEXT_CHARS", 500_000)
	if err != nil || maxTextChars < 1 {
		return Config{}, fmt.Errorf("MAX_DOCUMENT_TEXT_CHARS must be a positive integer")
	}
	documentDPI, err := envInt("DOCUMENT_DPI", 300)
	if err != nil || documentDPI < 1 || documentDPI > 1200 {
		return Config{}, fmt.Errorf("DOCUMENT_DPI must be an integer between 1 and 1200")
	}
	textExtractionEnabled, err := envBool("DOCUMENT_TEXT_EXTRACTION_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	workers, err := envInt("CONVERSION_WORKERS", 2)
	if err != nil || workers < 1 {
		return Config{}, fmt.Errorf("CONVERSION_WORKERS must be a positive integer")
	}
	timeoutSeconds, err := envFloat("REQUEST_TIMEOUT_SECONDS", 300)
	if err != nil || timeoutSeconds <= 0 {
		return Config{}, fmt.Errorf("REQUEST_TIMEOUT_SECONDS must be positive")
	}
	logVerbosity, err := envInt("LOGLEVEL", 0)
	if err != nil || logVerbosity < 0 || logVerbosity > 2 {
		return Config{}, fmt.Errorf("LOGLEVEL must be an integer between 0 and 2")
	}

	config := Config{
		Address:               net.JoinHostPort(gatewayHost, strconv.Itoa(gatewayPort)),
		VLLMModel:             model,
		VLLMBaseURL:           baseURL,
		GatewayAuthRequired:   authRequired,
		GatewayAPIKey:         os.Getenv("GATEWAY_API_KEY"),
		VLLMAPIKey:            os.Getenv("VLLM_API_KEY"),
		DataDir:               envString("GATEWAY_DATA_DIR", "gateway-data"),
		FileTTL:               time.Duration(fileTTLSeconds) * time.Second,
		MaxFileBytes:          maxFileBytes,
		MaxDocumentPages:      maxPages,
		MaxDocumentImages:     maxImages,
		MaxDocumentTextChars:  maxTextChars,
		DocumentDPI:           documentDPI,
		TextExtractionEnabled: textExtractionEnabled,
		Workers:               workers,
		RequestTimeout:        time.Duration(timeoutSeconds * float64(time.Second)),
		LogVerbosity:          logVerbosity,
	}
	if config.VLLMAPIKey == "" {
		return Config{}, fmt.Errorf("VLLM_API_KEY is required")
	}
	if config.GatewayAuthRequired && config.GatewayAPIKey == "" {
		return Config{}, fmt.Errorf("GATEWAY_API_KEY is required when GATEWAY_AUTH_REQUIRED=true")
	}
	return config, nil
}

// envString returns the value of the named environment variable, or the
// fallback when it is unset or empty.
func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// envBool parses the named environment variable as a boolean, using the
// fallback when it is unset.
func envBool(name string, fallback bool) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return parsed, nil
}

// envInt parses the named environment variable as an int, using the fallback
// when it is unset.
func envInt(name string, fallback int) (int, error) {
	value, err := envInt64(name, int64(fallback))
	return int(value), err
}

// envInt64 parses the named environment variable as an int64, using the
// fallback when it is unset.
func envInt64(name string, fallback int64) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

// envFloat parses the named environment variable as a float64, using the
// fallback when it is unset.
func envFloat(name string, fallback float64) (float64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number", name)
	}
	return parsed, nil
}
