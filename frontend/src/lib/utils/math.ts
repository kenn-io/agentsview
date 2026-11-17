import katex from "katex";
import { Tokenizer, type MarkedExtension, type Tokens } from "marked";
import { escapeHtml } from "@kenn-io/kit-ui";

interface MathToken extends Tokens.Generic {
  type: "inlineMath" | "displayMath";
  text: string;
  displayMode: boolean;
}

// KaTeX math environments, including the standard math/displaymath wrappers.
const environment =
  /^(?:math|displaymath|equation\*?|align\*?|alignat\*?|gather\*?|aligned|alignedat|gathered|split|CD|[pbBvV]?matrix\*?|smallmatrix|[d]?array|[dr]?cases|drcases)$/;
const opening = /^(\$\$|\$|\\\(|\\\[|\\begin\{([A-Za-z]+\*?)\})/;

export function isMathFence(language: string | undefined): boolean {
  return /^(math|latex|tex)$/i.test(language?.trim() ?? "");
}

/** Consume math before Markdown can interpret its escapes, underscores or HTML. */
function readMath(src: string): MathToken | undefined {
  const match = opening.exec(src);
  if (!match) return;
  const left = match[1]!;
  const name = match[2];
  if (name && !environment.test(name)) return;
  const singleDollar = left === "$";
  const backtick = "`";
  const githubMath = singleDollar && src[left.length] === backtick;
  if (singleDollar && /[\s$]/.test(src[left.length] ?? "")) return;
  const right = name ? `\\end{${name}}` : left === "\\(" ? "\\)" : left === "\\[" ? "\\]" : left;
  const displayMode = left !== "$" && left !== "\\(" && name !== "math";
  const environments = name ? [name] : [];
  let braces = 0;
  for (let i = left.length; i < src.length; i++) {
    const char = src[i]!;
    if (
      singleDollar &&
      braces === 0 &&
      char === backtick &&
      !(githubMath && (i === left.length || src[i + 1] === "$"))
    )
      return;
    if (singleDollar && (char === "\n" || char === "\r")) return;
    if (name && braces === 0 && char === "\\") {
      const command = /^\\(begin|end)\{([A-Za-z]+\*?)\}/.exec(src.slice(i));
      if (command) {
        if (command[1] === "begin") environments.push(command[2]!);
        else if (environments.pop() !== command[2]) return;
        // Only the matching outer end completes a bare math environment.
        if (environments.length > 0) {
          i += command[0].length - 1;
          continue;
        }
      }
    }
    if (braces === 0 && src.startsWith(right, i)) {
      // Pandoc-style dollar boundaries keep ordinary prices and escaped dollars literal.
      if (singleDollar && (/\s/.test(src[i - 1]!) || /\d/.test(src[i + 1] ?? ""))) return;
      const raw = src.slice(0, i + right.length);
      let text =
        name && name !== "math" && name !== "displaymath" ? raw : src.slice(left.length, i);
      // GitHub also accepts a backtick pair inside single-dollar math.
      if (githubMath && text.endsWith(backtick)) text = text.slice(1, -1);
      if (!text.trim()) return;
      return { type: displayMode ? "displayMath" : "inlineMath", raw, text, displayMode };
    }
    if (char === "\\") i++;
    else if (char === "%") {
      const newline = src.indexOf("\n", i);
      if (newline < 0 || singleDollar) return;
      i = newline;
    } else if (char === "{") braces++;
    else if (char === "}") braces = Math.max(0, braces - 1);
  }
}

function readDisplayMath(src: string): MathToken | undefined {
  const indent = /^ {0,3}/.exec(src)![0];
  const token = readMath(src.slice(indent.length));
  if (!token?.displayMode) return;
  // A block consumes its line; same-line prose belongs to the inline lexer.
  const rest = src.slice(indent.length + token.raw.length);
  const ending = /^(?:[ \t]*\r?\n|[ \t]*$)/.exec(rest);
  if (!ending) return;
  return { ...token, raw: indent + token.raw + ending[0] };
}

function typeset(text: string, displayMode: boolean): string | undefined {
  try {
    return katex.renderToString(text, {
      displayMode,
      output: "htmlAndMathml",
      trust: false,
      throwOnError: true,
      maxExpand: 1000,
      maxSize: 20,
    });
  } catch {
    // Unsupported or unfinished model output must not break the surrounding message.
    return undefined;
  }
}

/** A TeX source fence can contain a whole document; render only valid math. */
export function renderMathFence(source: string): string | undefined {
  const text = source.trim();
  const math = readMath(text);
  return typeset(math?.raw.length === text.length ? math.text : text, true);
}

export function mathExtension(): MarkedExtension {
  const renderer = (token: Tokens.Generic) => {
    const math = token as MathToken;
    return typeset(math.text, math.displayMode) ?? escapeHtml(math.raw);
  };
  return {
    extensions: [
      {
        name: "displayMath",
        level: "block",
        start: (src) => {
          const match = /(?:^|\n)(?= {0,3}(?:\$\$|\\\[|\\begin\{))/.exec(src);
          return match ? match.index + match[0].length : undefined;
        },
        tokenizer: readDisplayMath,
        renderer,
      },
      {
        name: "inlineMath",
        level: "inline",
        start: (src) => src.search(/\$|\\[([]|\\begin\{/),
        tokenizer(src) {
          const token = readMath(src);
          return token ? { ...token, type: "inlineMath" } : undefined;
        },
        renderer,
      },
    ],
    tokenizer: {
      lheading(src) {
        // Marked tries setext headings before applying extension start boundaries.
        // Do not let an equation's standalone "=" consume the preceding prose.
        const heading = this.rules.block.lheading.exec(src);
        if (!heading) return false;
        for (const match of src.matchAll(/(?:^|\n)(?= {0,3}(?:\$\$|\\\[|\\begin\{))/g)) {
          const start = match.index + match[0].length;
          if (start >= heading[0].length) break;
          if (readDisplayMath(src.slice(start))) {
            return Tokenizer.prototype.lheading.call(this, src.slice(0, start));
          }
        }
        return false;
      },
    },
    renderer: {
      code(token) {
        if (!isMathFence(token.lang)) return false;
        return renderMathFence(token.text) ?? false;
      },
    },
  };
}
