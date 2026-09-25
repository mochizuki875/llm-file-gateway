package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/config"
	"github.com/mochizuki875/llm-file-gateway/internal/converter"
	"github.com/mochizuki875/llm-file-gateway/internal/files"
	"github.com/mochizuki875/llm-file-gateway/internal/server"
	"github.com/mochizuki875/llm-file-gateway/internal/store"
)

// startGateway starts a real Gateway handler backed by a real file service and
// SQLite store. The upstream vLLM is mocked with an httptest server.
func startGateway(t *testing.T, upstreamURL string) (*httptest.Server, config.Config) {
	t.Helper()
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	settings := config.Config{
		DataDir: dataDir, FileTTL: 5 * time.Minute, MaxFileBytes: 50 * 1024 * 1024,
		MaxDocumentPages: 20, MaxDocumentTextChars: 500_000, Workers: 2,
		VLLMAPIKey: "upstream-key", VLLMModel: "test-model",
	}
	settings.VLLMBaseURL, _ = url.Parse(upstreamURL + "/v1")
	if err := os.MkdirAll(filepath.Join(dataDir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	service := files.New(settings, dataStore, converter.NewDispatcher(converter.NewInTreeRegistry(), converter.DefaultConverterConfig(), nil))
	ctx, cancel := context.WithCancel(context.Background())
	if err := service.Run(ctx, settings.Workers); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		service.Stop()
		_ = dataStore.Close()
	})
	gateway := httptest.NewServer(server.NewHandler(settings, dataStore, service))
	t.Cleanup(gateway.Close)
	return gateway, settings
}

