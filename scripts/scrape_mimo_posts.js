#!/usr/bin/env node

/**
 * Scrape MiMo Token Plan posts from linux.do forum welfare section.
 * Outputs raw post content as JSON — key extraction is done by AI (Claude).
 *
 * Requires a running Chrome/Chromium with remote debugging enabled:
 *   /Applications/Google\ Chrome.app/Contents/MacOS/Google\ Chrome --remote-debugging-port=9222
 *
 * Usage:
 *   node scripts/scrape_mimo_posts.js > /tmp/mimo_posts.json
 */

const puppeteer = require('puppeteer-core');

const BROWSER_URL = 'http://127.0.0.1:9222';
const LINUXDO_WELFARE_URL = 'https://linux.do/c/welfare/36';
const TOPIC_TITLE_REGEX = /mimo|小米|token|key/i;
const NAVIGATION_TIMEOUT = 30000;
const PAGE_LOAD_DELAY = 2000;
const POST_LOAD_DELAY = 1000;

function sleep(ms) {
  return new Promise(r => setTimeout(r, ms));
}

async function main() {
  const browser = await puppeteer.connect({ browserURL: BROWSER_URL });
  const pages = await browser.pages();
  let page = pages.find(p => p.url().includes('linux.do'));
  if (!page) {
    page = await browser.newPage();
  }

  await page.goto(LINUXDO_WELFARE_URL, { waitUntil: 'networkidle2', timeout: NAVIGATION_TIMEOUT }).catch(() => {});
  await sleep(PAGE_LOAD_DELAY);

  const topics = await page.evaluate((regexSource) => {
    const regex = new RegExp(regexSource, 'i');
    const items = [];
    document.querySelectorAll('.topic-list-item, .topic-list tbody tr').forEach(el => {
      const titleEl = el.querySelector('.title a, .link-top-line a, a.title');
      if (titleEl) {
        const title = titleEl.textContent.trim();
        if (regex.test(title)) {
          items.push({ title, url: titleEl.href });
        }
      }
    });
    return items;
  }, TOPIC_TITLE_REGEX.source);

  const results = [];

  for (const t of topics) {
    await page.goto(t.url, { waitUntil: 'networkidle2', timeout: 15000 }).catch(() => {});
    await sleep(POST_LOAD_DELAY);

    const content = await page.evaluate(() => {
      const posts = [];
      document.querySelectorAll('.cooked, .topic-body').forEach(el => posts.push(el.innerText));
      return posts.join('\n---\n');
    });

    results.push({
      title: t.title,
      url: t.url,
      content: content.substring(0, 2000),
    });
  }

  browser.disconnect();

  // Output to stdout for AI to process
  console.log(JSON.stringify(results, null, 2));
}

main().catch(e => {
  console.error('Error:', e.message);
  process.exit(1);
});
