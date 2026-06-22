import assert from "node:assert/strict";
import test from "node:test";

import { markdownToHtml, tocFromHtml } from "./docs-site-render.mjs";

const keepHref = (href) => href;

test("markdownToHtml escapes raw HTML inside link labels", () => {
  const html = markdownToHtml("[<br>](https://example.com)\n[<img src=x onerror=alert(1)>](https://example.com)", "index.md", keepHref);

  assert.match(html, /<a href="https:\/\/example\.com">&lt;br&gt;<\/a>/);
  assert.doesNotMatch(html, /<a href="https:\/\/example\.com"><br><\/a>/);
  assert.doesNotMatch(html, /<img\b/);
});

test("markdownToHtml gives duplicate headings stable unique ids", () => {
  const html = markdownToHtml("## Examples\n\n## Examples\n\n### Examples", "index.md", keepHref);

  assert.match(html, /<h2 id="examples">/);
  assert.match(html, /<h2 id="examples-2">/);
  assert.match(html, /<h3 id="examples-3">/);
  assert.match(tocFromHtml(html), /href="#examples-2"/);
});
