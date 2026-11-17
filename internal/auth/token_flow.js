// Microsoft Teams sign-in flow, run by teams-cli on a pinned Electron
// runtime (see token_flow_embedded.go). It opens the Teams login page,
// captures the three OAuth tokens teams-cli needs from the redirect, and
// writes them to $FOSSTEAMS_TOKEN_DIR as token-{teams,skype,chatsvcagg}.jwt.
// The saved SSO cookies make later runs (automatic token refresh) complete
// without user interaction.
//
// `electron token_flow.js logout` clears the saved session first, so the
// window asks for an account again.
//
// Only Electron and Node built-ins are used: the binary embeds this one
// file and ships no node_modules. Electron 12 bundles Node 14.16, which
// predates crypto.randomUUID and the base64url Buffer encoding.
//
// Derived from fossteams/teams-token (MIT, Copyright (c) 2021 Denys Vitali).
'use strict';

const { BrowserWindow, Menu, app, session } = require('electron');
const { randomBytes } = require('crypto');
const { homedir } = require('os');
const { mkdirSync, writeFileSync } = require('fs');
const https = require('https');
const path = require('path');

const DEBUG = process.env.NODE_ENV === 'debug';
const MICROSOFT_TENANT_ID = 'f8cdef31-a31e-4b4a-93e4-5f571e91255a';
const TEAMS_APP_ID = '5e3ce6c0-2b1f-4285-8d4b-75ee78787346';
const SKYPE_RESOURCE = 'https://api.spaces.skype.com';
const CHAT_SVC_AGG_RESOURCE = 'https://chatsvcagg.teams.microsoft.com';
const REDIRECT_URI = 'https://teams.microsoft.com/go';
const USER_AGENT = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) MicrosoftTeams-Preview/1.4.00.7556 Chrome/80.0.3987.163 Electron/8.5.5 Safari/537.36';

// The flow requests each token in turn: teams (id_token), then skype, then
// chatsvcagg (access tokens for those resources).
const RESOURCES = { skype: SKYPE_RESOURCE, chatsvcagg: CHAT_SVC_AGG_RESOURCE };

const MAX_LOAD_FAILURE_RETRIES = 3;
const LOAD_DEADLINE_MS = 20000;
const MAX_BLANK_RELOADS = 2;
const MAX_TOKEN_RESPONSES = 5;

let win = null;
let currentTenant = null;
let tokenResponseCount = 0;
let loadFailures = 0;

