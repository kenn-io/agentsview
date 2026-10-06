import { describe, expect, it } from "vite-plus/test";
import { displayPRLinks, isValidPRFilter, prLinkLabel, safeExternalHref } from "./prLinks.js";

describe("safeExternalHref", () => {
  it.each([
    ["https://github.com/acme/widgets/pull/42", "https://github.com/acme/widgets/pull/42"],
    [
      "http://git.example.test/acme/widgets/-/merge_requests/7",
      "http://git.example.test/acme/widgets/-/merge_requests/7",
    ],
  ])("keeps http(s) URL %s", (url, expected) => {
    expect(safeExternalHref(url)).toBe(expected);
  });

  it.each([
    "javascript:alert(1)",
    "JavaScript:alert(1)",
    "data:text/html,<script>alert(1)</script>",
    "file:///etc/passwd",
    "/relative/path",
    "github.com/acme/widgets/pull/42",
    "",
    undefined,
    null,
  ])("rejects %s", (url) => {
    expect(safeExternalHref(url)).toBeNull();
  });
});

describe("prLinkLabel", () => {
  it("formats repository and number like owner/repo#123", () => {
    expect(
      prLinkLabel({
        repository: "acme/widgets",
        number: 42,
        url: "https://github.com/acme/widgets/pull/42",
      }),
    ).toBe("acme/widgets#42");
  });

  it("falls back to the repository, then the URL", () => {
    expect(prLinkLabel({ repository: "acme/widgets", number: 0, url: "https://x.test/p" })).toBe(
      "acme/widgets",
    );
    expect(prLinkLabel({ repository: "", number: 0, url: "https://x.test/p" })).toBe(
      "https://x.test/p",
    );
  });
});

describe("displayPRLinks", () => {
  it("drops unsafe links and duplicate URLs while keeping order", () => {
    const links = displayPRLinks([
      {
        url: "https://github.com/acme/widgets/pull/42",
        host: "github.com",
        repository: "acme/widgets",
        number: 42,
      },
      { url: "javascript:alert(1)", host: "", repository: "evil/repo", number: 1 },
      {
        url: "https://github.com/acme/widgets/pull/42",
        host: "github.com",
        repository: "acme/widgets",
        number: 42,
      },
      {
        url: "https://github.com/acme/gadgets/pull/7",
        host: "github.com",
        repository: "acme/gadgets",
        number: 7,
      },
    ]);
    expect(links).toEqual([
      { href: "https://github.com/acme/widgets/pull/42", label: "acme/widgets#42" },
      { href: "https://github.com/acme/gadgets/pull/7", label: "acme/gadgets#7" },
    ]);
  });

  it("returns nothing for a session without links", () => {
    expect(displayPRLinks(undefined)).toEqual([]);
  });
});

describe("isValidPRFilter", () => {
  it.each([
    "",
    "acme/widgets",
    " acme/widgets#42 ",
    "/acme/widgets/",
    "group/sub/project#3",
    "https://github.com/acme/widgets/pull/42",
  ])("accepts %j", (value) => {
    expect(isValidPRFilter(value)).toBe(true);
  });

  it.each([
    "widgets",
    "widgets#42",
    "acme/widgets#",
    "acme/widgets#0",
    "acme/widgets#abc",
    "acme/my widgets",
    "javascript://acme/widgets",
  ])("rejects %j", (value) => {
    expect(isValidPRFilter(value)).toBe(false);
  });
});
