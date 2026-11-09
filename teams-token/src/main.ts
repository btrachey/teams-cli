import {
  BrowserWindow, Menu, app, session,
} from 'electron';

import { homedir } from 'os';
import { writeFileSync, existsSync, mkdirSync } from 'fs';
import * as path from 'path';
import { v4 as uuidv4 } from 'uuid';
import axios, { AxiosResponse } from 'axios';
import * as jwt from 'jsonwebtoken';

const configPath = '.config/fossteams';
const DEBUG = process.env.NODE_ENV === 'debug';
const MICROSOFT_TENANT_ID = 'f8cdef31-a31e-4b4a-93e4-5f571e91255a';
const TEAMS_APP_ID = '5e3ce6c0-2b1f-4285-8d4b-75ee78787346';
const SKYPE_RESOURCE = 'https://api.spaces.skype.com';
const CHAT_SVC_AGG_RESOURCE = 'https://chatsvcagg.teams.microsoft.com';
const REDIRECT_URI = 'https://teams.microsoft.com/go';
const USER_AGENT = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) MicrosoftTeams-Preview/1.4.00.7556 Chrome/80.0.3987.163 Electron/8.5.5 Safari/537.36';

type TeamsSkype = 'teams' | 'skype' | 'chatsvcagg';

let win : BrowserWindow | null = null;
let tokenResponseCount = 0;
let currentTenant : (string | null) = null;

const tokens : Record<TeamsSkype, boolean> = {
  teams: false,
  chatsvcagg: false,
  skype: false,
};

function getLoginURL(type: TeamsSkype, tenantId: string) : string {
  const loginUrl = new URL('https://login.microsoftonline.com');
  loginUrl.pathname = `/${tenantId}/oauth2/authorize`;

  const state = uuidv4();
  switch (type) {
    case 'teams':
      loginUrl.searchParams.append('response_type', 'id_token');
      loginUrl.searchParams.append('state', state);
      break;

    case 'skype':
      loginUrl.searchParams.append('response_type', 'token');
      loginUrl.searchParams.append('state', `${state}|${SKYPE_RESOURCE}`);
      loginUrl.searchParams.append('resource', SKYPE_RESOURCE);
      break;

    case 'chatsvcagg':
      loginUrl.searchParams.append('response_type', 'token');
      loginUrl.searchParams.append('state', `${state}|${CHAT_SVC_AGG_RESOURCE}`);
      loginUrl.searchParams.append('resource', CHAT_SVC_AGG_RESOURCE);
      break;

    default:
      break;
  }
  loginUrl.searchParams.append('client_id', TEAMS_APP_ID);
  loginUrl.searchParams.append('client-request-id', uuidv4());
  loginUrl.searchParams.append('redirect_uri', REDIRECT_URI);
  loginUrl.searchParams.append('x-client-SKU', 'Js');
  loginUrl.searchParams.append('x-client-Ver', '1.0.9');
  loginUrl.searchParams.append('nonce', uuidv4());

  return loginUrl.toString();
}

const MAX_LOAD_FAILURE_RETRIES = 3;
let loadFailures = 0;

function authorize(type: TeamsSkype, tenantId: string) {
  console.log(`Authorizing ${type} with tenantId=${tenantId}`);
  loadFailures = 0;
  // loadURL rejects when the navigation fails; without this catch the
  // rejection is swallowed, the window sits blank forever and teams-cli
  // waits out its full timeout blind. Retries are driven by did-fail-load.
  win!.loadURL(getLoginURL(type, tenantId), {
    userAgent: USER_AGENT,
  }).catch((err) => {
    console.error(`loadURL rejected: ${err}`);
  });
}

// A failed main-frame load (DNS blip, TLS error, offline Wi-Fi) used to
// leave the window blank with no log and no way out. Retry a few times
// with backoff, then give up loudly so teams-cli surfaces the reason
// instead of timing out. ERR_ABORTED (-3) is our own webContents.stop()
// or a preventDefault'd redirect, never a real failure.
function attachLoadFailureHandling() {
  win!.webContents.on('did-fail-load', (_e, errorCode, errorDescription, validatedURL, isMainFrame) => {
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
      if (win === null || win.isDestroyed()) {
        return;
      }
      console.log(`Retrying login load (attempt ${attempt}/${MAX_LOAD_FAILURE_RETRIES})`);
      win.loadURL(validatedURL, { userAgent: USER_AGENT }).catch((err) => {
        console.error(`retry loadURL rejected: ${err}`);
      });
    }, 1000 * attempt);
  });

  // A renderer crash (plausible on this pinned Electron) also blanks the
  // window without any other event. Reload once, then bail out.
  let rendererReloaded = false;
  win!.webContents.on('render-process-gone', (_e, details) => {
    console.error(`Renderer gone (${details.reason}, exitCode=${details.exitCode})`);
    if (rendererReloaded) {
      console.error('Renderer crashed twice; giving up');
      app.exit(1);
      return;
    }
    rendererReloaded = true;
    // Electron 12's reload() returns void; a failed reload surfaces via
    // did-fail-load, which the retry logic above already handles.
    win!.webContents.reload();
  });
}

