const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const since = new Date(Date.now() - 86400e3).toISOString();
const instructions = { en: "Refresh instructions", zh: "刷新说明", "zh-TW": "重新整理說明", ja: "更新手順", de: "Hinweise zum Aktualisieren" };
const pending = { en: "Pending refresh", zh: "待刷新", "zh-TW": "待重新整理", ja: "更新待ち", de: "Aktualisierung ausstehend" };

function fixture(lang, posted) {
  return async (route) => {
    const url = new URL(route.request().url()), request = route.request();
    if (request.method() === "POST") posted.push(url.pathname);
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs={lang:"${lang}",theme:"light",web:false};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window={};" });
    if (url.pathname === "/api/state") return json({
      agents: [{ id: "codex", name: "Codex", wired: true, path: "/fixture/config.toml",
        fields: [{ key: "model", label: "model", value: "fixture", options: [{value:"fixture",label:"Fixture",ref:"fixture/model"}] }],
        stale: 2, staleCopies: [{ kind: "app", bundle: "/Applications/Codex.app", since }, { kind: "daemon", since }] }],
      profiles: [], settings: { lang, theme: "light" },
    });
    if (url.pathname === "/api/providers") return json({
      providers: [{ id: "codex", name: "Codex", icon: "codex-color", models: [], agents: [], headers: {}, keyList: [],
        account: { agent: "codex", agentName: "Codex", user: "new@example.test", logins: [{ user: "new@example.test", active: true, on: true }] } }],
      codexDaemon: "old@example.test", codexApp: "old@example.test",
      presets: [], excluded: [], gateway: { running: true, window: true },
    });
    if (url.pathname === "/api/agents/cli") return json({ agents: {}, pending: false });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    return route.fulfill({ body: await fs.readFile(file), contentType: { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml" }[path.extname(file)] });
  };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of Object.keys(instructions)) {
    for (const width of [980, 440]) {
      test(`${engine} ${lang} ${width}px: Codex refresh controls send zero restart requests`, async (t) => {
        const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
        t.after(() => browser.close());
        const page = await browser.newPage({ viewport: { width, height: 820 }, reducedMotion: "reduce" });
        page.setDefaultTimeout(6000);
        const posted = [], errors = [];
        page.on("pageerror", (e) => errors.push(e.message));
        await page.route("**/*", fixture(lang, posted));
        await page.goto("http://magpie.test/?view=agents");
        const row = page.locator('.row.agent[data-id="codex"]');
        assert.ok((await row.locator(".ag-st-t").textContent()).includes(pending[lang]));
        await row.locator(".ag-link").click();
        const controls = row.locator(".ag-stale-copy button");
        await controls.first().waitFor();
        assert.equal(await controls.count(), 2);
        for (const go of await controls.all()) {
          assert.equal(await go.textContent(), instructions[lang]);
          await go.click();
          const said = await page.locator("#status").textContent();
          assert.ok(said.includes("Codex") && said.includes("magpie"), said);
          assert.doesNotMatch(said, /\{\w+\}/);
          assert.equal(await page.locator("dialog[open]").count(), 0);
        }
        assert.equal(await row.locator(".ag-stale-copy").count(), 2, "instructions do not clear pending state");
        assert.deepEqual(posted, []);
        await page.goto("http://magpie.test/?view=providers");
        await page.locator(".row.provider", { hasText: "Codex" }).first().click();
        const notices = page.locator(".editor .signing.daemon");
        await notices.first().waitFor();
        assert.equal(await notices.count(), 2);
        const go = notices.getByRole("button", { name: instructions[lang], exact: true });
        assert.equal(await go.count(), 2);
        for (const control of await go.all()) {
          await control.click();
          const text = await page.locator("#status").textContent();
          assert.ok(text.includes("Codex") && text.includes("magpie"), text);
          assert.doesNotMatch(text, /\{\w+\}/);
        }
        assert.deepEqual(posted, [], "account notice never posts a restart");
        assert.equal(await notices.count(), 2);
        assert.equal(await page.evaluate(() => document.scrollingElement.scrollWidth <= innerWidth), true);
        assert.deepEqual(errors, []);
        if (process.env.ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `codex-lifecycle-${engine}-${lang}-${width}.png`) });
      });
    }
  }
}