function uuidv4() {
  const b = randomBytes(16);
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const h = b.toString('hex');
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

// decodeJwt reads the claims without verifying the signature (the tokens
// come straight from login.microsoftonline.com over TLS). Node's base64
// decoder accepts the URL-safe alphabet.
function decodeJwt(token) {
  const payload = token && token.split('.')[1];
  if (!payload) {
    return null;
  }
  try {
    const claims = JSON.parse(Buffer.from(payload, 'base64').toString('utf8'));
    return typeof claims === 'object' && claims !== null ? claims : null;
  } catch (err) {
    return null;
  }
}

function loginURL(type, tenantId) {
  const url = new URL(`https://login.microsoftonline.com/${tenantId}/oauth2/authorize`);
  const state = uuidv4();
  if (type === 'teams') {
    url.searchParams.append('response_type', 'id_token');
    url.searchParams.append('state', state);
  } else {
    url.searchParams.append('response_type', 'token');
    url.searchParams.append('state', `${state}|${RESOURCES[type]}`);
    url.searchParams.append('resource', RESOURCES[type]);
  }
  url.searchParams.append('client_id', TEAMS_APP_ID);
  url.searchParams.append('client-request-id', uuidv4());
  url.searchParams.append('redirect_uri', REDIRECT_URI);
  url.searchParams.append('x-client-SKU', 'Js');
  url.searchParams.append('x-client-Ver', '1.0.9');
  url.searchParams.append('nonce', uuidv4());
  return url.toString();
}

function load(url, label) {
  // loadURL rejects when the navigation fails; without this catch the
  // rejection is swallowed and the window sits blank. Retries are driven
  // by did-fail-load.
  win.loadURL(url, { userAgent: USER_AGENT }).catch((err) => {
    console.error(`${label} loadURL rejected: ${err}`);
  });
}

function authorize(type, tenantId) {
  console.log(`Authorizing ${type} with tenantId=${tenantId}`);
  loadFailures = 0;
  load(loginURL(type, tenantId), 'authorize');
}

// A failed main-frame load (DNS blip, TLS error, offline Wi-Fi) would leave
// the window blank with no way out. Retry a few times with backoff, then
// give up loudly so teams-cli surfaces the reason instead of timing out.
// ERR_ABORTED (-3) is our own webContents.stop() or a preventDefault'd
// redirect, never a real failure.
function attachLoadFailureHandling() {
  win.webContents.on('did-fail-load', (_e, errorCode, errorDescription, validatedURL, isMainFrame) => {
    if (!isMainFrame || errorCode === -3) {
      return;
    }
    loadFailures += 1;
    console.error(`Login load failed (${errorCode}: ${errorDescription}) for ${validatedURL}`);
    if (loadFailures > MAX_LOAD_FAILURE_RETRIES) {
      console.error(`Giving up after ${MAX_LOAD_FAILURE_RETRIES} failed login loads: ${errorCode} ${errorDescription}`);
      app.exit(1);
      return;
    }
    const attempt = loadFailures;
    setTimeout(() => {
      if (win !== null && !win.isDestroyed()) {
        console.log(`Retrying login load (attempt ${attempt}/${MAX_LOAD_FAILURE_RETRIES})`);
        load(validatedURL, 'retry');
      }
    }, 1000 * attempt);
  });

  // A renderer crash also blanks the window without any other event.
  // Reload once, then bail out; a failed reload surfaces via did-fail-load.
  let rendererReloaded = false;
  win.webContents.on('render-process-gone', (_e, details) => {
    console.error(`Renderer gone (${details.reason}, exitCode=${details.exitCode})`);
    if (rendererReloaded) {
      console.error('Renderer crashed twice; giving up');
      app.exit(1);
      return;
    }
    rendererReloaded = true;
    win.webContents.reload();
  });
}

// Blank-page watchdog. A navigation can also wedge silently: no dom-ready,
// no did-fail-load (stalled TLS handshake, blackholed socket, SPA that
// boots but never paints). If the page has not finished loading within the
// deadline, reload it a bounded number of times; a rendered page awaiting
// human MFA input is left alone.
function attachBlankPageWatchdog() {
  let blankReloads = 0;
  let timer;
  let lastUrl = '';

  const arm = () => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      if (win === null || win.isDestroyed() || !win.webContents.isLoading()) {
        return;
      }
      blankReloads += 1;
      if (blankReloads > MAX_BLANK_RELOADS) {
        console.error(`Giving up: login page stuck blank after ${MAX_BLANK_RELOADS} reloads`);
        app.exit(1);
        return;
      }
      console.error(`Login page still blank after ${LOAD_DEADLINE_MS}ms; re-loading ${lastUrl} (attempt ${blankReloads}/${MAX_BLANK_RELOADS})`);
      // reload() on a navigation that never committed fails with an
      // empty-URL did-fail-load (-300); re-hitting the URL captured at
      // did-start-navigation works for pending loads too.
      if (lastUrl) {
        load(lastUrl, 'watchdog');
      }
    }, LOAD_DEADLINE_MS);
  };

  win.webContents.on('did-start-navigation', (_e, url, isInPlace, isMainFrame) => {
    if (isMainFrame && !isInPlace) {
      lastUrl = url;
      arm();
    }
  });
  win.webContents.on('did-finish-load', () => clearTimeout(timer));
  // Only hand the deadline off when the retry loop takes ownership
  // (main-frame, non-abort failure).
  win.webContents.on('did-fail-load', (_e, errorCode, _desc, _url, isMainFrame) => {
    if (isMainFrame && errorCode !== -3) {
      clearTimeout(timer);
    }
  });
  arm();
}

// Standard macOS Edit menu. macOS attaches its system AutoFill submenu
// (Passwords app / iCloud Keychain, with Touch ID approval) to the Edit
// menu of any app that has one, and it gives the form working ⌘C/⌘V/⌘A.
// Electron 12 does not build that menu by default.
function installEditMenu() {
  if (process.platform === 'darwin') {
    Menu.setApplicationMenu(Menu.buildFromTemplate([{ role: 'appMenu' }, { role: 'editMenu' }]));
  }
}

