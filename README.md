# Bitify Relay

Ek Go server: **Listen Together** (party mode), **accounts**, **Messages**, vault
aur leaderboard — wahi jo Bitify app (phone / TV / Wear / desktop) use karta hai.

> Pehle is repo se **do** service bante the: yeh Go backend aur ek `bitify-relay`
> node service (web app ka YouTube CORS middle-man). Web app hata diya gaya hai,
> isliye node relay bhi hata diya — ab blueprint sirf **ek** service banata hai.

<a href="https://render.com/deploy?repo=https://github.com/vishnu893-maker/bitify-relay">
  <img src="https://render.com/images/deploy-to-render-button.svg" alt="Deploy to Render" height="32">
</a>

## Deploy (Render)

1. Render Dashboard -> **New -> Blueprint** -> ye repo chuno.
2. Service `bitify-listen-together` ban jayegi (free plan, Singapore, root `listen-together`).
3. **Environment** me set karo:
   - `DATABASE_URL` — Neon/Postgres connection string (accounts + messages + vault).
   - `SYNC_SECRET` — lamba random string. **Ek baar set karo, kabhi mat badlo.**
   - `GOOGLE_CLIENT_ID` / `GOOGLE_DESKTOP_CLIENT_ID` — Google Sign-In ke OAuth client ids.
   - `JAM_ADMIN_KEY` — optional; `/admin` panel ke liye.
4. Verify: `/healthz` -> 200, aur `/api/auth/me` -> **401** (404 matlab purana build).

## Backup server (dusra Render account)

Ek Render account me free **750 instance-hours/month** milte hain, aur ek service
24/7 chalti hui ~**730 h** leti hai. Isliye:

- **Account A** — main server.
- **Account B (dusra id)** — wahi repo, wahi service = **backup**. Dono 24/7 chal sakte hain.

Dono pe **bilkul same** rakho (warna data tut jayega):

| Variable | Kyun same |
|---|---|
| `DATABASE_URL` | ek hi Neon DB — user data dono me identical |
| `SYNC_SECRET` | warna ek server ka sealed document dusre pe unreadable |
| `GOOGLE_CLIENT_ID`, `GOOGLE_DESKTOP_CLIENT_ID` | sign-in har server pe chale |

App pehle primary try karta hai, jawab na aaye to backup pe fall back karta hai
(phone / TV / Wear / desktop).

> **Party ka live session** server ki memory me hota hai, DB me nahi — primary off
> hone par chal rahi party tootegi, par account / messages / vault safe rehta hai.

## Endpoints (chhota)

| Endpoint | Kya |
|---|---|
| `/healthz` | liveness ping |
| `/api/parties`, `/ws/parties/{code}` | Listen Together + party WebSocket |
| `/api/auth/*` | register / login / google / me / logout |
| `/api/messages/*`, `/ws/messages` | Messages + messages WebSocket |
| `/api/vault/{userId}`, `/api/account/sync` | vault / playlists / cross-device sync |
| `/api/stats/*` | live stats + leaderboard |
| `/admin` | admin panel (needs `JAM_ADMIN_KEY`) |

## Chalayein (locally)

```bash
cd listen-together
JAM_AUTH_MEMORY=1 JAM_ADMIN_KEY=dev-key go run .
curl -i localhost:8000/healthz
```

Server ka apna detail `listen-together/README.md` me hai.
