"""Official OpenAI Python SDK smoke test for Clyde's generic OpenAI listener.

TestOpenAISDKSmoke in openai_sdk_smoke_test.go starts the adapter with a
local Codex upstream and runs this script with the listener URL. Every
response and stream event is validated against the SDK's Pydantic models,
which reject a missing required field. The script records every failure
and exits nonzero when any check fails.
"""

from __future__ import annotations

import sys
from collections.abc import Callable

from openai import BadRequestError, NotFoundError, OpenAI
from openai.types import Completion, Model
from openai.types.chat import ChatCompletion, ChatCompletionChunk
from openai.types.responses import Response, ResponseStreamEvent
from pydantic import BaseModel, TypeAdapter, ValidationError

CODEX_MODEL = "gpt-future"
ADVERTISED_MODEL = "gpt-anthropic-alias"
PROMPT = "say ok"
EXPECTED_TEXT = "ok"
EXPECTED_REASONING_TOKENS = 7

RESPONSE_STREAM_EVENT: TypeAdapter[ResponseStreamEvent] = TypeAdapter(ResponseStreamEvent)


class SmokeRun:
    """Collects check failures across every resource."""

    def __init__(self) -> None:
        self.failures: list[str] = []

    def check(self, condition: bool, message: str) -> None:
        if not condition:
            self.failures.append(message)

    def validate(self, label: str, model: type[BaseModel], value: BaseModel) -> None:
        try:
            model.model_validate(value.to_dict())
        except ValidationError as error:
            self.failures.append(f"{label}: {error}")

    def validate_stream_event(self, label: str, value: BaseModel) -> None:
        # The SDK selects the event class from the type discriminator.
        # Validating against that class reports the missing fields of one
        # event instead of every union member.
        try:
            RESPONSE_STREAM_EVENT.validate_python(value.to_dict())
        except ValidationError:
            self.validate(label, type(value), value)


def smoke_models(client: OpenAI, run: SmokeRun) -> None:
    listed = client.models.list()
    for model in listed.data:
        run.validate("models.list entry", Model, model)
    retrieved = client.models.retrieve(ADVERTISED_MODEL)
    run.validate("models.retrieve", Model, retrieved)
    run.check(retrieved.id == ADVERTISED_MODEL, f"retrieved model id {retrieved.id}")
    try:
        client.models.retrieve("does-not-exist")
    except NotFoundError:
        return
    run.check(False, "missing model did not raise NotFoundError")


def smoke_chat(client: OpenAI, run: SmokeRun) -> None:
    completion = client.chat.completions.create(
        model=CODEX_MODEL,
        messages=[{"role": "user", "content": PROMPT}],
    )
    run.validate("chat.completions.create", ChatCompletion, completion)
    run.check(completion.choices[0].message.content == EXPECTED_TEXT, "chat text")
    usage = completion.usage
    details = usage.completion_tokens_details if usage is not None else None
    reasoning_tokens = details.reasoning_tokens if details is not None else None
    run.check(reasoning_tokens == EXPECTED_REASONING_TOKENS, f"chat reasoning tokens {reasoning_tokens}")

    stream = client.chat.completions.create(
        model=CODEX_MODEL,
        messages=[{"role": "user", "content": PROMPT}],
        stream=True,
        stream_options={"include_usage": True},
    )
    streamed_text = ""
    final_usage_seen = False
    for chunk in stream:
        run.validate("chat.completions stream chunk", ChatCompletionChunk, chunk)
        if not chunk.choices:
            final_usage_seen = chunk.usage is not None
            continue
        run.check(chunk.usage is None, "ordinary chat chunk has usage")
        streamed_text += chunk.choices[0].delta.content or ""
    run.check(streamed_text == EXPECTED_TEXT, f"chat stream text {streamed_text!r}")
    run.check(final_usage_seen, "chat stream final usage chunk missing")

    try:
        client.chat.completions.create(
            model=CODEX_MODEL,
            messages=[{"role": "user", "content": PROMPT}],
            temperature=0.2,
        )
    except BadRequestError as error:
        run.check(error.param == "temperature", f"rejected param {error.param}")
        return
    run.check(False, "unsupported temperature did not raise BadRequestError")


def smoke_responses(client: OpenAI, run: SmokeRun) -> None:
    response = client.responses.create(model=CODEX_MODEL, input=PROMPT)
    run.validate("responses.create", Response, response)
    run.check(response.output_text == EXPECTED_TEXT, f"responses text {response.output_text!r}")
    usage = response.usage
    details = usage.output_tokens_details if usage is not None else None
    reasoning_tokens = details.reasoning_tokens if details is not None else None
    run.check(reasoning_tokens == EXPECTED_REASONING_TOKENS, f"responses reasoning tokens {reasoning_tokens}")

    stream = client.responses.create(model=CODEX_MODEL, input=PROMPT, stream=True)
    event_types: list[str] = []
    for event in stream:
        run.validate_stream_event(f"responses stream {event.type}", event)
        event_types.append(event.type)
    run.check(bool(event_types) and event_types[0] == "response.created", f"first event {event_types[:1]}")
    run.check(bool(event_types) and event_types[-1] == "response.completed", f"last event {event_types[-1:]}")


def smoke_completions(client: OpenAI, run: SmokeRun) -> None:
    completion = client.completions.create(model=CODEX_MODEL, prompt=PROMPT)
    run.validate("completions.create", Completion, completion)
    run.check(completion.choices[0].text == EXPECTED_TEXT, "completion text")

    stream = client.completions.create(model=CODEX_MODEL, prompt=PROMPT, stream=True)
    streamed_text = ""
    for chunk in stream:
        # The SDK types finish_reason as non-null, and the stream reference
        # sends null before the last text chunk. Validation of an
        # intermediate chunk substitutes "stop" for the null finish_reason.
        payload = chunk.to_dict()
        for choice_payload in payload.get("choices", []):
            if choice_payload.get("finish_reason") is None:
                choice_payload["finish_reason"] = "stop"
        try:
            Completion.model_validate(payload)
        except ValidationError as error:
            run.check(False, f"completions stream chunk: {error}")
        for choice in chunk.choices:
            streamed_text += choice.text
    run.check(streamed_text == EXPECTED_TEXT, f"completion stream text {streamed_text!r}")


def main() -> int:
    base_url = sys.argv[1]
    client = OpenAI(base_url=base_url, api_key="clyde-smoke", max_retries=0)
    run = SmokeRun()
    resources: list[Callable[[OpenAI, SmokeRun], None]] = [
        smoke_models,
        smoke_chat,
        smoke_responses,
        smoke_completions,
    ]
    for resource in resources:
        resource(client, run)
    if run.failures:
        for failure in run.failures:
            print(f"FAIL {failure}")
        return 1
    print("openai sdk smoke passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
