// The two sealed-task declarations belong to a Responses key, are restored
// from what was saved, and are posted independently. Subscriptions and
// chat-only keys must not offer them.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const base = {
  icon: "generic", chat: "", responses: "", anthropic: "", catalog: "",
  models: [{ id: "m1", name: "M1", on: true }], agents: [], fallback: [], headers: {},
  key: { set: true, masked: "sk-…1234" }, keyList: [], balanceToken: { takes: false, set: false }, proxy: "",
  searches: false, unredacted: false, passUserAgent: false,
};
const responses = (id, name, flags) => ({
  ...base, id, name, responses: "https://relay.example.com/v1", carriesCompaction: true, ...flags,
});
const enabled = responses("sealed-on", "Sealed Responses", {
  allowCrossProviderSealedTasks: true, routesToOfficialCodexBackend: true,
});
const disabled = responses("sealed-off", "Unmarked Responses", {
  allowCrossProviderSealedTasks: false, routesToOfficialCodexBackend: false,
});
const chat = { ...base, id: "chat-only", name: "Chat-only relay", chat: "https://chat.example.com/v1" };
const subscription = {
  ...base, id: "codex", name: "Codex subscription", responses: "https://chatgpt.com/backend-api/codex",
  carriesCompaction: true, nativeCompaction: true,
  account: { agent: "codex", agentName: "Codex", user: "me@example.com", plan: "PLUS", logins: [{ user: "me@example.com", plan: "PLUS", active: true, on: true }] },
};

function serve(lang, posts) {
  const providers = { providers: [enabled, disabled, chat, subscription], presets: [], excluded: [], gateway: { running: true, window: true } };
  const state = { agents: [], profiles: [], settings: { lang, theme: "light" } };
  return async (route) => {
    const url = new URL(route.request().url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json(state);
    if (url.pathname === "/api/providers") return json(providers);
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname.startsWith("/api/provider/") && route.request().method() === "POST") {
      posts.push({ action: url.pathname.slice("/api/provider/".length), body: route.request().postDataJSON() });
      return json(providers);
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    await route.fulfill({ body: await fs.readFile(file), contentType });
  };
}

const languages = ["en", "zh", "zh-TW", "ja", "de"];
const sharingRow = (page) => page.locator(".editor .cross-provider-sealed-tasks");
const officialRow = (page) => page.locator(".editor .official-codex-backend");
const checkbox = (row) => row.locator('input[type="checkbox"]');

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of languages) {
    for (const width of [1100, 420]) {
      const open = async (t, name) => {
        const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
        t.after(async () => browser.close());
        const page = await (await browser.newContext({ viewport: { width, height: 760 }, reducedMotion: "reduce" })).newPage();
        page.setDefaultTimeout(5000);
        page.setDefaultNavigationTimeout(60000);
        const errors = [];
        page.on("pageerror", (error) => errors.push(error.message));
        const posts = [];
        await page.route("**/*", serve(lang, posts));
        await page.goto("http://magpie.test/?view=providers");
        await page.locator(".row.provider", { hasText: name }).click();
        await page.locator(".editor .bar").waitFor();
        return { page, errors, posts };
      };

      const save = async (page, posts) => {
        const start = posts.length;
        await page.locator(".editor .bar button.primary").click();
        for (let i = 0; i < 50 && !posts.slice(start).some((post) => post.action === "save"); i++) {
          await page.waitForTimeout(50);
        }
        const result = posts.slice(start).find((post) => post.action === "save");
        assert.ok(result, "the provider Save POST was sent");
        return result;
      };

      const fits = async (row, label) => {
        const rect = await row.boundingBox();
        assert.ok(rect && rect.width > 0 && rect.x >= 0 && rect.x + rect.width <= width,
          `${label} fits at ${width}px: ${JSON.stringify(rect)}`);
      };

      test(`${engine} ${lang} ${width}: saved Responses flags open and save independently`, async (t) => {
        const { page, errors, posts } = await open(t, "Sealed Responses");
        const advanced = page.locator(".editor details.advanced");
        await advanced.waitFor({ state: "attached" });
        assert.equal(await advanced.evaluate((details) => details.open), true, "both saved flags open Advanced");
        assert.equal(await checkbox(sharingRow(page)).isChecked(), true);
        assert.equal(await checkbox(officialRow(page)).isChecked(), true);
        await fits(sharingRow(page), "cross-provider flag");
        await fits(officialRow(page), "official-backend flag");

        await checkbox(sharingRow(page)).uncheck();
        const saved = await save(page, posts);
        assert.equal(saved.body.id, "sealed-on");
        assert.equal(saved.body.allowCrossProviderSealedTasks, false);
        assert.equal(saved.body.routesToOfficialCodexBackend, true);
        assert.deepEqual(errors, []);
      });

      test(`${engine} ${lang} ${width}: unset Responses flags stay folded until Advanced opens`, async (t) => {
        const { page, errors, posts } = await open(t, "Unmarked Responses");
        const advanced = page.locator(".editor details.advanced");
        await advanced.waitFor({ state: "attached" });
        assert.equal(await advanced.evaluate((details) => details.open), false, "unset flags leave Advanced folded");
        await advanced.locator(":scope > summary").click();
        assert.equal(await advanced.evaluate((details) => details.open), true, "Advanced opens on request");
        await sharingRow(page).waitFor({ state: "visible" });
        await officialRow(page).waitFor({ state: "visible" });
        assert.equal(await checkbox(sharingRow(page)).isChecked(), false);
        assert.equal(await checkbox(officialRow(page)).isChecked(), false);

        await checkbox(officialRow(page)).check();
        const saved = await save(page, posts);
        assert.equal(saved.body.id, "sealed-off");
        assert.equal(saved.body.allowCrossProviderSealedTasks, false);
        assert.equal(saved.body.routesToOfficialCodexBackend, true);
        assert.deepEqual(errors, []);
      });

      test(`${engine} ${lang} ${width}: chat-only keys and subscriptions hide both flags`, async (t) => {
        for (const name of ["Chat-only relay", "Codex subscription"]) {
          const { page, errors } = await open(t, name);
          assert.equal(await sharingRow(page).isVisible(), false, `${name}: cross-provider flag hidden`);
          assert.equal(await officialRow(page).isVisible(), false, `${name}: official-backend flag hidden`);
          assert.deepEqual(errors, []);
        }
      });
    }
  }
}
