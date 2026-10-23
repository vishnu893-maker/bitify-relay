# Bitify Relay

Ek **chhota server** jo Bitify ke web version ko extension-less browsers (Safari/iPhone, Chrome/Android) me YouTube tak pahunchata hai. Extension kya karta hai browser ke andar se (header add karke), ye wahi kaam server se — CORS wala pahad.

GPLv3 jaise asli project ka hi hissa.

## Kya ye karta hai

| Endpoint | Kahan jata hai | Kiske liye |
|---|---|---|
| `/yt/*` | `music.youtube.com/*` | YouTube Music ke InnerTube calls (search, browse, player) |
| `/wy/*` | `www.youtube.com/*` | Session bootstrap + player script |
| `/gv?u=<URL>` | `*.googlevideo.com` | SABR/audio segments (tab jab ye path chalu ho) |
| `/healthz` | — | Wake-up ping |

**Privacy / bandwidth:** koi storage nahi, koi cookie nahi, kuch bhi save nahi — sirf aage pass karta hai. Gaane ki audio by default **direct** jati hai (browser se <audio> element pe), server ka bandwidth nahi khata.

## Chalayein

```bash
node server.mjs            # PORT env, default 8787
# test
curl -i localhost:8787/healthz
```

## Deploy kaise (Render)

1. README wala **Deploy to Render** button dabao (ya Render pe New Web Service → repo)
2. Plan: **Free** — server so jaye ga bhi khida uth jayega (app khud ping karega)
3. Mili URL `https://bitify-relay.onrender.com` jaise → web app ke Settings › **Bitify Relay** me daal do (ya build me `VITE_RELAY_URL` se bake karo)
