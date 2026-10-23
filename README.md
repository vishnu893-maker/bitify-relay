# Bitify Relay

Ek **chhota server** jo Bitify ke web version ko extension-less browsers (Safari/iPhone, Chrome/Android) me YouTube tak pahunchata hai. Extension kya karta hai browser ke andar se (header add karke), ye wahi kaam server se — CORS wala pahad.

GPLv3 jaise asli project ka hi hissa.

Kuch bhi store nahi hota — request aati hai, YouTube se jawab lekar jaata hai. Gaane ki audio is server se **nahi** guzarti (browser direct YouTube se lete hain), isliye free hosting ka bandwidth kaafi.

<a href="https://render.com/deploy?repo=https://github.com/vishnu893-maker/bitify-relay/create-render-deploy">
  <img src="https://render.com/images/deploy-to-render-button.svg" alt="Deploy to Render" height="32">
</a>

## Kya ye karta hai

| Endpoint | Kahan jata hai | Kiske liye |
|---|---|---|
| `/yt/*` | `music.youtube.com/*` | YouTube Music ke InnerTube calls (search, browse, player) |
| `/wy/*` | `www.youtube.com/*` | Session bootstrap + player script |
| `/gv?u=<URL>` | `*.googlevideo.com` | SABR/audio segments (tab jab ye path chalu ho) |
| `/healthz` | — | Wake-up ping |

## Chalayein

```bash
node server.mjs            # PORT env, default 8787
# test
curl -i localhost:8787/healthz
```

## Deploy kaise (Render)

1. Upar wala **Deploy to Render** dabao (ya Render pe New Web Service → repo)
2. Plan: **Free** — server so jaye to khud uth jayega (app khud warm-ping karega)
3. Mili URL (`https://bitify-relay.onrender.com` jaise) → Bitify web ke **Settings › Bitify Relay** me daal do

