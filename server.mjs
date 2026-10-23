/**
 * Bitify Relay — the small middle-man that lets a plain web page do what the
 * Bitify extension does (web/extension/lib/rules.ts), for browsers that can't
 * install extensions: Safari on iOS, Chrome on Android.
 *
 * The problem it solves is CORS: pages aren't allowed to send YouTube's
 * Origin/Referer/X-Origin headers, and YouTube refuses cross-origin reads. The
 * extension patched the browser; the relay does the same from a server.
 *
 * What it mirrors (all measured, not assumed — see the extension's rules):
 *   /yt/*    music.youtube.com/*   InnerTube as the YouTube Music web client
 *   /wy/*    www.youtube.com/*     InnerTube, the session bootstrap and the
 *                                  player script YouTube.js deciphers with —
 *                                  also www.youtube.com/api/jnn/* (BotGuard's
 *                                  Create and GenerateIT: the integrity token
 *                                  needs YouTube's own Origin, which only the
 *                                  relay can claim here; the page's real
 *                                  browser runs the BotGuard program itself)
 *   /gv?u=<full URL>               googlevideo bytes (SABR JSON and media),
 *                                  unless c=VISIONOS, fetched with no origin
 *
 * It is stateless and anonymous. No cookies are stored, echoed or forwarded;
 * the user's own session ("signed in" requests) stays an extension-only
 * feature, and the relay never sees it.
 *
 *   node server.mjs                          (PORT, default 8787)
 *   node server.mjs &  curl -i localhost:8787/healthz
 */

import http from 'node:http';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';

const PORT = Number(process.env.PORT ?? 8787);

/** The buckets the paths map to. Each adds exactly what the extension's DNR rules add. */
const BUCKETS = {
  '/yt/': { host: 'music.youtube.com', origin: 'https://music.youtube.com' },
  '/wy/': { host: 'www.youtube.com', origin: 'https://www.youtube.com' },
};

/** Browser-like UA: googlevideo asks one of YouTube's own web clients. */
const UA =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36';

/** Browser-to-browser headers worth carrying over; everything else is relay business.
 *  x-goog-api-key / x-user-agent are BotGuard's (api/jnn GenerateIT): without them
 *  www.youtube.com refuses to mint an integrity token. */
const PASSTHROUGH = [
  'accept', 'accept-language', 'content-type', 'content-language', 'range',
  'x-goog-api-key', 'x-user-agent',
];

/** The new CORS answers. Anonymous requests only, so `*` is correct everywhere. */
const CORS_HEADERS = {
  'access-control-allow-origin': '*',
  'access-control-expose-headers': '*',
  'access-control-allow-headers': '*',
  'access-control-allow-methods': 'GET, POST, HEAD, OPTIONS',
  'access-control-max-age': '86400',
};

/** Hop-by-hop and replaced headers never cross the relay. */
const DROP_RESPONSE_HEADERS = new Set([
  'connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization',
  'te', 'trailer', 'transfer-encoding', 'upgrade',
  // undici's fetch decompresses, so the old encodings/lengths are lies here.
  'content-encoding', 'content-length',
]);

function readBody(req, limit = 10 * 1024 * 1024) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on('data', (chunk) => {
      size += chunk.length;
      if (size > limit) {
        reject(new Error('request too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(size ? Buffer.concat(chunks) : null));
    req.on('error', reject);
  });
}

/** googlevideo over the relay: full URL under `u`, guarded to Google's video hosts. */
function googlevideoTarget(url) {
  const raw = url.searchParams.get('u');
  if (!raw) return null;
  let target;
  try {
    target = new URL(raw);
  } catch {
    return null;
  }
  if (!/(^|\.)googlevideo\.com$/.test(target.hostname)) return null;
  return target;
}

const server = http.createServer(async (req, res) => {
  const started = Date.now();
  let status = 500;
  let url;

  try {
    // CORS preflight: the page is allowed to ask first (SABR sends custom headers).
    if (req.method === 'OPTIONS') {
      res.writeHead(204, CORS_HEADERS);
      res.end();
      return;
    }

    const url = new URL(req.url, `http://${req.headers.host ?? 'localhost'}`);
    if (url.pathname === '/healthz') {
      status = 204;
      res.writeHead(204, CORS_HEADERS);
      res.end();
      return;
    }

    const body = await readBody(req);
    const headers = { 'user-agent': UA };
    for (const name of PASSTHROUGH) {
      const value = req.headers[name];
      if (value) headers[name] = value;
    }

    let target;
    const bucket = BUCKETS[url.pathname.slice(0, 4)];
    if (bucket) {
      // InnerTube-style: a YouTube origin, exactly as the extension's rules write it.
      target = new URL(`https://${bucket.host}${url.pathname.slice(3)}${url.search}`);
      headers['origin'] = bucket.origin;
      headers['referer'] = `${bucket.origin}/`;
      headers['x-origin'] = bucket.origin;
    } else if (url.pathname === '/gv') {
      target = googlevideoTarget(url);
      if (!target) {
        status = 403;
        res.writeHead(403, { ...CORS_HEADERS, 'content-type': 'text/plain' });
        res.end('googlevideo URLs only, encoded in ?u=');
        return;
      }
      // A server fetch has no Origin of its own, which is the "rule 7"
      // treatment VISIONOS links need outright; the rest dress as YouTube web.
      if (!/c=VISIONOS|&pot=/.test(target.search)) {
        headers['origin'] = 'https://www.youtube.com';
        headers['referer'] = 'https://www.youtube.com/';
      }
    } else {
      status = 404;
      res.writeHead(404, { ...CORS_HEADERS, 'content-type': 'text/plain' });
      res.end('the relay serves /yt/*, /wy/* and /gv?u=… (Bitify Relay)');
      return;
    }

    const upstream = await fetch(target, {
      method: req.method,
      headers,
      ...(body === null ? {} : { body }),
      redirect: 'follow',
    });

    const out = {};
    upstream.headers.forEach((value, name) => {
      if (!DROP_RESPONSE_HEADERS.has(name)) out[name] = value;
    });
    Object.assign(out, CORS_HEADERS);

    status = upstream.status;
    res.writeHead(status, out);
    if (!upstream.body || req.method === 'HEAD') {
      res.end();
      return;
    }
    await pipeline(Readable.fromWeb(upstream.body), res);
  } catch (error) {
    if (!res.headersSent) {
      res.writeHead(502, { ...CORS_HEADERS, 'content-type': 'text/plain' });
      status = 502;
      res.end(`the relay could not reach YouTube: ${String(error && error.message ? error.message : error)}`);
    } else {
      res.destroy();
    }
  } finally {
    const ms = Date.now() - started;
    const size = Number(res.getHeader('content-length') ?? 0);
    console.log(`${req.method} ${url?.pathname ?? req.url} -> ${status} (${ms}ms${size ? `, ${Math.round(size / 1024)}KB` : ''})`);
  }
});

server.listen(PORT, () => {
  console.log(`Bitify Relay on :${PORT} — /yt/ music.youtube.com, /wy/ www.youtube.com, /gv googlevideo`);
});
