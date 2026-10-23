// End-to-end relay probe: drives youtubei.js THROUGH the relay exactly like a
// page would, then plays a snippet of googlevideo through /gv. Confirms the
// relay's three buckets are right before touching the app's code.
import { Innertube } from 'youtubei.js/web';

const RELAY = 'http://localhost:8799';

/**
 * The page's relay fetch: every YouTube host request becomes a relay request
 * with its bucket path. This is the function web/app will use.
 */
function relayFetch(input, init) {
  const url = new URL(typeof input === 'string' || input instanceof URL ? input.toString() : input.url);
  if (url.hostname === 'music.youtube.com') return fetch(`${RELAY}/yt${url.pathname}${url.search}`, init);
  if (url.hostname === 'www.youtube.com') return fetch(`${RELAY}/wy${url.pathname}${url.search}`, init);
  return fetch(input, init);
}

const { Platform } = await import('youtubei.js/web');
Platform.shim.eval = async (data, env) => {
  const properties = [];
  if (env.n) properties.push(`n: exportedVars.nFunction(${JSON.stringify(env.n)})`);
  if (env.sig) properties.push(`sig: exportedVars.sigFunction(${JSON.stringify(env.sig)})`);
  return new Function(`${data.output}\nreturn { ${properties.join(', ')} }`)();
};

const yt = await Innertube.create({ fetch: relayFetch, retrieve_player: true });
console.log('[boot] InnerTube session through relay OK —', yt.session.context.client.clientName);

// 1) Search through the relay (browse path an extension-less page needs).
const search = await yt.search('arijit singhضافة tu hi haqeeqat'.slice(0, 12));
console.log('[search] results:', search.results?.length ?? 'none', search.results?.[0]?.title?.text ?? '');

// 2) Resolve + decipher a track: the URL that should appear for playback.
const info = await yt.getInfo('dQw4w9WgXcQ');
const audio = info.streaming_data?.adaptive_formats?.find((f) => f.mime_type?.startsWith('audio') && f.url);
const formats = info.streaming_data?.adaptive_formats ?? [];
console.log('[info] formats:', formats.length, 'with-url:', formats.filter((f) => f.url).length);

// 3) Play bytes through /gv.
if (audio?.url) {
  const gvRes = await fetch(`${RELAY}/gv?u=${encodeURIComponent(audio.url)}`, { headers: { Range: 'bytes=0-1023' } });
  const buf = Buffer.from(await gvRes.arrayBuffer());
  console.log(`[play] ${gvRes.status}, ${buf.length} bytes, content-type=${gvRes.headers.get('content-type')}, content-range=${gvRes.headers.get('content-range') ?? '-'}`);
} else {
  console.log('[play] no deciphered url — PoToken/gating may be needed (botguard step in-page handles it)');
}
