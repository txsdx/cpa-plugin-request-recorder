// Use the same real CPA integration fixture as the other browser tests.
const { chromium } = require("playwright-core");
const assert = require("node:assert/strict");

(async () => {
  const base = process.env.RECORDER_TEST_BASE || "http://127.0.0.1:8787";
  const key = process.env.RECORDER_MANAGEMENT_KEY;
  if (!key) throw new Error("Set RECORDER_MANAGEMENT_KEY");
  const expected = Buffer.from(
    "%PDF-1.7\nsynthetic uploaded file\x00\xff\n%%EOF\n",
    "latin1",
  );
  const browser = await chromium.launch({
    executablePath: process.env.CHROMIUM_PATH || "/usr/bin/chromium",
    headless: true,
    args: ["--no-sandbox"],
  });
  try {
    const page = await browser.newPage({
      viewport: { width: 1440, height: 1000 },
    });
    const errors = [],
      external = [],
      downloads = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("request", (request) => {
      if (new URL(request.url()).origin !== new URL(base).origin)
        external.push(request.url());
      if (new URL(request.url()).pathname.endsWith("/file"))
        downloads.push(request);
    });
    await page.route("**/v8/management/config/access/api-keys", (route) =>
      route.fulfill({ json: ["sk-recorder-integration-client"] }),
    );
    // Source URL injection must never create a direct link or network request.
    await page.route(
      "**/v0/management/plugins/request-recorder/record?*",
      async (route) => {
        const response = await route.fetch();
        if (!response.ok()) return route.fulfill({ response });
        const data = await response.json();
        for (const message of data.conversation || [])
          for (const file of message.files || []) {
            file.url = "https://file-tracker.invalid/steal.pdf";
            // Exercise the pending-to-ready inventory poll as well as download.
            file.status = "pending";
          }
        await route.fulfill({ response, json: data });
      },
    );
    await page.goto(base + "/v0/resource/plugins/request-recorder/index.html");
    await page.locator("#login-open").click();
    await page.locator("#management-key").fill(key);
    await page.locator("#login-form button[type=submit]").click();
    await page.locator("#login").waitFor({ state: "hidden" });
    await page.locator("#rows tr").first().waitFor();
    let found = false;
    const count = await page.locator("#rows tr").count();
    for (let i = 0; i < count; i++) {
      await page.locator("#rows tr").nth(i).click();
      await page.locator("#detail").waitFor({ state: "visible" });
      if (await page.locator(".file-card").count()) {
        found = true;
        break;
      }
      await page.locator("[data-close=detail]").click();
    }
    assert(found, "uploaded file must have a card in conversation replay");
    await page.locator(".bubble .message-time").first().waitFor();
    assert(
      await page.evaluate(() => {
        const when = "2026-11-03T08:00:00Z";
        for (const [role, kind] of [
          ["user", "text"],
          ["assistant", "tool_call"],
          ["tool", "tool_result"],
        ]) {
          const node = bubble({
            role,
            kind,
            content: "fixture",
            recorded_at: when,
            time_source:
              role === "assistant" ? "response_completed" : "request_received",
          });
          const time = node.querySelector("time");
          if (!time || time.dateTime !== when || !time.title.includes("CPA"))
            return false;
        }
        return !bubble({
          role: "user",
          kind: "text",
          content: "unknown old time",
        }).querySelector("time");
      }),
      "observed times must render beside every role, unknown history stays unstamped",
    );
    const card = page
      .locator(".file-card")
      .filter({ hasText: "测试附件.pdf" })
      .first();
    await card
      .getByRole("button", { name: "下载原文件", exact: true })
      .waitFor();
    assert.equal(
      downloads.length,
      0,
      "files must not be automatically downloaded",
    );
    assert(
      !(await page.locator("#detail-body").textContent()).includes("base64,"),
    );
    assert.equal(
      await page
        .locator('a[href*="file-tracker.invalid"],iframe,object,embed')
        .count(),
      0,
    );
    const downloadEvent = page.waitForEvent("download");
    await card.getByRole("button", { name: "下载原文件", exact: true }).click();
    const downloaded = await downloadEvent;
    assert.equal(downloaded.suggestedFilename(), "测试附件.pdf");
    const chunks = [];
    for await (const chunk of await downloaded.createReadStream())
      chunks.push(chunk);
    assert.deepEqual(Buffer.concat(chunks), expected);
    assert.equal(downloads.length, 1);
    assert.equal(downloads[0].headers().authorization, "Bearer " + key);
    await page.setViewportSize({ width: 390, height: 844 });
    await card.scrollIntoViewIfNeeded();
    assert(
      await card.evaluate(
        (node) => node.getBoundingClientRect().right <= innerWidth,
      ),
    );
    await page.screenshot({ path: "/tmp/recorder-files-mobile.png" });
    assert.deepEqual(errors, []);
    assert.deepEqual(external, []);
    console.log(
      "PASS: attachment cards, lazy authenticated download, original Unicode filename and bytes, no base64 in replay, no remote requests, mobile layout",
    );
  } finally {
    await browser.close();
  }
})().catch((error) => {
  console.error(error);
  process.exit(1);
});
