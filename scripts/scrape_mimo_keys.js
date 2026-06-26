#!/usr/bin/env node

/**
 * Scrape MiMo Token Plan keys from linux.do forum welfare section.
 *
 * Requires a running Chrome/Chromium with remote debugging enabled:
 *   /Applications/Google\ Chrome.app/Contents/MacOS/Google\ Chrome --remote-debugging-port=9222
 *
 * Usage:
 *   node scripts/scrape_mimo_keys.js                  # scrape and print results
 *   node scripts/scrape_mimo_keys.js --post-to-api    # scrape and post keys to local API
 *
 * Environment variables (for --post-to-api):
 *   API_BASE_URL          — base URL (default: http://localhost:3000)
 *   MIMO_CN_CHANNEL_ID    — channel ID for CN keys (default: 7)
 *   MIMO_SGP_CHANNEL_ID   — channel ID for SGP keys (default: 8)
 */

const puppeteer = require('puppeteer-core');

const BROWSER_URL = 'http://127.0.0.1:9222';
const LINUXDO_WELFARE_URL = 'https://linux.do/c/welfare/36';
const TOPIC_TITLE_REGEX = /mimo|小米|token|key/i;
const NAVIGATION_TIMEOUT = 30000;
const PAGE_LOAD_DELAY = 2000;
const POST_LOAD_DELAY = 1000;

// Key extraction patterns
const TP_KEY_REGEX = /tp-[a-zA-Z0-9]{20,}/g;
const SK_KEY_REGEX = /sk-[a-zA-Z0-9\-_]{20,}/g;

// URL patterns for CN vs SGP detection
const CN_URL_REGEX = /token-plan-cn\.xiaomimimo\.com/i;
const SGP_URL_REGEX = /token-plan-sgp\.xiaomimimo\.com/i;

// --- Helpers ---

/**
 * Remove Chinese characters and common obfuscation from a key string.
 * Some linux.do users insert Chinese chars into keys to evade auto-scraping.
 */
function removeObfuscation(str) {
  return str.replace(/[一-鿿㐀-䶿]/g, '');
}

/**
 * Extract tp-prefixed MiMo keys from text.
 * Handles obfuscation by removing Chinese chars first.
 */
function extractTpKeys(text) {
  const cleaned = removeObfuscation(text);
  const matches = cleaned.match(TP_KEY_REGEX) || [];
  return [...new Set(matches)];
}

/**
 * Extract sk-prefixed keys from text.
 */
function extractSkKeys(text) {
  const matches = text.match(SK_KEY_REGEX) || [];
  return [...new Set(matches)];
}

/**
 * Determine whether a post's content indicates CN or SGP (or both).
 */
function detectRegion(text) {
  return {
    isCn: CN_URL_REGEX.test(text),
    isSgp: SGP_URL_REGEX.test(text),
  };
}

/**
 * Post keys to the local API endpoint.
 */
async function postKeysToApi(baseUrl, channelId, keys) {
  if (keys.length === 0) return { success: true, message: 'no keys to add' };

  const resp = await fetch(`${baseUrl}/api/channel/multi_key/add_keys`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ channel_id: channelId, keys: keys.join('\n') }),
  });
  return resp.json();
}

/**
 * Delete auto-disabled keys for a channel.
 */
async function deleteDisabledKeys(baseUrl, channelId) {
  const resp = await fetch(`${baseUrl}/api/channel/multi_key/delete_disabled_keys`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ channel_id: channelId }),
  });
  return resp.json();
}

// --- Main ---

