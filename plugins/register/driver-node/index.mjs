#!/usr/bin/env node
// higgsgo-register-driver: an HTTP server that wraps the higgsfield-register
// Node project's registerAccount() call. Started as a subprocess by the Go
// side (plugins/register/adapters/camoufox/driver_node.go). Follows the same
// shape as klinggo/browser/camoufox_driver.py — one endpoint per semantic
// operation, not per DOM click.
//
// Usage:
//   node index.mjs --port 8801 [--headless]
//
// Endpoints:
//   GET  /ready      → { ready: true, name: "higgsgo-register-driver" }
//   POST /register   → runs one signup end-to-end; body:
//                        { email, password, oauth_source?, proxy_url?,
//                          mailbox_config?: { client_id, refresh_token } }
//                      → { account_id, session_id, user_id, cookies[], ...}
//                        or { error: "..." }
//   POST /shutdown   → clean exit
//
// The registerAccount() implementation is imported from
// ../../../../higgsfield-register/src/register/flow.mjs when that project is
// available on the filesystem — production deployments symlink or copy the
// higgsfield-register sources next to this driver. When flow.mjs cannot be
// resolved, /register returns 503 driver_unavailable so operators see a
// distinct failure mode from "flow ran and failed".
//
// Deliberately minimal — no auth, binds only to 127.0.0.1. Go spawns and
// owns the lifetime; nothing outside the box should reach this port.

import http from 'node:http';
import { parse as parseUrl } from 'node:url';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

// -----------------------------------------------------------------------------
// CLI args
// -----------------------------------------------------------------------------
function parseArgs(argv) {
  const out = { port: 8801, headless: true };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--port') out.port = parseInt(argv[++i], 10);
    else if (a === '--headless') out.headless = true;
    else if (a === '--headed') out.headless = false;
  }
  return out;
}

const args = parseArgs(process.argv.slice(2));

// -----------------------------------------------------------------------------
// Lazy load of the real flow.
//
// The higgsfield-register project is expected to sit as a sibling of the
// higgsgo checkout (../../../../higgsfield-register/) or be pointed at
// via HIGGSFIELD_REGISTER_ROOT. When resolvable, we import registerAccount()
// and use it verbatim. When not resolvable, /register answers 503 so the
// Go side can distinguish "no driver" from "driver failed".
// -----------------------------------------------------------------------------
async function resolveRegisterAccount() {
  const roots = [];
  if (process.env.HIGGSFIELD_REGISTER_ROOT) {
    roots.push(process.env.HIGGSFIELD_REGISTER_ROOT);
  }
  // Sibling repo layout: higgsgo/plugins/register/driver-node/ →
  // ../../../../higgsfield-register/
  roots.push(path.resolve(__dirname, '..', '..', '..', '..', 'higgsfield-register'));

  for (const root of roots) {
    const flowPath = path.join(root, 'src', 'register', 'flow.mjs');
    if (fs.existsSync(flowPath)) {
      try {
        const mod = await import(flowPath);
        if (typeof mod.registerAccount === 'function') {
          return { registerAccount: mod.registerAccount, root };
        }
      } catch (err) {
        console.error(`[driver] failed to import ${flowPath}:`, err.message);
      }
    }
  }
  return null;
}

const bridge = await resolveRegisterAccount();
if (bridge) {
  console.error(`[driver] flow.mjs resolved at ${bridge.root}`);
} else {
  console.error(`[driver] flow.mjs NOT resolvable — /register will 503`);
}

// -----------------------------------------------------------------------------
// HTTP handlers
// -----------------------------------------------------------------------------
function readBody(req) {
  return new Promise((resolve, reject) => {
    let buf = '';
    req.setEncoding('utf8');
    req.on('data', (chunk) => {
      buf += chunk;
      if (buf.length > 1 << 20) {
        req.destroy();
        reject(new Error('body too large'));
      }
    });
    req.on('end', () => {
      if (!buf) return resolve({});
      try {
        resolve(JSON.parse(buf));
      } catch (err) {
        reject(err);
      }
    });
    req.on('error', reject);
  });
}

function respond(res, status, obj) {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(obj));
}

