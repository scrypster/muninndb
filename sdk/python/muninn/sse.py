"""Async SSE (Server-Sent Events) stream implementation with auto-reconnect."""

import asyncio
import json

import httpx

from .errors import MuninnConnectionError
from .types import Push, PushEngram


class SSEStream:
    """Async SSE stream with automatic reconnection and Last-Event-ID tracking.

    Usage:
        stream = client.subscribe(vault="default")
        async for push in stream:
            print(push.engram_id)
            if should_stop:
                await stream.close()
    """

    def __init__(self, client_ref, url: str, params: dict):
        self._client = client_ref
        self._url = url
        self._params = params
        self._last_event_id: str | None = None
        self._closed = False

    async def close(self):
        """Close the SSE stream."""
        self._closed = True

    def __aiter__(self):
        """Iterate over SSE events."""
        return self._stream()

    async def _stream(self):
        """Internal stream loop with automatic reconnection."""
        backoff = 0.5
        while not self._closed:
            try:
                headers = {"Accept": "text/event-stream"}
                if self._last_event_id:
                    headers["Last-Event-ID"] = self._last_event_id
                if self._client._token:
                    headers["Authorization"] = f"Bearer {self._client._token}"

                async with self._client._http.stream(
                    "GET",
                    self._url,
                    params=self._params,
                    headers=headers,
                    # connect + write have bounded timeouts; read=None allows
                    # indefinite streaming without spurious disconnects.
                    timeout=httpx.Timeout(connect=10.0, read=None, write=10.0, pool=5.0),
                ) as resp:
                    if resp.status_code != 200:
                        raise httpx.HTTPStatusError(
                            f"SSE stream failed with {resp.status_code}",
                            request=resp.request,
                            response=resp,
                        )

                    backoff = 0.5  # reset on successful connect
                    event_type = "message"
                    data_lines = []

                    async for line in resp.aiter_lines():
                        if self._closed:
                            return

                        line = line.strip()

                        if line.startswith("event:"):
                            event_type = line[6:].strip()
                        elif line.startswith("data:"):
                            data_lines.append(line[5:].strip())
                        elif line.startswith("id:"):
                            self._last_event_id = line[3:].strip()
                        elif line == "":
                            # Empty line marks end of event
                            if data_lines:
                                data_str = "\n".join(data_lines)
                                try:
                                    data = json.loads(data_str)
                                    if event_type == "push":
                                        # The server sends the engram as a nested
                                        # object plus top-level score/why fields.
                                        engram_obj = data.get("engram")
                                        engram = None
                                        engram_id = data.get("engram_id")
                                        if isinstance(engram_obj, dict):
                                            engram = PushEngram(
                                                id=engram_obj.get("id"),
                                                concept=engram_obj.get("concept"),
                                                content=engram_obj.get("content"),
                                            )
                                            # Populate flat engram_id from the
                                            # nested id for backwards compat.
                                            if engram_id is None:
                                                engram_id = engram_obj.get("id")
                                        yield Push(
                                            subscription_id=data.get("subscription_id", ""),
                                            trigger=data.get("trigger", ""),
                                            push_number=data.get("push_number", 0),
                                            engram_id=engram_id,
                                            at=data.get("at"),
                                            score=data.get("score"),
                                            engram=engram,
                                            why=data.get("why"),
                                        )
                                    # ignore subscribed and other event types
                                except json.JSONDecodeError:
                                    pass

                            event_type = "message"
                            data_lines = []

            except httpx.HTTPStatusError as e:
                # Fatal status codes: don't retry — surface immediately.
                if e.response.status_code in (401, 403, 404):
                    raise MuninnConnectionError(
                        f"SSE stream failed with {e.response.status_code}: "
                        f"{e.response.text}"
                    ) from e
                # 5xx and other server errors: backoff and retry.
                if self._closed:
                    return
                await asyncio.sleep(min(backoff, 30.0))
                backoff = min(backoff * 2, 30.0)
            except (
                httpx.ConnectError,
                httpx.ReadError,
                httpx.RemoteProtocolError,
            ):
                if self._closed:
                    return
                await asyncio.sleep(min(backoff, 30.0))
                backoff = min(backoff * 2, 30.0)
