# Publishing runbook

Everything is built and tested locally. This is the ordered checklist to take
it live. Steps that create public repos or push data are marked ⚠️.

## 0. Prerequisites
- `gh auth status` shows you logged in (✓ `intUnderflow`).
- Both local repos are committed:
  - `rewardflights.lucy.sh` (this repo) — site + processor.
  - `rewardflights.lucy.sh-data` — the generated dataset.

## 1. ⚠️ Data repo (create + push first)
```sh
cd ../rewardflights.lucy.sh-data
gh repo create intUnderflow/rewardflights.lucy.sh-data \
  --public --source=. --remote=origin --push \
  --description "Web-optimized award-seat data for rewardflights.lucy.sh"
```

## 2. ⚠️ Site repo
```sh
cd ../rewardflights.lucy.sh
gh repo create intUnderflow/rewardflights.lucy.sh \
  --public --source=. --remote=origin --push \
  --description "Find award seats before they're gone — static site + open-data processor"
```

## 3. Processor watcher (on the source host)
The processor runs as a long-lived service on the host that produces the source
data, watching the local checkout and pushing the derived repo on every change.
Host-specific service config lives on that host, not in this repo. In short:

```sh
# build
cd processor && go build -o <bin path> .

# run (as a launchd/systemd service, restart-on-exit)
<bin path> -watch -push \
  -src   <local rewardflights checkout> \
  -out   <local rewardflights.lucy.sh-data checkout> \
  -token-cmd '<command that prints a short-lived git push token>'
```

- The `-token-cmd` output is used as an `x-access-token` for the push, so no
  ambient git credentials are needed. Any token/helper with write access to the
  data repo works.
- The service supervisor should restart the process if it exits; the watcher
  itself never exits on transient errors (it logs and retries).

## 4. Cloudflare Pages (the site)
1. Cloudflare dashboard → **Workers & Pages → Create → Pages → Connect to Git**.
2. Pick `intUnderflow/rewardflights.lucy.sh`.
3. Build settings: framework **None**, build command *(empty)*, output
   directory **`site`**.
4. Deploy, then add the custom domain **rewardflights.lucy.sh**.

`site/_headers` (security headers + CSP) and `site/_redirects` (SPA fallback)
are applied by Pages automatically.

## 5. Post-launch checks
- Search a city, open a route calendar, click a day.
- Network tab: `availability.json` from `raw.githubusercontent.com`, ~25 KB gz,
  `access-control-allow-origin: *`.
- Console: no CSP violations.
- Commit a change to the source repo; within seconds the data repo gets a
  `data: source <sha>` commit and the site's "data as of" updates.
