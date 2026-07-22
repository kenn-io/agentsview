import { describe, expect, it } from "vitest";
import { getResponseBody } from "./generated/core/request";

async function expectExactBlob(contentType: string, expected: Uint8Array): Promise<void> {
  const response = new Response(Uint8Array.from(expected).buffer, {
    headers: { "Content-Type": contentType },
  });

  const body = await getResponseBody(response);

  expect(body).toBeInstanceOf(Blob);
  expect(new Uint8Array(await (body as Blob).arrayBuffer())).toEqual(expected);
}

describe("generated response decoding", () => {
  it("preserves non-UTF-8 octet-stream response bytes", async () => {
    await expectExactBlob("application/octet-stream", new Uint8Array([0x00, 0xff, 0x80, 0x7f]));
  });

  it("preserves zstd response bytes", async () => {
    await expectExactBlob(
      "application/zstd; charset=binary",
      new Uint8Array([0x28, 0xb5, 0x2f, 0xfd, 0xff, 0x00]),
    );
  });

  it("continues parsing JSON error bodies", async () => {
    const response = new Response('{"detail":"invalid artifact"}', {
      status: 400,
      headers: { "Content-Type": "application/problem+json; charset=utf-8" },
    });

    await expect(getResponseBody(response)).resolves.toEqual({
      detail: "invalid artifact",
    });
  });
});
