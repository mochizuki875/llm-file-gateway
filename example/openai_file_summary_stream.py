from __future__ import annotations

import os
import time
from pathlib import Path

from openai import OpenAI

DOCUMENT_PATH = Path(__file__).with_name("samplefile.xlsx")
POLL_INTERVAL_SECONDS = 0.5
PROCESSING_TIMEOUT_SECONDS = 300.0
SUMMARY_PROMPT = "この内容を日本語で簡潔に要約してください。"
MAX_OUTPUT_TOKENS = int(os.getenv("OPENAI_MAX_OUTPUT_TOKENS", "253952"))


def required_environment(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


def wait_until_processed(client: OpenAI, file_id: str) -> None:
    deadline = time.monotonic() + PROCESSING_TIMEOUT_SECONDS

    while time.monotonic() < deadline:
        remote_file = client.files.retrieve(file_id)

        if remote_file.status == "processed":
            return

        if remote_file.status == "error":
            raise RuntimeError(f"File processing failed: {file_id}")

        time.sleep(POLL_INTERVAL_SECONDS)

    raise TimeoutError(f"File processing timed out: {file_id}")


def main() -> None:
    if not DOCUMENT_PATH.is_file():
        raise RuntimeError(f"Document not found: {DOCUMENT_PATH}")

    client = OpenAI(
        base_url=required_environment("OPENAI_BASE_URL"),
        api_key=required_environment("OPENAI_API_KEY"),
    )

    model = required_environment("OPENAI_MODEL")
    uploaded_file = None

    try:
        with DOCUMENT_PATH.open("rb") as document:
            uploaded_file = client.files.create(
                file=document,
                purpose="user_data",
            )

        print(f"Uploaded: {uploaded_file.id}")

        wait_until_processed(client, uploaded_file.id)
        print(f"Processed: {uploaded_file.id}")

        stream = client.responses.create(
            model=model,
            input=[
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "input_file",
                            "file_id": uploaded_file.id,
                        },
                        {
                            "type": "input_text",
                            "text": SUMMARY_PROMPT,
                        },
                    ],
                }
            ],
            max_output_tokens=MAX_OUTPUT_TOKENS,
            stream=True,
        )

        received_output = False

        for event in stream:
            if event.type == "response.output_text.delta":
                received_output = True
                print(event.delta, end="", flush=True)

            elif event.type == "response.failed":
                raise RuntimeError(
                    f"The Responses API request failed: {event.response.error}"
                )

        print()

        if not received_output:
            raise RuntimeError("The Responses API returned no output text")

    finally:
        if uploaded_file is not None:
            client.files.delete(uploaded_file.id)
            print(f"Deleted: {uploaded_file.id}")


if __name__ == "__main__":
    main()
