// browser_bridge.js: playwright node bridge. Usage: node browser_bridge.js <json-args>;
// prints a JSON result on stdout. Args: {action, url?, selector?, text?, path?}.
"use strict";

const fs = require("fs");
const path = require("path");

function fail(msg) {
  process.stdout.write(JSON.stringify({ success: false, error: msg }));
  process.exit(0);
}

async function main() {
  let args;
  try {
    args = JSON.parse(process.argv[2] || "{}");
  } catch (e) {
    return fail("invalid args json: " + e.message);
  }
  let playwright;
  try {
    // require may resolve from the global NODE_PATH; explicit env override supported
    playwright = require(process.env.CHYAN_PLAYWRIGHT || "playwright");
  } catch (e) {
    return fail("browser tool requires node + playwright (npm i playwright): " + e.message);
  }

  // one-shot: always launch a fresh chromium and close it before exit
  const browser = await playwright.chromium.launch();
  try {
    const page = await browser.newPage();
    const action = args.action;
    if (action === "open") {
      if (!args.url) return fail("url is required");
      const resp = await page.goto(args.url, { waitUntil: "load", timeout: 30000 });
      return ok({ action: action, url: args.url, title: await page.title(), status: resp ? resp.status() : null });
    }
    if (action === "click") {
      if (!args.selector) return fail("selector is required");
      await page.click(args.selector, { timeout: 10000 });
      return ok({ action: action, selector: args.selector });
    }
    if (action === "type") {
      if (!args.selector) return fail("selector is required");
      await page.fill(args.selector, args.text || "", { timeout: 10000 });
      return ok({ action: action, selector: args.selector });
    }
    if (action === "text") {
      if (!args.url) return fail("url is required");
      await page.goto(args.url, { waitUntil: "load", timeout: 30000 });
      return ok({ action: action, url: args.url, text: await page.textContent("body") || "" });
    }
    if (action === "screenshot") {
      if (!args.url) return fail("url is required");
      await page.goto(args.url, { waitUntil: "load", timeout: 30000 });
      if (!args.path) return fail("path is required");
      fs.mkdirSync(path.dirname(args.path), { recursive: true });
      await page.screenshot({ path: args.path, fullPage: true });
      return ok({ action: action, url: args.url, path: args.path });
    }
    return fail("unknown action: " + String(action));
  } finally {
    await browser.close();
  }
}

function ok(extra) {
  process.stdout.write(JSON.stringify(Object.assign({ success: true }, extra)));
}

main().catch((e) => fail(String((e && e.message) || e)));
