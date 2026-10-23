# Bitify Relay

Ek chhota server + ek blueprint file: **Render pe 1 click me do cheezein deploy ho jaati hain** —
1. `bitify-relay` — YouTube ka CORS middle-man (Bitify web ka search/data, extension-less browsers ke liye)
2. `bitify-listen-together` — Listen Together ka server (party mode ka Go backend)

Dono free plan pe so-jate/khut-jate hain, koi data store nahi karte, aurGPLv3 ke under hain.

<a href="https://render.com/deploy?repo=https://github.com/vishnu893-maker/bitify-relay/create-render-deploy">
  <img src="https://render.com/images/deploy-to-render-button.svg" alt="Deploy to Render" height="32">
</a>

## Deploy karne ke baad

1. Render deta do URLs dega (donon services ke) tab news do sab tak
2. **Bitify web app** me Settings › **Bitify Relay** me relay ka URL daal do (ya build me `VITE_RELAY_URL` se bake karo)
3. **Bitify app (Android/PC/web)** me Listen Together settings me apna naya server daal do

## Kya ye karta hai

| Endpoint | Kahan jata hai | Kiske liye |
|---|---|---|
| `/yt/*` | `music.youtube.com/*` | YouTube Music ke InnerTube calls (search, browse, player) |
| `/wy/*` | `www.youtube.com/*` | Session bootstrap + player script |
| `/gv?u=<URL>` | `*.googlevideo.com` | SABR/audio segments (sırф jab ye path chalu ho) |
| `/healthz` | — | Wake-up ping sab jagah |

**Privacy:** relay sirf request forward karta hai — koi storage nahi, koi cookie nahi. Gaane ki audio server se **nahi** guzarti (browser direct YouTube se lete hain), isliye free hosting ka bandwidth kaafi.

## Chalayein (locally, testing ke liye)

```bash
node server.mjs            # PORT env, default 8787
# test
curl -i localhost:8787/healthz
```

Listen Together ka server `listen-together/` folder me hai — wahan ka apna `README.md` file dekho.
