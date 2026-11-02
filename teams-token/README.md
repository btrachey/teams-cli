# teams-token (vendored)

A vendored copy of [fossteams/teams-token](https://github.com/fossteams/teams-token)
(MIT, © Denys Vitali) — the Electron login flow that mints the Microsoft Teams
JWT files (`token-teams.jwt`, `token-skype.jwt`, `token-chatsvcagg.jwt`) that
teams-cli reads. It lives here so token capture and automatic token refresh
are maintained in one repository.

## Setup (once)

```bash
yarn install
```

## Manual use

```bash
yarn start          # build + open the login window, save tokens
yarn start logout   # clear the Electron session (switch users)
yarn start get-url  # print the login URL
```

## Local modifications

- `saveTeamsToken` honors `FOSSTEAMS_TOKEN_DIR` so teams-cli can point the
  flow at the directory it actually reads (`--token-dir`); without the env
  var the historical `~/.config/fossteams` default applies.
- The `will-redirect` handler (upstream fix) catches tokens before Teams
  server-redirects `/go` to `/error/eoa`.

teams-cli launches `node_modules/electron` against `dist/main.js`
automatically when stored tokens expire; run `node_modules/.bin/tsc` after
editing `src/` so `dist/` matches.
