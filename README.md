# LLM File Gateway

![](images/logo.png)

> [!WARNING]
> 本プロジェクトはMVP実装です。評価・開発用途に使用してください。

[vLLMのAPI](https://docs.vllm.ai/en/stable/serving/online_serving/)を[OpenAI Files API](https://developers.openai.com/api/reference/resources/files)に対応させるGatewayです。
PDF、Office文書、テキスト、画像などのファイルをOpenAI Files API互換のAPIでアップロードすると画像変換およびテキスト抽出が行われ、`file_id`が生成されます。
生成された`file_id`をResponses APIまたはChat Completions APIに付加することで、バックエンドにFiles APIでアップロードしたファイルから変換された画像(base64)およびテキストを転送することができます。

文書画像変換には[document-image-renderer](https://github.com/mochizuki875/document-image-renderer)を使用します。

## Features
- OpenAI互換のFiles API
- Responses API、Chat Completions APIへの`file_id`、base64の`file_data`、公開HTTPSの`file_url`によるファイル入力
- `GATEWAY_API_KEY`によるGatewayでのToken認証
  - ただし複数の`GATEWAY_API_KEY`による認証(マルチテナント)には未対応

## Supported APIs

| API | Endpoint |
| --- | --- |
| Health | `GET /health` |
| Create file | `POST /v1/files` |
| List files | `GET /v1/files` |
| Retrieve file | `GET /v1/files/{file_id}` |
| Retrieve content | `GET /v1/files/{file_id}/content` |
| Delete file | `DELETE /v1/files/{file_id}` |
| Responses | `POST /v1/responses` |
| Chat Completions | `POST /v1/chat/completions` |
| Other vLLM APIs | `/v1/*` (Passed through)

Files APIが所有するパスの未対応メソッドはvLLMへ転送せず、`405`を返します。

## Supported Formats

| 種別 | 拡張子 | モデルへの入力 |
| --- | --- | --- |
| PDF | `.pdf` | 文書全体の抽出テキストとページ画像を生成し、モデルへの入力として使用 |
| Word | `.doc`, `.docx` | 文書全体の抽出テキストとページ画像を生成し、モデルへの入力として使用 |
| PowerPoint | `.ppt`, `.pptx` | 文書全体の抽出テキストとスライド画像を生成し、モデルへの入力として使用 |
| Excel | `.xls`, `.xlsx`, `.xlsm` | 文書全体の抽出テキストとシート画像を生成し、モデルへの入力として使用 |
| Text | `.txt`, `.md`, `.markdown`, `.json`, `.jsonl`, `.yaml`, `.yml`, `.go`など | UTF-8の内容をテキストとして生成し、モデルへの入力として使用 |
| Structured text | `.csv`, `.html`, `.htm` | CSVを行形式に変換、HTMLから可視テキストを抽出しテキストを生成し、モデルへの入力として使用 |
| Image | `.jpeg`, `.jpg`, `.png` | 元形式の画像をそのままモデルへの入力として使用 |

PDFとOffice形式ファイルのテキスト抽出および画像変換は`document-image-renderer`へ委譲します。Office文書のマクロは実行しません。

`DOCUMENT_TEXT_EXTRACTION_ENABLED=false`にすると、PDFとOfficeは画像だけをモデルへ送ります。

テキスト形式は個別pluginを持たず、共通text converterで処理します。(CSVとHTMLだけは同じconverterへ抽出関数を設定)。未知の拡張子や拡張子がないファイルも、有効なUTF-8でNUL byteを含まなければプレーンテキストとして処理します。

## Requirements

- Go 1.27以降
- OpenAI互換APIを提供するvLLMサーバー
- PDF、Office、画像を使う場合はマルチモーダル対応モデル
- [document-image-renderer](https://github.com/mochizuki875/document-image-renderer)の動作要件も確認してください

PDF描画はPDFium/WASMを使用するため、CGOや外部PDFコマンドは不要です。Office文書の安定したレイアウトには、文書で使用されるフォントも必要です。Gatewayは特定モデル専用ではありませんが、画像数、context長、chat templateなどの制約は利用するモデルとvLLM構成に依存します。

## Build

```bash
git clone https://github.com/mochizuki875/llm-file-gateway.git
make build
```

## Environment Setup

```bash
cp .env.example .env
```
`.env`へ少なくとも`VLLM_MODEL`と`VLLM_BASE_URL`を設定してください。`.env`はGit管理対象外です。

## Configuration

Gatewayは設定値をプロセスの環境変数から読み取ります。

| Environment variable | Required | Default | Description |
| --- | --- | --- | --- |
| `VLLM_MODEL` | Yes | - | クライアント要求で許可するモデル名 |
| `VLLM_BASE_URL` | Yes | - | `/v1`で終わるvLLMの絶対URL |
| `GATEWAY_ADDRESS` | No | `:8080` | Listen address |
| `GATEWAY_AUTH_REQUIRED` | No | `false` | GatewayでBearer tokenを検証するか |
| `GATEWAY_API_KEY` | Conditional | - | Gateway用キー |
| `VLLM_API_KEY` | Yes | - | vLLM転送用キー |
| `GATEWAY_DATA_DIR` | No | `gateway-data` | SQLiteとファイルの保存先 |
| `FILE_TTL_SECONDS` | No | `300` | ファイル保持秒数。正の整数で変更可能 |
| `MAX_FILE_BYTES` | No | `52428800` | 1ファイルの最大サイズ |
| `MAX_DOCUMENT_PAGES` | No | `20` | 画像変換する最大ページ数 |
| `MAX_DOCUMENT_IMAGES` | No | `8` | 1文書からvLLMへ送る最大画像数 |
| `MAX_DOCUMENT_TEXT_CHARS` | No | `500000` | 1文書から抽出するテキストの最大文字数（PDF、Officeを含む） |
| `DOCUMENT_TEXT_EXTRACTION_ENABLED` | No | `true` | PDFとOfficeからテキストを抽出するか |
| `CONVERSION_WORKERS` | No | `2` | 並行して文書を変換するworker数。正の整数で変更可能 |
| `REQUEST_TIMEOUT_SECONDS` | No | `300` | vLLM通信のtimeout。streamingではstream全体に適用 |
| `LOGLEVEL` | No | `0` | ログverbosity（`0`: 通常、`1`: DEBUG、`2`: 高頻度の詳細ログ） |

`GATEWAY_AUTH_REQUIRED=true`を設定した場合はGatewayでの認証が有効となり、`GATEWAY_API_KEY`の設定が必須となります。
GatewayからvLLMへの認証は`VLLM_API_KEY`を用いて行われるため、Gatewayに送信された`OPENAI_API_KEY`は転送されません。(`VLLM_API_KEY`は常に必須です。)
`GET /health`は認証対象外です。

## Running the Gateway

`set -a`により、`.env`で定義した値を子プロセスの環境変数としてexportします。
別のterminalから起動状態を確認します。

```bash
set -a
. ./.env
set +a

go run ./cmd/llm-file-gateway
```

Gatewayにリクエストを送信します。
`{"status":"ok"}`が返ればGatewayは起動しています。`GET /health`はプロセスの稼働だけを示し、SQLite、LibreOffice、vLLMへの接続性は検査しません。

```bash
# Check the health of the Gateway
curl -sS --fail http://localhost:8080/health

# Check the available models from the Gateway
if [[ "${GATEWAY_AUTH_REQUIRED:-false}" == "true" ]]; then
  CLIENT_API_KEY="$GATEWAY_API_KEY"
else
  CLIENT_API_KEY="$VLLM_API_KEY"
fi
curl -sS --fail http://localhost:8080/v1/models \
  -H "Authorization: Bearer $CLIENT_API_KEY" | jq .
```

## Docker

公開ポートは`GATEWAY_PORT=18080 docker compose up --build -d`のように変更できます。

Composeはカレントディレクトリの`.env`を展開してコンテナへ渡します。保存データを永続化するvolumeは既定のCompose構成に含まれないため、コンテナを削除するとSQLiteと保存ファイルも失われます。

```bash
docker compose up --build -d
docker compose ps
curl --fail http://localhost:8080/health
docker compose down
```

## Usage
クライアントからはGatewayをOpenAI互換APIの接続先として使用します。

環境変数を設定します。
```bash
set -a
. ../.env
set +a

export OPENAI_BASE_URL=http://localhost:8080/v1
if [[ "${GATEWAY_AUTH_REQUIRED:-false}" == "true" ]]; then
  export OPENAI_API_KEY="$GATEWAY_API_KEY"
else
  export OPENAI_API_KEY=unused
fi
export OPENAI_MODEL="$VLLM_MODEL"
```

### Python Client Example

仮想環境を作成し有効化します。
```bash
cd example
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
```

Pythonスクリプトを実行します。
```bash
python openai_file_summary.py
# Streaming response
python openai_file_summary_stream.py
```

### curl

curlコマンドを用いてファイルのアップロード、変換完了待ち、Responses APIによる要約、削除を順に実行します。
```bash
cd example
DOCUMENT=samplefile.docx

FILE_ID=$(curl --fail --silent "$OPENAI_BASE_URL/files" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -F purpose=user_data \
  -F "file=@$DOCUMENT" | jq -r .id)
echo "Uploaded: $FILE_ID"

while true; do
  FILE_STATUS=$(curl --fail --silent "$OPENAI_BASE_URL/files/$FILE_ID" \
    -H "Authorization: Bearer $OPENAI_API_KEY" | jq -r .status)
  [[ "$FILE_STATUS" == "processed" ]] && break
  [[ "$FILE_STATUS" == "error" ]] && { echo "Conversion failed" >&2; exit 1; }
  sleep 1
done

curl --fail --silent "$OPENAI_BASE_URL/responses" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$OPENAI_MODEL\",\"input\":[{\"role\":\"user\",\"content\":[{\"type\":\"input_file\",\"file_id\":\"$FILE_ID\"},{\"type\":\"input_text\",\"text\":\"この文書を簡潔に要約してください。\"}]}]}" | \
  jq '{id, status, output_text: [.output[].content[] | select(.type == "output_text").text] | join("")}'

curl --fail --silent -X DELETE "$OPENAI_BASE_URL/files/$FILE_ID" \
  -H "Authorization: Bearer $OPENAI_API_KEY"
```

### Input Forms

| API | Input | Shape |
| --- | --- | --- |
| Responses | Uploaded file | `{"type":"input_file","file_id":"file_..."}` |
| Responses | Inline base64 | `{"type":"input_file","filename":"document.pdf","file_data":"..."}` |
| Responses | Public URL | `{"type":"input_file","file_url":"https://example.com/document.pdf"}` |
| Chat Completions | Uploaded file | `{"type":"file","file":{"file_id":"file_..."}}` |
| Chat Completions | Inline base64 | `{"type":"file","file":{"filename":"document.pdf","file_data":"..."}}` |

一つの参照には`file_id`、`file_data`、`file_url`のいずれか一つだけを指定します。Chat Completionsでは`file_url`を使用できません。

## Limits and Compatibility

- 1ファイルの上限はデフォルト50 MiBです。inlineデータとURL取得にも同じ`MAX_FILE_BYTES`を適用します。
- PDF/Officeの最大ページ数は`MAX_DOCUMENT_PAGES`、vLLMへ送る画像数は文書ごとに`MAX_DOCUMENT_IMAGES`で制限します。画像上限後のページも抽出テキストは送信します。
- 抽出テキストはPDF、Office、すべてのテキスト形式を含めて`MAX_DOCUMENT_TEXT_CHARS`で制限します。
- Files APIの`purpose`は`user_data`だけを受け付けます。`expires_after`を指定する場合は`{"anchor":"created_at","seconds":<FILE_TTL_SECONDS>}`と一致させる必要があります。
- Files APIの変換は非同期です。推論で参照する前に`status: "processed"`を確認してください。変換中は`409 file_not_ready`、失敗後は`422 file_processing_failed`を返します。
- Responses APIの`previous_response_id`はそのままvLLMへ転送するため、利用可否はvLLM側の対応と設定に依存します。
- `stream: true`ではvLLMのSSE eventとresponse headerを逐次転送します。
- Responsesの文字列形式`input`にファイルを追加することはできません。Chat Completionsのファイルは配列形式の`content`内で展開します。
- JPEG/PNGは再圧縮しません。通常のテキスト形式は元テキストを保持し、HTMLは外部URLや埋め込み画像を取得しません。
- Microsoft OfficeとLibreOfficeではフォント置換、レイアウト、改ページが異なる場合があります。
- OpenAI内部の文書変換、token使用量、回答品質との完全な一致は保証しません。
- content part以外のJSON fieldは同種のvLLM APIへベストエフォートで転送します。実際の対応範囲はvLLM、モデル、chat template、tool parserに依存します。


### Adding a Format

専用の検証、解析、画像化が必要な形式を追加する場合は`internal/converter.DocumentConverter`を実装し、`registry.go`の標準registryへ登録します。一つのplugin instanceは`Extension() string`で一つの拡張子だけを所有しますが、同じ実装を複数拡張子へ再利用できます。dispatcher、Files service、推論resolverへ形式別分岐を追加しないことが設計上の制約です。単にUTF-8テキストとして渡す形式は、未知拡張子向けfallbackで処理されるため登録不要です。

共通処理は次の単位で再利用します。

- `convertRenderedDocument`: PDF/Officeの画像化とpage単位artifact
- `convertTextDocument`: text上限とtext-only artifact
- `convertImageDocument`: 元画像を保持するimage artifact
- `writeResult`: schema version 3のmanifest生成

PDF/Office変換では`document-image-renderer`の`renderer.RenderDocument`をLibreOffice timeout 300秒で呼び出し、画像は150 DPIのPNGとして生成します。`DOCUMENT_TEXT_EXTRACTION_ENABLED=true`の場合は`renderer.ExtractDocumentWithOptions`も呼び出します。Gatewayは変換ごとに専有する`derived` directoryと`manifest.json`を初期化し、途中失敗時は両方を削除します。

Files APIの変換はprocess内queueと`CONVERSION_WORKERS`個のworkerで非同期実行されます。起動時にはSQLite上で中断されたjobをqueueへ戻し、janitorは別goroutineで期限切れfileを削除します。

## Testing

```bash
make test
make verify
make test-integration
```

`make test`は外部rendererを使うケースを除く短縮testです。`make verify`は同じtestをrace detector付きで実行し、続けて`go vet`を実行します。どちらもvLLMをmockするためGPU serverは不要です。

`make test-integration`はPDF/Officeの実変換を含み、LibreOfficeと必要なフォントが必要です。外部vLLMを使うE2E確認には[Python Client Example](#python-client-example)を使用できます。

## Security

- 登録済み拡張子の基本signature、テキストのUTF-8とNUL byte、画像形式を検証します。
- `file_url`はHTTPSの443番ポートと公開IPだけを許可し、redirectごとに再検証します。
- 保存ファイルは`FILE_TTL_SECONDS`後（既定5分）に削除します。Gateway認証無効時の保存領域は全クライアントで共有されます。
- 文書由来テキストを信頼しないようsystem instructionを追加します。
- ログへ文書本文やAPI keyを明示的には出力しません。
- `.env`と`gateway-data/`はGit管理対象外です。

認証無効時はAuthorizationをFiles APIの所有範囲に使用せず、全リクエストが同じ保存領域を共有します。必須認証ではFiles APIを含む`/v1`全体でGateway keyを検証します。`GET /health`は認証対象外です。

単一GatewayとローカルSQLiteを前提とします。conversion worker数はprocess内で変更できますが、複数replica間でqueueは共有しません。parserの完全なsandbox、rate limit、malware scan、保存時暗号化、TLS終端、高可用化は含みません。本番公開時はreverse proxyでTLS、追加認証、rate limit、request size制限を適用し、必要に応じて文書変換を隔離してください。

## Contributing

変更は小さく保ち、関連するtestと文書を更新してください。変換結果に影響する変更では、PDFと各Office形式のintegration testを実行し、LibreOffice、フォント、`document-image-renderer`のversion差も考慮してください。

詳細は[DESIGN.md](DESIGN.md)を参照してください。

## License

Apache License 2.0。全文は[LICENSE](LICENSE)を参照してください。