# Publishing runbook

Everything is built and tested locally. This is the ordered checklist to take
it live. Steps that create public repos or push data are marked ⚠️ (outward-
facing, hard to reverse) — do them when you're ready to be public.

## 0. Prerequisites
- `gh auth status` shows you logged in as `intUnderflow` (✓ confirmed).
- Both local repos are committed:
  - `rewardflights.lucy.sh` (this repo) — site + processor + pipeline.
  - `rewardflights.lucy.sh-data` — the generated dataset (178 files, ODbL).

## 1. ⚠️ Create and push the data repo first
The pipeline refuses to run against an unseeded data repo (no LICENSE), so the
data repo must exist with its license + a first generation before the pipeline
turns on.

```sh
cd ../rewardflights.lucy.sh-data
gh repo create intUnderflow/rewardflights.lucy.sh-data \
  --public --source=. --remote=origin --push \
  --description "Web-optimized, ODbL award-seat data for rewardflights.lucy.sh"
```

## 2. ⚠️ Create and push the site repo
```sh
cd ../rewardflights.lucy.sh
gh repo create intUnderflow/rewardflights.lucy.sh \
  --public --source=. --remote=origin --push \
  --description "Find award seats before they're gone — static site + open-data pipeline"
```

## 3. Data-pipeline token
The scheduled Action pushes to the *data* repo, which needs a token beyond the
default `GITHUB_TOKEN` (that only grants access to the site repo).

1. Create a fine-grained PAT: **Settings → Developer settings → Fine-grained
   tokens**. Repository access: only `intUnderflow/rewardflights.lucy.sh-data`.
   Permissions: **Contents: Read and write**. Short-ish expiry + calendar reminder.
2. Add it to the *site* repo: **Settings → Secrets and variables → Actions →
   New repository secret**, name `DATA_REPO_TOKEN`.
3. Trigger a run: **Actions → Process reward-flight data → Run workflow**. It
   should skip (source SHA already processed) or push a no-op-free commit.

## 4. Cloudflare Pages (the site)
1. Cloudflare dashboard → **Workers & Pages → Create → Pages → Connect to Git**.
2. Pick `intUnderflow/rewardflights.lucy.sh`.
3. Build settings:
   - Framework preset: **None**
   - Build command: *(empty)*
   - Build output directory: **`site`**
4. Deploy. Then add the custom domain **rewardflights.lucy.sh** under the
   project's **Custom domains** tab (Cloudflare will guide the DNS record).

`site/_headers` (security headers + CSP) and `site/_redirects` (SPA fallback)
are picked up by Pages automatically.

## 5. Post-launch checks
- Visit the site: search a city, open a route calendar, click a day.
- DevTools Network: `availability.json` served from
  `raw.githubusercontent.com`, ~25 KB gzipped, `access-control-allow-origin: *`.
- DevTools Console: no CSP violations.
- Wait for a source-data change (or `Run workflow`) and confirm the data repo
  gets a `data: source <sha>` commit and the site's "data as of" updates.

## Optional hardening (deferred; format already supports)
- Second Cloudflare Pages project serving the data repo as a fallback origin
  (`data.rewardflights.lucy.sh`) if you ever want to not depend solely on
  raw.githubusercontent.com.
- Service Worker for offline browsing.
