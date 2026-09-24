# Project Guidelines

## Overview

LLM File Gateway is a Go gateway that provides an OpenAI Files API-compatible interface in front of a vLLM server. Uploaded files (PDF, Office, text, images) are converted asynchronously into extracted text plus base64 images, then expanded into Responses API / Chat Completions requests forwarded to vLLM.

- Module: `github.com/mochizuki875/llm-file-gateway` (Go 1.27+)
- Key dependency: `github.com/mochizuki875/document-image-renderer` (PDFium/WASM + LibreOffice for PDF/Office rendering)
- SQLite via `modernc.org/sqlite` (CGO-free)
- See `README.md` for user-facing docs and `DESIGN.md` for architecture details.

## Architecture

| Package | Responsibility |
| --- | --- |
| `cmd/llm-file-gateway` | Process lifecycle and HTTP server startup |
| `internal/config` | Environment variable validation (`config.Load`) |
| `internal/store` | SQLite schema and tenant-aware CRUD |
| `internal/files` | File storage, conversion queue, workers, janitor |
| `internal/converter` | Converter registry, dispatcher, pipeline, format converters and extractors |
| `internal/server` | Files/Responses/Chat APIs, file expansion, public URL fetch, vLLM proxy |
| `internal/apierror` | OpenAI-format errors |
| `test/` | Integration tests (real PDF/Office conversion, requires LibreOffice/PDFium) |

### Converter architecture

- All converters implement `DocumentConverter` (`Extension`, `MediaType`, `Validate`, `Convert`).
- `Registry` maps extensions to converters; `Dispatcher` resolves a converter by path and falls back to the shared `textConverter` for unknown extensions.
- Text formats use `extractor.Extractor` (`func(string) (string, error)`) in `internal/converter/extractor/`; every extractor must call `readUTF8` to validate UTF-8 and reject NUL bytes.
- PDF/Office rendering is delegated to `document-image-renderer` via `convertRenderedDocument`; images use `convertImage`; text uses `convertTextDocument`.
- Converters never write `manifest.json` directly — the shared pipeline in `pipeline.go` produces artifacts and the manifest.

## Build and Test

```bash
make build            # build to _output/llm-file-gateway
make test             # unit tests only (internal/... cmd/...), no external tools
make test-integration # integration tests (test/...), requires LibreOffice + fonts
make verify           # unit tests with -race + go vet
make run              # run the gateway
```

- `make test` and `make verify` intentionally exclude `test/` — integration tests are run separately via `make test-integration`.
- Tests that depend on external tools (LibreOffice, PDFium) must be guarded with `if testing.Short() { t.Skip(...) }` and live in `test/`.
- Unit tests mock vLLM with `httptest`; no GPU server is needed.

## Conventions

- Keep changes small and update related tests and files.
- When adding a format:
  1. Text-like format → add an `Extractor` in `internal/converter/extractor/` and register via `newTextConverter` in `text_common.go`.
  2. Image → use `validateImage`/`convertImage` from `image_common.go`.
  3. Rendering-based → use `convertRenderedDocument`.
  4. Register in `NewInTreeRegistry` (`registry.go`); duplicate extensions are an error.
  5. Add tests: `registry_test.go` (`TestInTreeRegistryContainsSupportedFormats`), `text_common_test.go` (`TestTextConverters`), and converter tests via `convertForTest`.
- Extensions are normalized to lowercase in the registry; register them lowercase with a leading dot.
- `Registry` is a `map[string]ConverterFactory` (Kubernetes-style). Factories receive `ConverterConfig` (DPI, image format, LibreOffice timeout) and a `ConverterHandle`; use `ConverterAdapter` for stateless in-tree converters.
- For changes affecting conversion output, run the PDF and each Office integration test, considering LibreOffice, font, and `document-image-renderer` version differences.
- Do not log file contents or API keys explicitly.
- `file_url` must be HTTPS on port 443 with a public IP, re-validated on each redirect.