// mockVLLM returns an httptest server that records the last request payload.
func mockVLLM(t *testing.T, received *map[string]any) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(received); err != nil {
			t.Error(err)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"id":"resp_1","object":"response"}`)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func uploadFile(t *testing.T, gatewayURL, filename string, content []byte) string {
	t.Helper()
	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	file, err := form.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("purpose", "user_data"); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/files", body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("upload status = %d: %s", response.StatusCode, body)
	}
	var created map[string]any
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	return created["id"].(string)
}

func waitProcessed(t *testing.T, gatewayURL, id string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		response, err := http.Get(gatewayURL + "/v1/files/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var current map[string]any
		decodeErr := json.NewDecoder(response.Body).Decode(&current)
		_ = response.Body.Close()
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		switch current["status"] {
		case "processed":
			return
		case "error":
			t.Fatalf("file failed: %v", current)
		}
		if time.Now().After(deadline) {
			t.Fatalf("file did not process: %v", current)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func sendResponses(t *testing.T, gatewayURL string, payload map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("responses status = %d: %s", response.StatusCode, body)
	}
}

// TestGatewayEndToEndTextFile uploads a text file, waits for conversion, and
// verifies that the expanded document text reaches the upstream vLLM.
func TestGatewayEndToEndTextFile(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	var received map[string]any
	upstream := mockVLLM(t, &received)
	gateway, _ := startGateway(t, upstream.URL)

	id := uploadFile(t, gateway.URL, "notes.txt", []byte("integration test content"))
	waitProcessed(t, gateway.URL, id)
	sendResponses(t, gateway.URL, map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_file", "file_id": id,
			}},
		}},
	})

	items := received["input"].([]any)
	content := items[0].(map[string]any)["content"].([]any)
	first := content[0].(map[string]any)
	if first["type"] != "input_text" || !strings.Contains(first["text"].(string), "integration test content") {
		t.Fatalf("expanded content = %#v", content)
	}
}

// TestGatewayEndToEndPDF uploads a real PDF, waits for conversion, and
// verifies that page images reach the upstream vLLM.
func TestGatewayEndToEndPDF(t *testing.T) {
	if testing.Short() {
		t.Skip("PDFium rendering is an integration test")
	}
	source := filepath.Join("..", "example", "samplefile.pdf")
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	var received map[string]any
	upstream := mockVLLM(t, &received)
	gateway, _ := startGateway(t, upstream.URL)

	id := uploadFile(t, gateway.URL, "samplefile.pdf", content)
	waitProcessed(t, gateway.URL, id)
	sendResponses(t, gateway.URL, map[string]any{
		"model": "test-model",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_file", "file_id": id,
			}},
		}},
	})

	items := received["input"].([]any)
	contentParts := items[0].(map[string]any)["content"].([]any)
	hasImage := false
	for _, part := range contentParts {
		if part.(map[string]any)["type"] == "input_image" {
			hasImage = true
		}
	}
	if !hasImage {
		t.Fatalf("no input_image in expanded content: %#v", contentParts)
	}
}

// TestGatewayEndToEndOffice uploads a real Office document, waits for
// conversion, and verifies that page images reach the upstream vLLM.
func TestGatewayEndToEndOffice(t *testing.T) {
	if testing.Short() {
		t.Skip("LibreOffice rendering is an integration test")
	}
	for _, name := range []string{"samplefile.docx", "samplefile.xlsx", "samplefile.pptx"} {
		t.Run(name, func(t *testing.T) {
			source := filepath.Join("..", "example", name)
			content, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			var received map[string]any
			upstream := mockVLLM(t, &received)
			gateway, _ := startGateway(t, upstream.URL)

			id := uploadFile(t, gateway.URL, name, content)
			waitProcessed(t, gateway.URL, id)
			sendResponses(t, gateway.URL, map[string]any{
				"model": "test-model",
				"input": []any{map[string]any{
					"role": "user",
					"content": []any{map[string]any{
						"type": "input_file", "file_id": id,
					}},
				}},
			})

			items := received["input"].([]any)
			contentParts := items[0].(map[string]any)["content"].([]any)
			hasImage := false
			for _, part := range contentParts {
				if part.(map[string]any)["type"] == "input_image" {
					hasImage = true
				}
			}
			if !hasImage {
				t.Fatalf("no input_image in expanded content: %#v", contentParts)
			}
		})
	}
}

// TestPDFSampleExtractionAndRendering verifies the PDF converter against the
// sample file using the public converter API.
func TestPDFSampleExtractionAndRendering(t *testing.T) {
	if testing.Short() {
		t.Skip("PDFium rendering is an integration test")
	}
	source := filepath.Join("..", "example", "samplefile.pdf")
	for _, test := range []struct {
		name        string
		disableText bool
		wantText    bool
	}{
		{name: "text_enabled", wantText: true},
		{name: "text_disabled", disableText: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "derived")
			result, err := convertSample(t, source, output, converter.Options{
				MaxPages: 20, MaxTextChars: 500_000, DisableTextExtraction: test.disableText,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Artifacts) != 2 {
				t.Fatalf("artifacts = %d, want 2", len(result.Artifacts))
			}
			if result.Artifacts[0].ImagePath == nil {
				t.Fatal("PDF artifact has no image")
			}
			textPath := result.Manifest.Documents[0].TextPath
			if got := textPath != ""; got != test.wantText {
				t.Fatalf("has text artifact = %t, want %t", got, test.wantText)
			}
			if textPath == "" {
				return
			}
			text, err := os.ReadFile(filepath.Join(output, textPath))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(text)) == "" {
				t.Fatal("PDF text artifact is empty")
			}
		})
	}
}

// TestOfficePluginSampleConversion verifies the Office converters against the
// sample files using the public converter API.
func TestOfficePluginSampleConversion(t *testing.T) {
	if testing.Short() {
		t.Skip("LibreOffice rendering is an integration test")
	}
	for _, name := range []string{
		"samplefile.doc", "samplefile.docx", "samplefile.ppt", "samplefile.pptx",
		"samplefile.xls", "samplefile.xlsx", "samplefile.xlsm",
	} {
		t.Run(name, func(t *testing.T) {
			source := filepath.Join("..", "example", name)
			output := filepath.Join(t.TempDir(), "derived")
			result, err := convertSample(t, source, output, converter.Options{MaxPages: 99, MaxTextChars: 500_000})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Artifacts) == 0 || result.Artifacts[0].ImagePath == nil {
				t.Fatalf("Office plugin produced no page image: %#v", result.Artifacts)
			}
			textPath := result.Manifest.Documents[0].TextPath
			content, err := os.ReadFile(filepath.Join(output, textPath))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(content)) == "" {
				t.Fatal("Office plugin produced no extracted text")
			}
			for _, artifact := range result.Artifacts {
				if artifact.TextPath != "" {
					t.Fatalf("Office image artifact unexpectedly references text: %#v", artifact)
				}
			}
		})
	}
}

func convertSample(t *testing.T, source, outputDir string, options converter.Options) (converter.Result, error) {
	t.Helper()
	documentConverter, err := converter.NewDispatcher(converter.NewInTreeRegistry(), converter.DefaultConverterConfig(), nil).ResolveConverter(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if err := documentConverter.Validate(source); err != nil {
		t.Fatal(err)
	}
	return documentConverter.Convert(context.Background(), source, outputDir, options)
}
