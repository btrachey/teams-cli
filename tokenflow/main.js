"use strict";
var __createBinding = (this && this.__createBinding) || (Object.create ? (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    Object.defineProperty(o, k2, { enumerable: true, get: function() { return m[k]; } });
}) : (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    o[k2] = m[k];
}));
var __setModuleDefault = (this && this.__setModuleDefault) || (Object.create ? (function(o, v) {
    Object.defineProperty(o, "default", { enumerable: true, value: v });
}) : function(o, v) {
    o["default"] = v;
});
var __importStar = (this && this.__importStar) || function (mod) {
    if (mod && mod.__esModule) return mod;
    var result = {};
    if (mod != null) for (var k in mod) if (k !== "default" && Object.prototype.hasOwnProperty.call(mod, k)) __createBinding(result, mod, k);
    __setModuleDefault(result, mod);
    return result;
};
var __awaiter = (this && this.__awaiter) || function (thisArg, _arguments, P, generator) {
    function adopt(value) { return value instanceof P ? value : new P(function (resolve) { resolve(value); }); }
    return new (P || (P = Promise))(function (resolve, reject) {
        function fulfilled(value) { try { step(generator.next(value)); } catch (e) { reject(e); } }
        function rejected(value) { try { step(generator["throw"](value)); } catch (e) { reject(e); } }
        function step(result) { result.done ? resolve(result.value) : adopt(result.value).then(fulfilled, rejected); }
        step((generator = generator.apply(thisArg, _arguments || [])).next());
    });
};
var __generator = (this && this.__generator) || function (thisArg, body) {
    var _ = { label: 0, sent: function() { if (t[0] & 1) throw t[1]; return t[1]; }, trys: [], ops: [] }, f, y, t, g;
    return g = { next: verb(0), "throw": verb(1), "return": verb(2) }, typeof Symbol === "function" && (g[Symbol.iterator] = function() { return this; }), g;
    function verb(n) { return function (v) { return step([n, v]); }; }
    function step(op) {
        if (f) throw new TypeError("Generator is already executing.");
        while (_) try {
            if (f = 1, y && (t = op[0] & 2 ? y["return"] : op[0] ? y["throw"] || ((t = y["return"]) && t.call(y), 0) : y.next) && !(t = t.call(y, op[1])).done) return t;
            if (y = 0, t) op = [op[0] & 2, t.value];
            switch (op[0]) {
                case 0: case 1: t = op; break;
                case 4: _.label++; return { value: op[1], done: false };
                case 5: _.label++; y = op[1]; op = [0]; continue;
                case 7: op = _.ops.pop(); _.trys.pop(); continue;
                default:
                    if (!(t = _.trys, t = t.length > 0 && t[t.length - 1]) && (op[0] === 6 || op[0] === 2)) { _ = 0; continue; }
                    if (op[0] === 3 && (!t || (op[1] > t[0] && op[1] < t[3]))) { _.label = op[1]; break; }
                    if (op[0] === 6 && _.label < t[1]) { _.label = t[1]; t = op; break; }
                    if (t && _.label < t[2]) { _.label = t[2]; _.ops.push(op); break; }
                    if (t[2]) _.ops.pop();
                    _.trys.pop(); continue;
            }
            op = body.call(thisArg, _);
        } catch (e) { op = [6, e]; y = 0; } finally { f = t = 0; }
        if (op[0] & 5) throw op[1]; return { value: op[0] ? op[1] : void 0, done: true };
    }
};
var __importDefault = (this && this.__importDefault) || function (mod) {
    return (mod && mod.__esModule) ? mod : { "default": mod };
};
exports.__esModule = true;
var electron_1 = require("electron");
var os_1 = require("os");
var fs_1 = require("fs");
var path = __importStar(require("path"));
var uuid_1 = require("uuid");
var axios_1 = __importDefault(require("axios"));
var jwt = __importStar(require("jsonwebtoken"));
var configPath = '.config/fossteams';
var DEBUG = process.env.NODE_ENV === 'debug';
var MICROSOFT_TENANT_ID = 'f8cdef31-a31e-4b4a-93e4-5f571e91255a';
var TEAMS_APP_ID = '5e3ce6c0-2b1f-4285-8d4b-75ee78787346';
var SKYPE_RESOURCE = 'https://api.spaces.skype.com';
var CHAT_SVC_AGG_RESOURCE = 'https://chatsvcagg.teams.microsoft.com';
var REDIRECT_URI = 'https://teams.microsoft.com/go';
var USER_AGENT = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) MicrosoftTeams-Preview/1.4.00.7556 Chrome/80.0.3987.163 Electron/8.5.5 Safari/537.36';
var win = null;
var tokenResponseCount = 0;
var currentTenant = null;
var tokens = {
    teams: false,
    chatsvcagg: false,
    skype: false
};
function getLoginURL(type, tenantId) {
    var loginUrl = new URL('https://login.microsoftonline.com');
    loginUrl.pathname = "/" + tenantId + "/oauth2/authorize";
    var state = uuid_1.v4();
    switch (type) {
        case 'teams':
            loginUrl.searchParams.append('response_type', 'id_token');
            loginUrl.searchParams.append('state', state);
            break;
        case 'skype':
            loginUrl.searchParams.append('response_type', 'token');
            loginUrl.searchParams.append('state', state + "|" + SKYPE_RESOURCE);
            loginUrl.searchParams.append('resource', SKYPE_RESOURCE);
            break;
        case 'chatsvcagg':
            loginUrl.searchParams.append('response_type', 'token');
            loginUrl.searchParams.append('state', state + "|" + CHAT_SVC_AGG_RESOURCE);
            loginUrl.searchParams.append('resource', CHAT_SVC_AGG_RESOURCE);
            break;
        default:
            break;
    }
    loginUrl.searchParams.append('client_id', TEAMS_APP_ID);
    loginUrl.searchParams.append('client-request-id', uuid_1.v4());
    loginUrl.searchParams.append('redirect_uri', REDIRECT_URI);
    loginUrl.searchParams.append('x-client-SKU', 'Js');
    loginUrl.searchParams.append('x-client-Ver', '1.0.9');
    loginUrl.searchParams.append('nonce', uuid_1.v4());
    return loginUrl.toString();
}
function authorize(type, tenantId) {
    console.log("Authorizing " + type + " with tenantId=" + tenantId);
    win.loadURL(getLoginURL(type, tenantId), {
        userAgent: USER_AGENT
    });
}
function saveTeamsToken(token, type) {
    // teams-cli sets FOSSTEAMS_TOKEN_DIR to the directory it actually reads
    // (e.g. --token-dir); without it, fall back to the historical default.
    var tokenDir = process.env.FOSSTEAMS_TOKEN_DIR
        ? path.resolve(process.env.FOSSTEAMS_TOKEN_DIR)
        : path.join(os_1.homedir(), configPath);
    if (!fs_1.existsSync(tokenDir)) {
        fs_1.mkdirSync(tokenDir, { recursive: true });
    }
    fs_1.writeFileSync(tokenDir + "/token-" + type + ".jwt", token);
}
function getTenants(token) {
    var req = axios_1["default"].get('https://teams.microsoft.com/api/mt/emea/beta/users/tenants', {
        headers: {
            Authorization: "Bearer " + token
        }
    });
    return req;
}
// Verifies that we've got all tokens
function checkTokens() {
    if (tokens.chatsvcagg && tokens.skype && tokens.teams) {
        win.destroy();
        electron_1.app.quit();
    }
}
// Teams now server-redirects /go to /error/eoa, so the did-navigate handler
// never sees the token URL anymore. Catch the token in will-redirect, before
// the bounce happens, and stop the navigation ourselves.
function handleRedirect(url) {
    return __awaiter(this, void 0, void 0, function () {
        var token, searchParams, teamsToken, decoded, err, errDesc, tenants, err_1;
        return __generator(this, function (_a) {
            switch (_a.label) {
                case 0:
                    token = url.replace(REDIRECT_URI + "#", '');
                    searchParams = new URLSearchParams(token);
                    teamsToken = searchParams.get('id_token');
                    if (teamsToken === null) {
                        teamsToken = searchParams.get('access_token');
                    }
                    decoded = jwt.decode(teamsToken);
                    if (decoded === null) {
                        console.warn("Invalid JWT provided: " + searchParams);
                        if (searchParams.has('error')) {
                            err = searchParams.get('error');
                            errDesc = searchParams.get('error_description');
                            console.error("Got error = " + err + ": " + errDesc);
                            switch (err) {
                                case 'interaction_required':
                                    // User has to interact!
                                    console.error('User interaction is required (e.g: MFA). Are you using "?prompt=none" ?');
                                    break;
                                default:
                                // Can't handle this, sorry
                            }
                        }
                        return [2 /*return*/];
                    }
                    if (typeof (decoded) === 'string') {
                        console.error('Invalid decoded JWT: is a string');
                        return [2 /*return*/];
                    }
                    if (!(decoded.tid === MICROSOFT_TENANT_ID && decoded.aud === SKYPE_RESOURCE)) return [3 /*break*/, 6];
                    // We need a tenant selection
                    console.log("Tenant ID is MICROSOFT: aud=" + decoded.aud);
                    _a.label = 1;
                case 1:
                    _a.trys.push([1, 4, , 5]);
                    return [4 /*yield*/, getTenants(teamsToken)];
                case 2: return [4 /*yield*/, (_a.sent()).data];
                case 3:
                    tenants = _a.sent();
                    // Pick the first tenant and authorize Skype
                    currentTenant = tenants[0].tenantId;
                    authorize('skype', currentTenant);
                    return [3 /*break*/, 5];
                case 4:
                    err_1 = _a.sent();
                    console.error("Unable to get tenants: " + err_1);
                    return [3 /*break*/, 5];
                case 5: return [2 /*return*/];
                case 6:
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
                        return [2 /*return*/];
                    }
                    console.log("Audience: " + decoded.aud);
                    console.log('Decoded', decoded);
                    win.webContents.stop();
                    if (decoded.aud === TEAMS_APP_ID) {
                        // Teams Token
                        console.log('Got a Teams token');
                        saveTeamsToken(teamsToken, 'teams');
                        checkTokens();
                        tokens.teams = true;
                        authorize('skype', currentTenant);
                    }
                    else if (decoded.aud === SKYPE_RESOURCE) {
                        console.log('Got a Skype token');
                        saveTeamsToken(teamsToken, 'skype');
                        authorize('chatsvcagg', currentTenant);
                        tokens.skype = true;
                        checkTokens();
                    }
                    else if (decoded.aud === CHAT_SVC_AGG_RESOURCE) {
                        console.log('Got a ChatSvcAgg token');
                        saveTeamsToken(teamsToken, 'chatsvcagg');
                        tokens.chatsvcagg = true;
                        checkTokens();
                    }
                    else {
                        console.error("Invalid audience " + decoded.aud + " found.");
                    }
                    return [2 /*return*/];
            }
        });
    });
}
// Disable GPU sandbox for WSL2/wslg compatibility
electron_1.app.commandLine.appendSwitch('disable-gpu-sandbox');
electron_1.app.whenReady().then(function () {
    // Check process arguments
    var p = new Promise(function (resolve, _) {
        if (process.argv.length === 3) {
            // Check if 3rd argument is "logout"
            switch (process.argv[2]) {
                case 'logout':
                    electron_1.session.defaultSession.clearStorageData().then(function () {
                        resolve();
                    });
                    break;
                case 'get-url':
                    console.log(getLoginURL('teams', 'common'));
                    electron_1.app.exit();
                    break;
                default:
                    break;
            }
        }
        resolve();
    });
    p.then(function () {
        win = new electron_1.BrowserWindow({ width: 800, height: 600 });
        if (DEBUG) {
            win.webContents.openDevTools();
        }
        win.webContents.on('will-navigate', function (e, url) {
            if (url.startsWith('https://teams.microsoft.com/')) {
                e.preventDefault();
            }
        });
        // Teams now server-redirects /go to /error/eoa, so the did-navigate
        // handler never sees the token URL. Grab the token in will-redirect
        // instead, before the bounce happens.
        win.webContents.on('will-redirect', function (e, url) { return __awaiter(void 0, void 0, void 0, function () {
            return __generator(this, function (_a) {
                switch (_a.label) {
                    case 0:
                        if (!url.startsWith(REDIRECT_URI)) return [3 /*break*/, 2];
                        e.preventDefault();
                        return [4 /*yield*/, handleRedirect(url)];
                    case 1:
                        _a.sent();
                        _a.label = 2;
                    case 2: return [2 /*return*/];
                }
            });
        }); });
        // Safety net for the case the token URL is loaded without an
        // intermediate redirect.
        win.webContents.on('did-navigate', function (e, url) { return __awaiter(void 0, void 0, void 0, function () {
            return __generator(this, function (_a) {
                switch (_a.label) {
                    case 0:
                        if (!url.startsWith(REDIRECT_URI)) return [3 /*break*/, 2];
                        return [4 /*yield*/, handleRedirect(url)];
                    case 1:
                        _a.sent();
                        _a.label = 2;
                    case 2: return [2 /*return*/];
                }
            });
        }); });
        authorize('teams', 'common');
    });
});
//# sourceMappingURL=main.js.map