import { describe, expect, it } from "vite-plus/test";
import { sessionAncestryMatcher, sessionAncestryMatches } from "./session-ancestry.js";

interface Node {
  id: string;
  parent_session_id?: string;
  parent_session_ids?: string[];
}

describe("sessionAncestryMatcher", () => {
  it("keeps one-off parent lookups bounded when the parent is near the front", () => {
    let idReads = 0;
    const nodes: Node[] = Array.from({ length: 500 }, (_, i) => ({
      get id() {
        idReads++;
        return `session-${i}`;
      },
    }));

    expect(
      sessionAncestryMatches(
        { id: "child", parent_session_id: "session-1" },
        nodes,
        (node) => node.id === "session-1",
      ),
    ).toBe(true);
    expect(idReads).toBeLessThanOrEqual(10);
  });

  it.each([
    ["leaf first", false],
    ["root first", false],
    ["leaf first", true],
    ["root first", true],
  ] as const)(
    "classifies a 1,000-session chain once per node, %s, root match %s",
    (order, rootMatches) => {
      const nodes = Array.from({ length: 1000 }, (_, i) => ({
        id: `session-${i}`,
        parent_session_id: i ? `session-${i - 1}` : undefined,
      }));
      let calls = 0;
      const matcher = sessionAncestryMatcher(nodes, (node) => {
        calls++;
        return rootMatches && node.id === "session-0";
      });
      for (const node of order === "root first" ? nodes : nodes.toReversed()) {
        expect(matcher(node)).toBe(rootMatches);
      }
      expect(calls).toBeLessThanOrEqual(1000);
    },
  );

  it.each<{ name: string; nodes: Node[]; match: string; answers: boolean[] }>([
    {
      name: "a match in the middle only reaches descendants",
      nodes: [
        { id: "root" },
        { id: "middle", parent_session_id: "root" },
        { id: "leaf", parent_session_id: "middle" },
      ],
      match: "middle",
      answers: [false, true, true],
    },
    {
      name: "a contextual parent takes precedence",
      nodes: [
        { id: "root" },
        { id: "match" },
        { id: "leaf", parent_session_id: "root", parent_session_ids: ["match"] },
      ],
      match: "match",
      answers: [false, true, false],
    },
    {
      name: "a visited branch off the matching path is not marked as matching",
      nodes: [
        { id: "leaf", parent_session_ids: ["match", "root"] },
        { id: "root" },
        { id: "match" },
      ],
      match: "match",
      answers: [true, false, true],
    },
    {
      name: "cycles terminate without a match",
      nodes: [
        { id: "a", parent_session_id: "b" },
        { id: "b", parent_session_id: "a" },
      ],
      match: "missing",
      answers: [false, false],
    },
    {
      name: "missing parents stop the walk",
      nodes: [{ id: "leaf", parent_session_id: "missing" }],
      match: "missing",
      answers: [false],
    },
    {
      name: "a settled false branch leaves a sibling's other parent reachable",
      nodes: [
        { id: "a", parent_session_id: "root" },
        { id: "b", parent_session_ids: ["match", "root"] },
        { id: "root" },
        { id: "match" },
      ],
      match: "match",
      answers: [false, true, false, true],
    },
  ])("$name", ({ nodes, match, answers }) => {
    const matcher = sessionAncestryMatcher(nodes, (node) => node.id === match);
    expect(nodes.map(matcher)).toEqual(answers);
  });
});
