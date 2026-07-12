# rewardflights.lucy.sh

**[rewardflights.lucy.sh](https://rewardflights.lucy.sh)** — find award seats
before they're gone. A fully static website over fully open data: a live
calendar of British Airways reward-flight (Avios) availability on every route.

## How it works

```
intUnderflow/rewardflights            open source-of-truth dataset
        │   watched live on the host that produces it
        ▼
processor/                            Go; deterministic transform, runs as a
        │                             constantly-running watcher
        ▼
intUnderflow/rewardflights.lucy.sh-data   web-optimized derived data
        │   fetched straight from raw.githubusercontent.com by the browser
        ▼
site/                                 static SPA on Cloudflare Pages
```

The processor runs continuously as a watcher over a local `rewardflights`
checkout — the moment a change is committed there, the derived data is
regenerated and pushed, usually within a couple of seconds.

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
# processor — one-shot transform
cd processor && go test ./... && go run . -src ../../rewardflights -out /tmp/data-out

# processor — watch mode (what runs in production)
go run . -watch -commit -src ../../rewardflights -out /tmp/data-out

# site — any static server with SPA fallback; point it at local data:
#   http://localhost:8790/?data=http://localhost:8790/data
```

## Deployment

- **Processor**: build the binary and run it in watch mode as a long-lived
  service (launchd / systemd / etc.) on the host that produces the source data:
  ```sh
  processor -watch -push -src <rewardflights checkout> -out <data-repo checkout> \
            -token-cmd '<command that prints a git push token>'
  ```
  Host-specific service config lives on that host, not in this repo.
- **Site**: Cloudflare Pages project, build command none, output directory `site/`.

## License

**Code** (this repo — the processor and the site) is licensed
[CC BY-NC-SA 4.0](https://creativecommons.org/licenses/by-nc-sa/4.0/): share
and adapt with attribution, non-commercially, share-alike.

The bundled fonts (B612, Archivo Black) are under the SIL Open Font License —
see [`site/assets/fonts/OFL.txt`](site/assets/fonts/OFL.txt).

The **derived data** in `intUnderflow/rewardflights.lucy.sh-data` carries no
license of its own; each file embeds a `source` provenance note pointing back
to [intUnderflow/rewardflights](https://github.com/intUnderflow/rewardflights).
