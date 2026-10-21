import assert from "node:assert/strict";
import { access } from "node:fs/promises";
import { test } from "node:test";
import { chromium } from "playwright-core";

const APP_URL = process.env.MIND_APP_URL || "http://127.0.0.1:7331/";

async function findChrome() {
  const candidates = [
    "/opt/google/chrome/chrome", "/usr/bin/chromium", "/usr/bin/google-chrome",
    process.env.HOME + "/.cache/ms-playwright/chromium-1248/chrome-linux64/chrome",
    process.env.HOME + "/.cache/ms-playwright/chromium-1243/chrome-linux64/chrome",
    process.env.HOME + "/.cache/ms-playwright/chromium_headless_shell-1243/chrome-headless-shell-linux64/chrome-headless-shell",
    process.env.HOME + "/.cache/ms-playwright/chromium_headless_shell-1243/chrome-linux/headless_shell",
  ];
  for (const c of candidates) { try { await access(c); return c; } catch {} }
  return null;
}

// 原始 touch 事件序列（不经 Playwright 的合成点击），edge swipe 必须走真实 touch 通道：
// AppShell 里明确写了"横向手势由 touch 事件通道接管，不依赖 touch-action"。
async function swipe(cdp, from, to, y = 300) {
  await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: from, y }] });
  for (const x of [from + (to - from) * 0.3, from + (to - from) * 0.6, to]) {
    await cdp.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x, y }] });
    await new Promise((r) => setTimeout(r, 40));
  }
  await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  await new Promise((r) => setTimeout(r, 600));
}

async function drawerState(page, side) {
  return page.evaluate((side) => {
    const el = document.querySelector(`aside[data-drawer-touch="${side}"]`);
    if (!el) return { present: false };
    const r = el.getBoundingClientRect();
    const cs = getComputedStyle(el);
    const open = el.getAttribute("aria-hidden") !== "true"
      && cs.visibility === "visible" && cs.pointerEvents === "auto";
    const onScreen = side === "left" ? r.left >= -1 : r.right <= window.innerWidth + 1;
    return { present: true, open, onScreen, width: r.width, x: Math.round(r.left) };
  }, side);
}

// REGRESSION(批v93): NAS 部署的旧包没有移动端抽屉层（edge 热区 + touch 手势通道都缺），
// 手机上左/右侧边栏怎么点都开不了。此测试钉住两条入口都可用：顶栏按钮（主人实际点的就是它）
// 与边缘滑动。
const OPEN_LABELS = {
  left: /Open file sidebar|打开文件侧栏/,
  right: /Open session sidebar|打开会话侧栏/,
};

async function tapButtonByLabel(page, cdp, pattern) {
  const box = await page.evaluate((src) => {
    const re = new RegExp(src);
    const el = [...document.querySelectorAll("button[aria-label]")].find((b) => re.test(b.getAttribute("aria-label")));
    if (!el) return null;
    const r = el.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  }, pattern.source);
  assert.ok(box, `顶栏按钮未找到：${pattern}`);
  await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [box] });
  await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  await new Promise((r) => setTimeout(r, 700));
}

test("REGRESSION: mobile drawers open from both the header buttons and edge swipes", async () => {
  const executablePath = await findChrome();
  assert.ok(executablePath, "需要本机 Chromium");
  const browser = await chromium.launch({ executablePath, headless: true });
  try {
    for (const [w, h, tag] of [[375, 667, "375x667"], [360, 640, "360x640"], [390, 844, "390x844"]]) {
      const page = await browser.newPage({ viewport: { width: w, height: h }, isMobile: true, hasTouch: true });
      const cdp = await page.context().newCDPSession(page);
      await page.goto(APP_URL, { waitUntil: "domcontentloaded" });
      await page.waitForTimeout(3000);

      const zones = await page.evaluate(() => document.querySelectorAll("[data-drawer-edge]").length);
      assert.equal(zones, 2, `${tag}: 左右边缘热区必须都在（旧包=0，即"侧边栏打不开"的根因）`);

      for (const side of ["left", "right"]) {
        // 每一步都从"两扇都关"的干净态开始：抽屉开着时顶栏按钮被遮罩盖住，点它只会
        // 收起当前抽屉——那会把断言误报成"打不开"。
        await page.reload({ waitUntil: "domcontentloaded" });
        await page.waitForTimeout(2000);
        const before = await drawerState(page, side);
        assert.ok(before.present, `${tag}: ${side} 抽屉未挂载`);
        assert.ok(!before.open, `${tag}: 重载后 ${side} 抽屉仍是开的，测试前提不成立`);

        // ① 顶栏按钮（主人报的就是"点了没反应"）
        await tapButtonByLabel(page, cdp, OPEN_LABELS[side]);
        const byButton = await drawerState(page, side);
        assert.ok(byButton.open && byButton.onScreen,
          `${tag}: 点顶栏按钮没拉开 ${side} 抽屉 ${JSON.stringify(byButton)}`);

        // ② 边缘滑动
        await page.reload({ waitUntil: "domcontentloaded" });
        await page.waitForTimeout(2000);
        await swipe(cdp, side === "left" ? 8 : w - 8, side === "left" ? 148 : w - 148, 320);
        const bySwipe = await drawerState(page, side);
        assert.ok(bySwipe.open && bySwipe.onScreen,
          `${tag}: 边缘滑动没拉开 ${side} 抽屉 ${JSON.stringify(bySwipe)}`);
      }
      await page.close();
    }
  } finally {
    await browser.close();
  }
});
