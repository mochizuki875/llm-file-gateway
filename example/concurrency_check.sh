#!/usr/bin/env bash
# Observe the MAX_CONCURRENT_REQUESTS limit: fire more concurrent inference
# requests than the configured limit and show that the excess requests are
# rejected immediately with 429 too_many_requests, while the accepted ones
# keep their slots until response headers are sent.
#
# Usage (from example/):
#   set -a; source ../.env; set +a
#   export OPENAI_BASE_URL="http://localhost:8080/v1"
#   export OPENAI_MODEL="$VLLM_MODEL"
#   ./concurrency_check.sh
#
#   # More requests or a different document:
#   CONCURRENCY=10 DOCUMENT=samplefile.txt ./concurrency_check.sh
#
# Requires: curl, jq. The uploaded file expires via FILE_TTL_SECONDS.
set -euo pipefail

command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

: "${OPENAI_BASE_URL:?OPENAI_BASE_URL is required (e.g. http://localhost:8080/v1)}"
: "${OPENAI_API_KEY:?OPENAI_API_KEY is required}"
: "${OPENAI_MODEL:?OPENAI_MODEL is required}"

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
DOCUMENT=${DOCUMENT:-$SCRIPT_DIR/samplefile.xlsm}
CONCURRENCY=${CONCURRENCY:-10} # MAX_CONCURRENT_REQUESTS + 1 by default

if [[ ! -f $DOCUMENT ]]; then
  echo "Document not found: $DOCUMENT" >&2
  exit 1
fi

echo "gateway:     $OPENAI_BASE_URL"
echo "model:       $OPENAI_MODEL"
echo "document:    $DOCUMENT"
echo "concurrency: $CONCURRENCY"
echo

# Upload once; every request reuses the same file so conversion runs once and
# the waiting requests hold their slots, which widens the observation window.
FILE_ID=$(curl --fail --silent "$OPENAI_BASE_URL/files" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -F purpose=user_data \
  -F "file=@$DOCUMENT" | jq -r .id)
echo "Uploaded: $FILE_ID"
echo

PAYLOAD=$(jq -n --arg model "$OPENAI_MODEL" --arg file_id "$FILE_ID" '{
  model: $model,
  input: [{role: "user", content: [
    {type: "input_file", file_id: $file_id},
    {type: "input_text", text: "Summarize this document concisely."}
  ]}]
}')

STATUSES=$(mktemp)
trap 'rm -f "$STATUSES"' EXIT

request() {
  local index=$1 body status text
  if ! body=$(curl --silent --show-error --max-time 120 \
      -w '\n%{http_code}' "$OPENAI_BASE_URL/responses" \
      -H "Authorization: Bearer $OPENAI_API_KEY" \
      -H 'Content-Type: application/json' \
      -d "$PAYLOAD"); then
    printf '#%s -> curl error\n' "$index"
    echo curl_error >>"$STATUSES"
    return
  fi
  status=${body##*$'\n'}
  body=${body%$'\n'*}
  case $status in
    429)
      printf '#%s -> %s %s\n' "$index" "$status" \
        "$(jq -c '{code: .error.code, message: .error.message}' <<<"$body")"
      ;;
    200)
      text=$(jq -r '[.output[].content[] | select(.type == "output_text").text] | join("")' <<<"$body")
      printf '#%s -> %s %.60s...\n' "$index" "$status" "$text"
      ;;
    *)
      printf '#%s -> %s %.120s\n' "$index" "$status" "$body"
      ;;
  esac
  echo "$status" >>"$STATUSES"
}

# Fire all requests at once. The first MAX_CONCURRENT_REQUESTS acquire slots;
# the rest are rejected with 429 before reaching the handler.
for index in $(seq 1 "$CONCURRENCY"); do
  request "$index" &
done
wait

echo
echo "Summary:"
sort "$STATUSES" | uniq -c | sed 's/^/  /'
