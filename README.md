# rewardflights.lucy.sh

**[rewardflights.lucy.sh](https://rewardflights.lucy.sh)** — find award seats
before they're gone. A fully static website over fully open data: a live
calendar of British Airways reward-flight (Avios) availability on every route.

## How it works

```
intUnderflow/rewardflights            open source-of-truth dataset (ODbL)
        │   polled every 30 min by GitHub Actions (in this repo)
        ▼
processor/                            Go; deterministic transform
        ▼
intUnderflow/rewardflights.lucy.sh-data   web-optimized derived data (ODbL)
        │   fetched straight from raw.githubusercontent.com by the browser
        ▼
site/                                 static SPA on Cloudflare Pages
```

There is no server and no database: the site is static files, and the data is
a git repo served by GitHub's CDN. The whole availability dataset compresses
to a few tens of KB, so the browser loads it once and every interaction —
search-as-you-type, route calendars, cabin filters, "everywhere from London" —
runs in memory with zero further network requests.

See [`SPEC.md`](SPEC.md) for the full architecture and data-format spec.

## Repo layout

| Path | What |
|------|------|
| `processor/` | Go program: source dataset → derived web format |
| `site/` | The static website (no build step; deploy as-is) |
| `.github/workflows/process-data.yml` | Scheduled pipeline that keeps the data repo fresh |
| `SPEC.md` | Architecture + data-format specification |

## Development

```sh
# processor
cd processor && go test ./... && go run . -src ../../rewardflights -out /tmp/data-out

# site — any static server with SPA fallback; point it at local data:
#   http://localhost:8790/?data=http://localhost:8790/data
```

## Deployment

- **Site**: Cloudflare Pages project, build command none, output directory `site/`.
- **Data pipeline**: needs a `DATA_REPO_TOKEN` repo secret (fine-grained PAT,
  `contents: write` on `rewardflights.lucy.sh-data`).

## License

Site + processor code: MIT. The underlying data is ODbL — the site displays
attribution to [intUnderflow/rewardflights](https://github.com/intUnderflow/rewardflights),
and the derived data repo is share-alike licensed under ODbL v1.0.