// Standard macOS Edit menu. This is what makes the sign-in window behave
// like a browser for passwords: macOS attaches its system AutoFill submenu
// (Edit ▸ AutoFill ▸ Passwords…, backed by the Passwords app / iCloud
// Keychain, with Touch ID approval) to the Edit menu of any app that has a
// standard one — verified in the running app via accessibility inspection.
// The pinned Electron 12 has no AutoFill API of its own and does not build
// that menu by default, so the flow installs it explicitly; it also gives
// the form working ⌘C/⌘V/⌘A.
function installEditMenu() {
  if (process.platform !== 'darwin') {
    return;
  }
  const template: Electron.MenuItemConstructorOptions[] = [
    { role: 'appMenu' },
    { role: 'editMenu' },
  ];
  Menu.setApplicationMenu(Menu.buildFromTemplate(template));
}

// Blank-page watchdog. A navigation can also wedge silently: no
// dom-ready, no did-fail-load (stalled TLS handshake, blackholed
// socket, SPA that boots but never paints). The old flow then showed a
// pure-white window for teams-cli's entire timeout. If the page has not
// finished loading within the deadline, reload it a bounded number of
// times; a rendered page awaiting human MFA input is left alone, so a
// legitimate interactive sign-in is never interrupted.
const LOAD_DEADLINE_MS = 20000;
const MAX_BLANK_RELOADS = 2;

function attachBlankPageWatchdog() {
  let blankReloads = 0;
  let timer: NodeJS.Timeout | undefined;
  let lastUrl = '';

  const arm = () => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      if (win === null || win.isDestroyed()) {
        return;
      }
      if (!win.webContents.isLoading()) {
        // Page finished (interactive login waiting on the user) — the
        // watchdog must not touch it.
        return;
      }
      blankReloads += 1;
      if (blankReloads > MAX_BLANK_RELOADS) {
        console.error(`Giving up: login page stuck blank after ${MAX_BLANK_RELOADS} reloads`);
        app.exit(1);
        return;
      }
      console.error(`Login page still blank after ${LOAD_DEADLINE_MS}ms; re-loading ${lastUrl} (attempt ${blankReloads}/${MAX_BLANK_RELOADS})`);
      if (lastUrl) {
        // reload() on a navigation that never committed fails with an
        // empty-URL did-fail-load (-300), which then poisons the
        // did-fail-load retry loop with loadURL(''). Re-hitting the URL
        // captured at did-start-navigation works for pending loads too.
        win.loadURL(lastUrl, { userAgent: USER_AGENT }).catch((err) => {
          console.error(`watchdog loadURL rejected: ${err}`);
        });
      }
    }, LOAD_DEADLINE_MS);
  };

  win!.webContents.on('did-start-navigation', (_e, url, isInPlace, isMainFrame) => {
    if (isMainFrame && !isInPlace) {
      lastUrl = url;
      arm();
    }
  });
  win!.webContents.on('did-finish-load', () => clearTimeout(timer));
  // Only hand the deadline off when the retry loop actually takes
  // ownership (main-frame, non-abort failure); -3 here is our own
  // interrupted navigation, and subframe failures must not disarm the
  // watchdog for a still-pending main frame.
  win!.webContents.on('did-fail-load', (_e, errorCode, _desc, _url, isMainFrame) => {
    if (isMainFrame && errorCode !== -3) {
      clearTimeout(timer);
    }
  });
  arm();
}

function saveTeamsToken(token: string, type: TeamsSkype) {
  // teams-cli sets FOSSTEAMS_TOKEN_DIR to the directory it actually reads
  // (e.g. --token-dir); without it, fall back to the historical default.
  const tokenDir = process.env.FOSSTEAMS_TOKEN_DIR
    ? path.resolve(process.env.FOSSTEAMS_TOKEN_DIR)
    : path.join(homedir(), configPath);
  if (!existsSync(tokenDir)) {
    mkdirSync(tokenDir, { recursive: true });
  }
  writeFileSync(`${tokenDir}/token-${type}.jwt`, token);
}

type Tenant = {
  tenantId: string;
  tenantName: string;
  userId: string;
  isInvitationRedeemed: boolean;
  userType: string;
  tenantType: string;
}

function getTenants(token: string) : Promise<AxiosResponse<Tenant[]>> {
  const req = axios.get('https://teams.microsoft.com/api/mt/emea/beta/users/tenants',
    {
      headers: {
        Authorization: `Bearer ${token}`,
      },
    });
  return req;
}

