#!/usr/bin/env node

/**
 * Scrape MiMo Token Plan posts from linux.do forum welfare section.
 * Uses puppeteer to navigate pages (handles Cloudflare).
 * Outputs raw post content as JSON — key extraction is done by AI (Claude).
 *
 * Requires a running Chrome/Edge with remote debugging enabled:
 *   --remote-debugging-port=9222
 *
 * Usage:
 *   node scripts/scrape_mimo_posts.js              # default: scrape ~2 pages of topics
 *   node scripts/scrape_mimo_posts.js --pages=5    # scrape more pages
 */

const puppeteer = require('puppeteer-core');

const BROWSER_URL = 'http://127.0.0.1:9222';
const LINUXDO_WELFARE_URL = 'https://linux.do/c/welfare/36';
const TOPIC_TITLE_REGEX = /mimo|小米|token|key/i;
const NAVIGATION_TIMEOUT = 30000;
const PAGE_LOAD_DELAY = 2000;
const POST_LOAD_DELAY = 1000;
const SCROLL_DELAY = 1500;

const pagesArg = process.argv.find(a => a.startsWith('--pages='));
const MAX_PAGES = pagesArg ? parseInt(pagesArg.split('=')[1], 10) : 2;

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

  // Navigate to welfare category
  console.error('Navigating to linux.do welfare section...');
  await page.goto(LINUXDO_WELFARE_URL, { waitUntil: 'networkidle2', timeout: NAVIGATION_TIMEOUT }).catch(() => {});
  await sleep(PAGE_LOAD_DELAY);

  // Collect topics across multiple pages by scrolling to load more (Discourse infinite scroll)
  const allTopics = [];
  const seenUrls = new Set();

  for (let pageNum = 0; pageNum < MAX_PAGES; pageNum++) {
    console.error(`Scanning page ${pageNum + 1}/${MAX_PAGES}...`);

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

    for (const t of topics) {
      if (!seenUrls.has(t.url)) {
        seenUrls.add(t.url);
        allTopics.push(t);
      }
    }

    console.error(`  Found ${topics.length} matching topics (${allTopics.length} total unique)`);

    // Scroll down to trigger Discourse infinite scroll for next page
    if (pageNum < MAX_PAGES - 1) {
      const moreLoaded = await page.evaluate(async () => {
        const before = document.querySelectorAll('.topic-list-item').length;
        // Click "more topics" button if present, or scroll to bottom
        const moreBtn = document.querySelector('.more-topics a, .btn-more');
        if (moreBtn) {
          moreBtn.click();
          await new Promise(r => setTimeout(r, 2000));
        } else {
          window.scrollTo(0, document.body.scrollHeight);
          await new Promise(r => setTimeout(r, 2000));
        }
        const after = document.querySelectorAll('.topic-list-item').length;
        return after > before;
      });

      if (!moreLoaded) {
        console.error('  No more topics to load, stopping pagination');
        break;
      }
      await sleep(SCROLL_DELAY);
    }
  }

  console.error(`\nTotal topics to scrape: ${allTopics.length}`);

  // Visit each topic and extract content
  const results = [];
  for (let i = 0; i < allTopics.length; i++) {
    const t = allTopics[i];
    console.error(`[${i + 1}/${allTopics.length}] ${t.title}`);
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
  console.log(JSON.stringify(results, null, 2));
}

main().catch(e => {
  console.error('Error:', e.message);
  process.exit(1);
});
