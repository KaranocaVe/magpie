const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const override = { agent: "codex", session: "sess-1", title: "Remote build session", model: "gpt-6.2-sol", provider: "moonup0.25", mode: "prefer", valid: true };
const invalid = { agent: "codex", session: "sess-1", title: "Remote build session", model: "gpt-6.2-terra", provider: "removed-provider", mode: "strict", valid: false, reason: "Provider removed" };

function serve(lang) {
  let current = [override, invalid];
  return async (route) => {
    const u = new URL(route.request().url());
    const json = (x) => route.fulfill({ json: x });
    if (u.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs={lang:"${lang}",theme:"light",web:true};` });
    if (u.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (u.pathname === "/api/state") return json({ agents: [{ id: "codex", name: "Codex", fields: [] }], profiles: [], settings: { lang, theme: "light" } });
    if (u.pathname === "/api/session-routing/trace") return json({ override: { session: override.session, model: override.model, provider: override.provider, mode: override.mode, result: "applied", actualProvider: override.provider }, note: "subagent request does not identify its parent session" });
    if (u.pathname === "/api/session-routing" && route.request().method() === "GET") return json({ overrides: current, models: [{ id: override.model, name: override.model, providers: [override.provider] }, { id: "gpt-6.2-terra", name: "gpt-6.2-terra", providers: ["moonup0.25"] }, { id: "gpt-6.2-luna", name: "gpt-6.2-luna", providers: ["moonup0.25"] }], providers: [{ id: override.provider, name: "Moonup 0.25" }] });
    if (u.pathname === "/api/session-routing" && route.request().method() === "POST") {
      const body = route.request().postDataJSON();
      if (body.action === "clear") current = [];
      else if (body.action === "delete") current = current.filter((x) => !(x.agent === body.agent && x.session === body.session && x.model === body.model));
      else if (body.action === "set") current = [...current.filter((x) => !(x.agent === body.agent && x.session === body.session && x.model === body.model)), { ...body, title: "Remote build session", valid: true }];
      return json({ saved: true });
    }
    if (u.pathname === "/api/sessions/manage") return json({ agent: "codex", agents: [{ agent: "codex", name: "Codex", icon: "" }], sessions: [{ id: "sess-1", agent: "codex", title: "Remote build session", cwd: "/tmp", start: new Date().toISOString(), last: new Date().toISOString(), models: [], transcript: false, gateway: true }], trash: [], terminal: false });
    if (u.pathname === "/api/speeds") return json([]);
    if (u.pathname === "/api/gateway/trace") return json({ mine: true, now: new Date().toISOString(), seq: 1, totals: { requests: 1 }, routes: [{ id: 1, seq: 1, time: new Date().toISOString(), agent: "codex", session: "sess-1", model: override.model, provider: override.provider, done: true, status: 200, order: [{ id: "p", provider: override.provider, name: "Moonup 0.25", model: override.model }], tries: [], sessionOverride: { session: "sess-1", model: override.model, provider: override.provider, mode: "prefer", result: "applied", actualProvider: override.provider }, sessionOverrideNote: "subagent request does not identify its parent session" }] });
    if (u.pathname === "/api/groups") return json({ groups: [], models: [{ id: override.model, name: override.model }] });
    if (u.pathname === "/api/providers") return json({ providers: [{ id: override.provider, name: "Moonup 0.25", models: [{ id: override.model, name: override.model }, { id: "gpt-6.2-terra", name: "gpt-6.2-terra" }, { id: "gpt-6.2-luna", name: "gpt-6.2-luna" }] }], gateway: { running: true, window: true } });
    if (u.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, u.pathname === "/" ? "index.html" : u.pathname);
    const type = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml" }[path.extname(file)];
    return route.fulfill({ body: await fs.readFile(file), contentType: type });
  };
}

for (const engine of ["chromium", "webkit"]) for (const lang of ["en", "zh"]) {
  test(`${engine} ${lang}: session routing overrides are visible and searchable`, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const page = await browser.newPage({ viewport: { width: 1200, height: 900 }, reducedMotion: "reduce" });
    page.setDefaultTimeout(8000);
    const errors = [];
    page.on("pageerror", (e) => errors.push(e.message));
    await page.route("**/*", serve(lang));
    await page.goto("http://magpie.test/?view=routing");
    await page.locator("#rtToSessionOverrides").click();
    const panel = page.locator(".rt-session-overrides");
    await panel.waitFor();
    await assert.doesNotReject(() => panel.locator(".rt-session-row").first().waitFor());
    assert.match(await panel.textContent(), /Remote build session/);
    await panel.locator("input[type=search]").fill("moonup0.25");
    assert.equal(await panel.locator(".rt-session-row").count(), 1);
    await panel.locator("input[type=search]").fill("does-not-match");
    assert.match(await panel.textContent(), lang === "zh" ? /没有匹配/ : /No matching/);
    await panel.locator("input[type=search]").fill("");
    await panel.locator(".rt-session-row button").filter({ hasText: lang === "zh" ? "编辑" : "Edit" }).first().click();
    await panel.locator(".rt-session-editor button").first().click();
    await panel.locator(".rt-session-open").first().click();
    await page.locator(".sm-sess").first().waitFor();
    await page.locator(".sm-routing-overrides").waitFor();
    await page.waitForFunction(() => { const x = document.querySelector(".sm-routing-overrides")?.textContent || ""; return !x.includes("Loading routing overrides") && !x.includes("正在加载路由覆盖"); });
    const sessionPanel = page.locator(".sm-routing-overrides");
    assert.match(await sessionPanel.textContent(), /Remote build session|gpt-6.2-sol/);
    assert.match(await sessionPanel.textContent(), /Provider removed|供应商已删除/);
    await sessionPanel.getByRole("button", { name: lang === "zh" ? "添加模型覆盖" : "Add model override" }).click();
    const editor = sessionPanel.locator(".sm-routing-editor");
    await editor.locator("select").nth(0).selectOption("gpt-6.2-luna");
    await editor.locator("select").nth(1).selectOption("moonup0.25");
    await editor.getByRole("button", { name: lang === "zh" ? "保存" : "Save" }).click();
    await page.waitForFunction(() => document.querySelectorAll(".sm-routing-item").length === 3);
    const sol = sessionPanel.locator(".sm-routing-item").filter({ hasText: "gpt-6.2-sol" });
    await sol.getByRole("button", { name: lang === "zh" ? "编辑" : "Edit" }).click();
    await sessionPanel.locator(".sm-routing-editor select").last().selectOption("strict");
    await sessionPanel.locator(".sm-routing-editor").getByRole("button", { name: lang === "zh" ? "保存" : "Save" }).click();
    await page.waitForFunction(() => document.querySelector(".sm-routing-item")?.textContent.includes("Strict") || document.querySelector(".sm-routing-item")?.textContent.includes("严格"));
    const bad = sessionPanel.locator(".sm-routing-item").filter({ hasText: "removed-provider" });
    await bad.getByRole("button", { name: lang === "zh" ? "删除" : "Delete" }).click();
    await page.waitForFunction(() => !document.querySelector(".sm-routing-overrides")?.textContent.includes("removed-provider"));
    await sessionPanel.getByRole("button", { name: lang === "zh" ? "清除全部覆盖" : "Clear all overrides" }).click();
    await page.waitForFunction(() => document.querySelector(".sm-routing-overrides")?.textContent.includes("No model overrides") || document.querySelector(".sm-routing-overrides")?.textContent.includes("没有模型覆盖"));
    assert.match(await sessionPanel.textContent(), lang === "zh" ? /没有模型覆盖/ : /No model overrides/);
    assert.deepEqual(errors, []);
  });
}