async function main() {
  const postToApi = process.argv.includes('--post-to-api');
  const baseUrl = process.env.API_BASE_URL || 'http://localhost:3000';
  const cnChannelId = parseInt(process.env.MIMO_CN_CHANNEL_ID || '7', 10);
  const sgpChannelId = parseInt(process.env.MIMO_SGP_CHANNEL_ID || '8', 10);

  console.log('Connecting to browser...');
  const browser = await puppeteer.connect({ browserURL: BROWSER_URL });
  const pages = await browser.pages();
  let page = pages.find(p => p.url().includes('linux.do'));
  if (!page) {
    page = await browser.newPage();
  }

  console.log('Navigating to linux.do welfare section...');
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

  console.log(`Found ${topics.length} mimo-related topics`);

  const cnKeys = [];
  const sgpKeys = [];

  for (const t of topics) {
    console.log(`Scraping: ${t.title}`);
    await page.goto(t.url, { waitUntil: 'networkidle2', timeout: 15000 }).catch(() => {});
    await sleep(POST_LOAD_DELAY);

    const content = await page.evaluate(() => {
      const posts = [];
      document.querySelectorAll('.cooked, .topic-body').forEach(el => posts.push(el.innerText));
      return posts.join('\n---\n');
    });

    const tpKeys = extractTpKeys(content);
    const skKeys = extractSkKeys(content);
    const allKeys = [...tpKeys, ...skKeys];
    const region = detectRegion(content);

    if (allKeys.length > 0) {
      console.log(`  -> Found ${tpKeys.length} tp-keys, ${skKeys.length} sk-keys`);
      console.log(`  -> Region: ${region.isCn ? 'CN' : ''}${region.isSgp ? 'SGP' : ''}${!region.isCn && !region.isSgp ? 'UNKNOWN' : ''}`);

      for (const key of allKeys) {
        if (region.isCn && !region.isSgp) {
          cnKeys.push(key);
        } else if (region.isSgp && !region.isCn) {
          sgpKeys.push(key);
        } else if (region.isCn && region.isSgp) {
          const keyIdx = content.indexOf(key);
          const surroundingText = content.substring(Math.max(0, keyIdx - 200), keyIdx + 200);
          if (SGP_URL_REGEX.test(surroundingText) && !CN_URL_REGEX.test(surroundingText)) {
            sgpKeys.push(key);
          } else {
            cnKeys.push(key);
          }
        } else {
          cnKeys.push(key);
        }
      }
    } else {
      console.log('  -> No keys found');
    }
  }

  browser.disconnect();

  const uniqueCnKeys = [...new Set(cnKeys)];
  const uniqueSgpKeys = [...new Set(sgpKeys)];

  console.log(`\n--- Results ---`);
  console.log(`CN keys: ${uniqueCnKeys.length}`);
  console.log(`SGP keys: ${uniqueSgpKeys.length}`);

  if (postToApi) {
    console.log(`\nPosting to API at ${baseUrl}...`);

    console.log('Cleaning up auto-disabled keys...');
    const cnDel = await deleteDisabledKeys(baseUrl, cnChannelId);
    console.log(`  CN: ${JSON.stringify(cnDel)}`);
    const sgpDel = await deleteDisabledKeys(baseUrl, sgpChannelId);
    console.log(`  SGP: ${JSON.stringify(sgpDel)}`);

    if (uniqueCnKeys.length > 0) {
      console.log(`Adding ${uniqueCnKeys.length} CN keys to channel ${cnChannelId}...`);
      const result = await postKeysToApi(baseUrl, cnChannelId, uniqueCnKeys);
      console.log(`  Result: ${JSON.stringify(result)}`);
    }

    if (uniqueSgpKeys.length > 0) {
      console.log(`Adding ${uniqueSgpKeys.length} SGP keys to channel ${sgpChannelId}...`);
      const result = await postKeysToApi(baseUrl, sgpChannelId, uniqueSgpKeys);
      console.log(`  Result: ${JSON.stringify(result)}`);
    }
  } else {
    console.log('\n--- CN Keys ---');
    uniqueCnKeys.forEach(k => console.log(k));
    console.log('\n--- SGP Keys ---');
    uniqueSgpKeys.forEach(k => console.log(k));
  }
}

function sleep(ms) {
  return new Promise(r => setTimeout(r, ms));
}

main().catch(e => {
  console.error('Error:', e.message);
  process.exit(1);
});