async function handleRegister(req, res) {
  if (!bridge) {
    return respond(res, 503, {
      error: 'driver_unavailable',
      detail: 'higgsfield-register/src/register/flow.mjs not found. Set HIGGSFIELD_REGISTER_ROOT or place the sibling repo alongside higgsgo.',
    });
  }
  let body;
  try {
    body = await readBody(req);
  } catch (err) {
    return respond(res, 400, { error: 'bad_body', detail: String(err) });
  }
  const { email, password, proxy_url: proxyUrl, mailbox_config } = body;
  if (!email || !password) {
    return respond(res, 400, { error: 'invalid_request', detail: 'email + password required' });
  }

  // fetchOtp callback runs the higgsfield-register Graph OTP module
  // against this row's mailbox credentials. Refuses to attempt when
  // mailbox_config is missing — the caller is Go-side, which already
  // requires the fields for the password flow, so this branch mostly
  // exists as a safety net for hand-crafted requests.
  //
  // higgsfield-register's registerAccount() invokes fetchOtp with the
  // sign-up submit timestamp (as a Date). We forward it as-is to
  // waitForOtp so the Graph query only returns emails received after
  // the sign-up. Returns a string per registerAccount's contract —
  // waitForOtp yields { code, message }, we return just the code.
  const fetchOtp = async (notBefore) => {
    if (!mailbox_config) {
      throw new Error('mailbox_config required for OTP retrieval');
    }
    const graphOtpPath = path.join(bridge.root, 'src', 'mail', 'graph-otp.mjs');
    if (!fs.existsSync(graphOtpPath)) {
      throw new Error(`graph-otp.mjs not found at ${graphOtpPath}`);
    }
    const graph = await import(graphOtpPath);
    const token = await graph.refreshAccessToken({
      clientId: mailbox_config.client_id,
      refreshToken: mailbox_config.refresh_token,
      proxyUrl,
    });
    const result = await graph.waitForOtp({
      accessToken: token.accessToken,
      notBefore,
      proxyUrl,
    });
    // registerAccount expects a string OTP code; unwrap.
    return result.code;
  };

  const logs = [];
  const log = (m) => {
    logs.push(m);
    console.error(`[driver] ${m}`);
  };

  try {
    const harvest = await bridge.registerAccount({
      account: { email, password },
      proxyUrl,
      headed: !args.headless,
      fetchOtp,
      log,
    });
    // Normalize the sibling repo's harvest shape into the field
    // names the Go NodeDriver.mapDriverResult expects. The Go side
    // was designed against the plugin's CompletedResult (snake_case,
    // cookies as array); higgsfield-register/harvest emits camelCase
    // with cookies as a map. Doing the shape mapping here — where
    // both shapes are one file away — keeps Go free of harvest-
    // specific knowledge.
    const cookiesArr = Object.entries(harvest.cookies || {}).map(([name, value]) => ({
      name,
      value,
      domain: name.startsWith('__client') || name.startsWith('__session')
        ? '.higgsfield.ai'
        : 'higgsfield.ai',
      path: '/',
      secure: true,
      httpOnly: name.startsWith('__') || name === 'datadome',
    }));
    // Best-effort credit extraction. walletSnapshot may not exist
    // (harvest gets it opportunistically); zero is fine — the pool's
    // balance refresher will fill it in on the next tick anyway.
    let credits = 0;
    if (harvest.walletSnapshot?.subscription_balance != null) {
      credits = Number(harvest.walletSnapshot.subscription_balance);
    } else if (harvest.walletSnapshot?.credits_balance != null) {
      credits = Number(harvest.walletSnapshot.credits_balance);
    }
    const result = {
      // higgsfield's clerk user_id doubles as the account_id in the
      // higgsgo Account row (see internal/domain/account.go — the
      // ID field is documented as "clerk user_id").
      account_id: harvest.userId || '',
      user_id: harvest.userId || '',
      session_id: harvest.sessionId || '',
      user_agent: harvest.capturedUserAgent || '',
      datadome_id: harvest.xDatadomeClientId || '',
      plan_type: harvest.planType || '',
      credits,
      cookies: cookiesArr,
    };
    return respond(res, 200, { ok: true, result, logs });
  } catch (err) {
    return respond(res, 200, { ok: false, error: String(err?.message || err), logs });
  }
}

const server = http.createServer(async (req, res) => {
  const parsed = parseUrl(req.url || '', true);
  const url = parsed.pathname;

  if (req.method === 'GET' && url === '/ready') {
    return respond(res, 200, {
      ready: true,
      name: 'higgsgo-register-driver',
      driver_available: !!bridge,
    });
  }

  if (req.method === 'POST' && url === '/register') {
    return handleRegister(req, res);
  }

  if (req.method === 'POST' && url === '/shutdown') {
    respond(res, 200, { ok: true });
    setTimeout(() => process.exit(0), 100);
    return;
  }

  respond(res, 404, { error: 'not_found', path: url });
});

server.listen(args.port, '127.0.0.1', () => {
  console.error(`[driver] listening on 127.0.0.1:${args.port}`);
});

// Clean shutdown on SIGTERM/SIGINT so Go's cmd.Process.Kill leaves no
// stray browser subprocess.
for (const sig of ['SIGTERM', 'SIGINT']) {
  process.on(sig, () => {
    console.error(`[driver] ${sig} received, exiting`);
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(1), 3000).unref();
  });
}