// Verifies that we've got all tokens
function checkTokens() {
  if (tokens.chatsvcagg && tokens.skype && tokens.teams) {
    win.destroy();
    app.quit();
  }
}

// Teams now server-redirects /go to /error/eoa, so the did-navigate handler
// never sees the token URL anymore. Catch the token in will-redirect, before
// the bounce happens, and stop the navigation ourselves.
async function handleRedirect(url: string) {
  const token = url.replace(`${REDIRECT_URI}#`, '');
  const searchParams = new URLSearchParams(token);
  let teamsToken = searchParams.get('id_token');

  if (teamsToken === null) {
    teamsToken = searchParams.get('access_token');
  }

  const decoded = jwt.decode(teamsToken);
  if (decoded === null) {
    console.warn(`Invalid JWT provided: ${searchParams}`);
    if (searchParams.has('error')) {
      const err = searchParams.get('error');
      const errDesc = searchParams.get('error_description');
      console.error(`Got error = ${err}: ${errDesc}`);
      switch (err) {
        case 'interaction_required':
          // User has to interact!
          console.error('User interaction is required (e.g: MFA). Are you using "?prompt=none" ?');
          break;
        default:
          // Can't handle this, sorry
      }
    }
    return;
  }

  if (typeof (decoded) === 'string') {
    console.error('Invalid decoded JWT: is a string');
    return;
  }

  if (decoded.tid === MICROSOFT_TENANT_ID && decoded.aud === SKYPE_RESOURCE) {
    // We need a tenant selection
    console.log(`Tenant ID is MICROSOFT: aud=${decoded.aud}`);
    // Get Tenant list
    try {
      const tenants = await (await getTenants(teamsToken)).data;
      // Pick the first tenant and authorize Skype
      currentTenant = tenants[0].tenantId;
      authorize('skype', currentTenant);
    } catch (err) {
      console.error(`Unable to get tenants: ${err}`);
    }
    return;
  }

  if (decoded.tid !== MICROSOFT_TENANT_ID && currentTenant === null) {
    // Company account
    currentTenant = decoded.tid;
  }

  if (currentTenant === null) {
    currentTenant = 'common';
  }

  tokenResponseCount += 1;

  if (tokenResponseCount > 5) {
    console.error('Redirecting too many times, stopping');
    win.webContents.stop();
    return;
  }

  console.log(`Audience: ${decoded.aud}`);
  console.log('Decoded', decoded);

  win.webContents.stop();

  if (decoded.aud === TEAMS_APP_ID) {
    // Teams Token
    console.log('Got a Teams token');
    saveTeamsToken(teamsToken, 'teams');
    checkTokens();
    tokens.teams = true;
    authorize('skype', currentTenant);
  } else if (decoded.aud === SKYPE_RESOURCE) {
    console.log('Got a Skype token');
    saveTeamsToken(teamsToken, 'skype');
    authorize('chatsvcagg', currentTenant);
    tokens.skype = true;
    checkTokens();
  } else if (decoded.aud === CHAT_SVC_AGG_RESOURCE) {
    console.log('Got a ChatSvcAgg token');
    saveTeamsToken(teamsToken, 'chatsvcagg');
    tokens.chatsvcagg = true;
    checkTokens();
  } else {
    console.error(`Invalid audience ${decoded.aud} found.`);
  }
}

// Disable GPU sandbox for WSL2/wslg compatibility
app.commandLine.appendSwitch('disable-gpu-sandbox');

app.whenReady().then(() => {
  // Check process arguments
  const p = new Promise<void>((resolve, _) => {
    if (process.argv.length === 3) {
      // Check if 3rd argument is "logout"
      switch (process.argv[2]) {
        case 'logout':
          session.defaultSession.clearStorageData().then(() => {
            resolve();
          });
          break;

        case 'get-url':
          console.log(getLoginURL('teams', 'common'));
          app.exit();
          break;

        default:
          break;
      }
    }
    resolve();
  });

  p.then(() => {
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

    // Teams now server-redirects /go to /error/eoa, so the did-navigate
    // handler never sees the token URL. Grab the token in will-redirect
    // instead, before the bounce happens.
    win.webContents.on('will-redirect', async (e, url) => {
      if (url.startsWith(REDIRECT_URI)) {
        e.preventDefault();
        await handleRedirect(url);
      }
    });

    // Safety net for the case the token URL is loaded without an
    // intermediate redirect.
    win.webContents.on('did-navigate', async (e, url) => {
      if (url.startsWith(REDIRECT_URI)) {
        await handleRedirect(url);
      }
    });

    authorize('teams', 'common');
  });
});