function saveToken(type, token) {
  // teams-cli sets FOSSTEAMS_TOKEN_DIR to the directory it reads.
  const dir = process.env.FOSSTEAMS_TOKEN_DIR
    ? path.resolve(process.env.FOSSTEAMS_TOKEN_DIR)
    : path.join(homedir(), '.config', 'fossteams');
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, `token-${type}.jwt`), token, { mode: 0o600 });
  console.log(`Got a ${type} token`);
}

function getTenants(token) {
  return new Promise((resolve, reject) => {
    const req = https.get('https://teams.microsoft.com/api/mt/emea/beta/users/tenants', {
      headers: { Authorization: `Bearer ${token}` },
    }, (res) => {
      let body = '';
      res.setEncoding('utf8');
      res.on('data', (chunk) => { body += chunk; });
      res.on('end', () => {
        if (res.statusCode !== 200) {
          reject(new Error(`tenants request failed: ${res.statusCode}`));
          return;
        }
        try {
          resolve(JSON.parse(body));
        } catch (err) {
          reject(err);
        }
      });
    });
    req.on('error', reject);
  });
}

// handleRedirect extracts the token from the redirect URL fragment, saves
// it, and requests the next one; the last one ends the flow.
async function handleRedirect(url) {
  const params = new URLSearchParams(url.replace(`${REDIRECT_URI}#`, ''));
  const token = params.get('id_token') || params.get('access_token');
  const claims = decodeJwt(token);
  if (claims === null) {
    console.warn(`Invalid JWT provided: ${params}`);
    if (params.has('error')) {
      console.error(`Got error = ${params.get('error')}: ${params.get('error_description')}`);
      if (params.get('error') === 'interaction_required') {
        console.error('User interaction is required (e.g: MFA).');
      }
    }
    return;
  }

  if (claims.tid === MICROSOFT_TENANT_ID && claims.aud === SKYPE_RESOURCE) {
    // A personal Microsoft account: pick the first tenant it belongs to.
    console.log(`Tenant ID is MICROSOFT: aud=${claims.aud}`);
    try {
      const tenants = await getTenants(token);
      currentTenant = tenants[0].tenantId;
      authorize('skype', currentTenant);
    } catch (err) {
      console.error(`Unable to get tenants: ${err}`);
    }
    return;
  }

  if (currentTenant === null) {
    // A company account pins its own tenant; anything else stays common.
    currentTenant = claims.tid && claims.tid !== MICROSOFT_TENANT_ID ? claims.tid : 'common';
  }

  tokenResponseCount += 1;
  win.webContents.stop();
  if (tokenResponseCount > MAX_TOKEN_RESPONSES) {
    console.error('Redirecting too many times, stopping');
    return;
  }

  console.log(`Audience: ${claims.aud}`);
  if (DEBUG) {
    console.log('Decoded', claims);
  }

  switch (claims.aud) {
    case TEAMS_APP_ID:
      saveToken('teams', token);
      authorize('skype', currentTenant);
      break;
    case SKYPE_RESOURCE:
      saveToken('skype', token);
      authorize('chatsvcagg', currentTenant);
      break;
    case CHAT_SVC_AGG_RESOURCE:
      saveToken('chatsvcagg', token);
      win.destroy();
      app.quit();
      break;
    default:
      console.error(`Invalid audience ${claims.aud} found.`);
  }
}

// Disable GPU sandbox for WSL2/wslg compatibility.
app.commandLine.appendSwitch('disable-gpu-sandbox');

app.whenReady().then(async () => {
  if (process.argv.includes('logout')) {
    await session.defaultSession.clearStorageData();
  }

  win = new BrowserWindow({ width: 800, height: 600 });
  if (DEBUG) {
    win.webContents.openDevTools();
  }
  installEditMenu();
  attachLoadFailureHandling();
  attachBlankPageWatchdog();

  win.webContents.on('will-navigate', (e, url) => {
    if (url.startsWith('https://teams.microsoft.com/')) {
      e.preventDefault();
    }
  });
  // Teams server-redirects /go to /error/eoa, so the token URL has to be
  // caught in will-redirect, before the bounce happens.
  win.webContents.on('will-redirect', async (e, url) => {
    if (url.startsWith(REDIRECT_URI)) {
      e.preventDefault();
      await handleRedirect(url);
    }
  });
  // Safety net for a token URL loaded without an intermediate redirect.
  win.webContents.on('did-navigate', async (_e, url) => {
    if (url.startsWith(REDIRECT_URI)) {
      await handleRedirect(url);
    }
  });

  authorize('teams', 'common');
});
