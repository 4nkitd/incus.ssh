import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync, existsSync } from "node:fs";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { runInNewContext } from "node:vm";

const root = fileURLToPath(new URL("../docs/", import.meta.url));
const pages = ["index.html", "docs.html"];

test("site links and local assets resolve, with unique section IDs", () => {
  for (const name of pages) {
    const file = resolve(root, name);
    const html = readFileSync(file, "utf8");
    const ids = [...html.matchAll(/\bid="([^"]+)"/g)].map((match) => match[1]);
    assert.equal(new Set(ids).size, ids.length, `${name}: duplicate section ID`);
    for (const match of html.matchAll(/\b(?:href|src|srcset)="([^"]+)"/g)) {
      const target = match[1];
      if (/^[a-z]+:|^\/\//i.test(target)) continue;
      const [path, hash] = target.split("#");
      const destination = path ? resolve(dirname(file), path) : file;
      assert.ok(existsSync(destination), `${name}: missing ${target}`);
      if (hash && destination.endsWith(".html")) {
        const body = readFileSync(destination, "utf8");
        assert.ok(body.includes(`id="${hash}"`), `${name}: missing anchor ${target}`);
      }
    }
  }
});

test("documentation covers each workflow and separates previews", () => {
  const guide = readFileSync(resolve(root, "docs.html"), "utf8");
  for (const id of ["install", "connect", "quickstart", "environments", "shells", "lifecycle", "resources", "images", "files", "sharing", "commands", "security", "limits"]) {
    assert.ok(guide.includes(`id="${id}"`), `missing workflow ${id}`);
  }
  assert.ok(guide.includes("Experimental"));
  assert.match(guide, /share[^.]*kernel/i);
});

test("fonts are local and system dark/reduced-motion styles exist", () => {
  const css = readFileSync(resolve(root, "styles.css"), "utf8");
  assert.ok(css.includes("prefers-color-scheme: dark"));
  assert.ok(css.includes("prefers-reduced-motion: reduce"));
  for (const match of css.matchAll(/url\("([^"]+)"\)/g)) {
    assert.ok(!/^https?:/.test(match[1]), "font should not contact a third-party host");
    assert.ok(existsSync(resolve(root, match[1])));
  }
});

test("the real terminal GIF has a static reduced-motion fallback and transcript", () => {
  const gif = readFileSync(resolve(root, "assets/demo.gif"));
  const poster = readFileSync(resolve(root, "assets/demo.png"));
  assert.match(gif.subarray(0, 6).toString(), /^GIF8[79]a$/);
  assert.equal(poster.subarray(1, 4).toString(), "PNG");
  const html = readFileSync(resolve(root, "index.html"), "utf8");
  assert.match(html, /srcset="assets\/demo\.png" media="\(prefers-reduced-motion: reduce\)"/);
  assert.match(readFileSync(resolve(root, "assets/demo.txt"), "utf8"), /new --name=melon/);
});

test("copy button writes the exact command without HTML or clipboard reads", async () => {
  let handler;
  let copied;
  const button = { dataset: { copy: "install-command" }, textContent: "Copy", addEventListener(_name, callback) { handler = callback; } };
  const source = { textContent: "  curl example | sudo bash\n" };
  const status = { textContent: "" };
  runInNewContext(readFileSync(resolve(root, "site.js"), "utf8"), {
    document: { querySelectorAll: () => [button], getElementById: (id) => id === "copy-status" ? status : id === "install-command" ? source : null },
    navigator: { clipboard: { writeText: async (value) => { copied = value; } } },
    setTimeout: () => {},
  });
  await handler();
  assert.equal(copied, "curl example | sudo bash");
  assert.equal(button.textContent, "Copied");
  assert.match(status.textContent, /copied/);
});

test("guide navigation follows viewport size and closes after a mobile selection", () => {
  let resize;
  let click;
  const media = { matches: true, addEventListener(_event, callback) { resize = callback; } };
  const guide = { open: true, querySelectorAll: () => [{ addEventListener(_event, callback) { click = callback; } }] };
  runInNewContext(readFileSync(resolve(root, "site.js"), "utf8"), {
    document: { querySelectorAll: () => [], getElementById: () => guide },
    window: { matchMedia: () => media },
  });
  assert.equal(guide.open, false);
  media.matches = false;
  resize();
  assert.equal(guide.open, true);
  media.matches = true;
  guide.open = true;
  click();
  assert.equal(guide.open, false);
});
