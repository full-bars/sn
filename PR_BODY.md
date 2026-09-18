## What

Completes the release prep for the queued `v2026.9.17-1789646883-meso` tag. The notes file existed but predated four changes that landed after it was drafted:

- DoH server-score cache removal (`01b8e5f5`) — live-fleet probe showed it inert under v2026 connect
- Docker entrypoint jwt build autodetect (`77e215ca`)
- Installer regression suite port (PR `#6`)
- Wiki ported into `docs/` (PR `#5`)

Updates `releases/v2026.9.17-1789646883-meso.md` (new section 8 + What's Changed + deploy notes), the CHANGELOG entry, and corrects FORK_CHANGES section 9, which claimed the removed cache was shipped.

## Ship plan

After merge: `git tag -s v2026.9.17-1789646883-meso` on main and push — release.yml picks up `releases/<tag>.md`.
