// Adobe 账号自动登录与 Cookie（含风险会话 sherlockToken）可视化提取工具
// 双击「启动Cookie提取工具.bat」自动打开 GUI 界面（http://127.0.0.1:18899）

const crypto = require('crypto');
const http = require('http');
const fs = require('fs');
const path = require('path');
const { spawn, execSync } = require('child_process');

const PORT = Number(process.env.COOKIE_TOOL_PORT || 18899);
const TMP_ROOT = 'D:\\tmp';

// 查找本机已安装的 Chrome 或 Edge 浏览器路径
function findBrowserPath() {
  const candidates = [
    process.env.CHROME_PATH,
    'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
    path.join(process.env.LOCALAPPDATA || '', 'Google\\Chrome\\Application\\chrome.exe'),
    'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
    'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe'
  ].filter(Boolean);
  for (const p of candidates) {
    if (fs.existsSync(p)) return p;
  }
  throw new Error('未找到 Chrome 或 Edge 浏览器，请确认已安装 Chrome');
}

// 解析组合卡密行（支持 ----、-—、—— 等分隔符）
function parseAccountLine(raw) {
  const line = String(raw || '').trim();
  if (!line) return null;
  const parts = line.split(/(?:-+|—+|\u2014+){2,}|-\u2014|\u2014-/).map(s => s.trim()).filter(Boolean);
  if (parts.length >= 4) {
    const email = parts[0];
    const emailPassword = parts[1] || '';
    const mailClientId = parts[2] || '';
    const mailRefreshToken = parts[3] || '';
    let note = parts[4] || '';
    let adobePassword = parts.length >= 6 ? parts[parts.length - 1] : '';
    // 如果倒数第二项本身是 refresh_token 的尾部（如 Pzh3LO3Q$$），则最后一项是备注而非密码
    if (parts.length === 6 && mailRefreshToken.endsWith(parts[4])) {
      note = parts[5];
      adobePassword = '';
    }
    return { email, emailPassword, mailClientId, mailRefreshToken, note, adobePassword };
  }
  if (parts.length === 2) {
    return { email: parts[0], adobePassword: parts[1] };
  }
  return null;
}

// 通过微软 Graph API 读取最新一封邮件的 ID 与 6 位 Adobe 验证码
async function fetchLatestOutlookCode(clientId, refreshToken) {
  if (!clientId || !refreshToken) return { id: '', code: null };
  try {
    const tokenRes = await fetch('https://login.microsoftonline.com/common/oauth2/v2.0/token', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({
        client_id: clientId,
        grant_type: 'refresh_token',
        refresh_token: refreshToken,
        scope: 'https://graph.microsoft.com/.default'
      })
    });
    const tokenData = await tokenRes.json();
    if (!tokenData.access_token) return { id: '', code: null };

    const mailRes = await fetch(
      'https://graph.microsoft.com/v1.0/me/messages?$top=3&$select=id,subject,receivedDateTime,bodyPreview,body',
      { headers: { Authorization: `Bearer ${tokenData.access_token}` } }
    );
    const mailData = await mailRes.json();
    const messages = Array.isArray(mailData.value) ? mailData.value : [];
    for (const msg of messages) {
      const rawHtml = (msg.body && msg.body.content) || '';
      const plain = rawHtml
        .replace(/<style[\s\S]*?<\/style>/gi, ' ')
        .replace(/<[^>]+>/g, ' ')
        .replace(/&nbsp;|&#\d+;|[\u200B-\u200D\uFEFF]/g, ' ')
        .replace(/\s+/g, ' ');
      const text = `${msg.subject || ''} ${msg.bodyPreview || ''} ${plain}`;
      if (!/adobe|authentication code|verification code|验证码/i.test(text)) continue;
      const match = plain.match(/\b(\d{6})\b/) || text.match(/\b(\d{6})\b/);
      if (match) return { id: msg.id, code: match[1] };
    }
  } catch {}
  return { id: '', code: null };
}

let currentStatus = '就绪';

// 执行单次账号自动登录与 Cookie 提取
async function extractAdobeCookie(params) {
  const log = msg => {
    currentStatus = msg;
    console.log('[提取进度]', msg);
  };
  const parsed = parseAccountLine(params.accountLine) || {};
  const email = (params.email || parsed.email || '').trim();
  const adobePassword = (params.adobePassword || parsed.adobePassword || parsed.emailPassword || '').trim();
  const mailClientId = (params.mailClientId || parsed.mailClientId || '').trim();
  const mailRefreshToken = (params.mailRefreshToken || parsed.mailRefreshToken || '').trim();
  const note = (params.note || parsed.note || '').trim();

  if (!email) {
    throw new Error('请填写 Adobe 邮箱账号（或粘贴完整卡密行）');
  }
  if (!adobePassword && (!mailClientId || !mailRefreshToken)) {
    throw new Error('请填写 Adobe 登录密码，或提供微软邮箱 Client ID + Refresh Token 以使用邮箱验证码登录');
  }

  fs.mkdirSync(TMP_ROOT, { recursive: true });
  const profileDir = path.join(TMP_ROOT, `adobe-cookie-tool-${Date.now()}`);
  const debugPort = 19300 + Math.floor(Math.random() * 400);
  const browserPath = findBrowserPath();

  log('正在读取邮箱初始状态并启动独立浏览器...');
  const beforeMail = await fetchLatestOutlookCode(mailClientId, mailRefreshToken);

  const loginUrl =
    'https://ims-na1.adobelogin.com/ims/authorize/v2?client_id=clio-playground-web&scope=AdobeID%2Cfirefly_api%2Copenid%2Cpps.read%2Cpps.write%2Cadditional_info.projectedProductContext%2Cadditional_info.ownerOrg%2Cuds_read%2Cuds_write%2Cab.manage%2Cread_organizations%2Cadditional_info.roles%2Caccount_cluster.read%2Ccreative_production%2Ctk_platform%2Ctk_platform_sync%2Caf_byof%2Cprofile%2Cdii_lr_ml_unlimited&redirect_uri=https%3A%2F%2Ffirefly.adobe.com%2Fgenerate%2Fimage&response_type=token&state=%7B%22ac%22%3A%22firefly.adobe.com%22%7D&locale=en_US';

  const child = spawn(
    browserPath,
    [
      `--remote-debugging-port=${debugPort}`,
      `--user-data-dir=${profileDir}`,
      '--no-first-run',
      '--no-default-browser-check',
      loginUrl
    ],
    { detached: false, stdio: 'ignore' }
  );

  const cleanup = () => {
    try {
      if (child.pid) {
        execSync(`taskkill /PID ${child.pid} /T /F`, { stdio: 'ignore' });
      }
    } catch {}
    setTimeout(() => {
      try {
        fs.rmSync(profileDir, { recursive: true, force: true });
      } catch {}
    }, 1500);
  };

  try {
    let pageTarget = null;
    for (let i = 0; i < 30; i++) {
      await new Promise(r => setTimeout(r, 500));
      try {
        const res = await fetch(`http://127.0.0.1:${debugPort}/json/list`);
        const list = await res.json();
        pageTarget = list.find(t => t.type === 'page');
        if (pageTarget) break;
      } catch {}
    }
    if (!pageTarget) throw new Error('浏览器调试端口启动超时');

    const ws = new WebSocket(pageTarget.webSocketDebuggerUrl);
    await new Promise((resolve, reject) => {
      ws.addEventListener('open', resolve);
      ws.addEventListener('error', reject);
    });

    let seq = 1;
    const send = (method, cmdParams = {}) =>
      new Promise((resolve, reject) => {
        const id = seq++;
        const timer = setTimeout(() => {
          ws.removeEventListener('message', handler);
          reject(new Error(`CDP 命令超时: ${method}`));
        }, 30000);
        const handler = ev => {
          const msg = JSON.parse(ev.data);
          if (msg.id === id) {
            clearTimeout(timer);
            ws.removeEventListener('message', handler);
            if (msg.error) reject(new Error(msg.error.message));
            else resolve(msg.result);
          }
        };
        ws.addEventListener('message', handler);
        ws.send(JSON.stringify({ id, method, params: cmdParams }));
      });

    log(`正在打开 Adobe 登录框并输入邮箱 ${email}...`);
    for (let i = 0; i < 30; i++) {
      await new Promise(r => setTimeout(r, 1000));
      const check = await send('Runtime.evaluate', {
        expression: `Boolean(document.querySelector('#EmailPage-EmailField'))`,
        returnByValue: true
      });
      if (check.result?.value) break;
    }

    // 如果提供了微软邮箱令牌且未填密码（或优先使用一次性验证码），自动点一次性验证码登录
    let submittedAuth = false;
    let capturedToken = '';
    for (let step = 0; step < 45; step++) {
      await new Promise(r => setTimeout(r, 1500));
      const stateRes = await send('Runtime.evaluate', {
        expression: `JSON.stringify({
          url: location.href,
          text: document.body ? document.body.innerText.slice(0, 300) : '',
          hasEmail: Boolean(document.querySelector('#EmailPage-EmailField')),
          hasPwd: Boolean(document.querySelector('#PasswordPage-PasswordField')),
          hasUsePwdBtn: Array.from(document.querySelectorAll('button, a')).some(b => /sign in with your password|continue with password|use password|密码/i.test(b.innerText || b.textContent || '')),
          hasOtpBtn: Array.from(document.querySelectorAll('button, a')).some(b => /one-time code|sign in with a code|send.*verification code|验证码/i.test(b.innerText || b.textContent || '')) || (/send you a verification code|验证码发送到/i.test(document.body ? document.body.innerText : '') && !document.querySelectorAll('[data-id^=CodeInput-], input[inputmode=numeric]').length),
          hasCodeBoxes: document.querySelectorAll('[data-id^=CodeInput-], input[inputmode=numeric]').length > 0,
          hasSkipPhone: location.hash.includes('add-security-phone-number') || Boolean(document.querySelector('[data-id=PP-AddSecurityPhoneNumber-skip-btn]')) || /mobile phone number|Not now/i.test(document.body ? document.body.innerText : ''),
          isAccountHome: location.hostname === 'firefly.adobe.com'
        })`,
        returnByValue: true
      });
      const st = JSON.parse(stateRes.result.value);
      if (st.url && st.url.includes('access_token=')) {
        const m = st.url.match(/access_token=([^&]+)/);
        if (m) capturedToken = decodeURIComponent(m[1]);
      }
      if (step < 5) console.log('Step', step, st.url.slice(0, 70), 'hasEmail:', st.hasEmail, 'hasPwd:', st.hasPwd, 'hasOtpBtn:', st.hasOtpBtn);

      if (submittedAuth && st.isAccountHome) {
        break;
      }

      if (st.hasEmail) {
        log(`正在填入邮箱 ${email} 并点击继续...`);
        await send('Runtime.evaluate', {
          expression: `(() => {
            const el = document.querySelector('#EmailPage-EmailField');
            if (!el) return;
            el.focus();
            const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
            setter.call(el, ${JSON.stringify(email)});
            el.dispatchEvent(new Event('input', { bubbles: true }));
            el.dispatchEvent(new Event('change', { bubbles: true }));
            setTimeout(() => {
              document.querySelector('[data-id=EmailPage-ContinueButton], button[type=submit]')?.click();
            }, 300);
          })()`
        });
        await new Promise(r => setTimeout(r, 4500));
        continue;
      }

      if (st.hasSkipPhone) {
        log('正在自动跳过绑定手机号提示...');
        await send('Runtime.evaluate', {
          expression: `(() => {
            const btn = document.querySelector('[data-id=PP-AddSecurityPhoneNumber-skip-btn]') ||
              Array.from(document.querySelectorAll('button, a')).find(b => /not now|skip|以后|跳过/i.test(b.innerText || b.textContent || ''));
            if (btn) btn.click();
          })()`
        });
        await new Promise(r => setTimeout(r, 4000));
        continue;
      }

      if (!st.hasPwd && st.hasUsePwdBtn && adobePassword) {
        log('正在切换至密码登录...');
        await send('Runtime.evaluate', {
          expression: `(() => {
            const btn = Array.from(document.querySelectorAll('button, a')).find(b => /sign in with your password|continue with password|use password|密码/i.test(b.innerText || b.textContent || ''));
            if (btn) btn.click();
          })()`
        });
        await new Promise(r => setTimeout(r, 2500));
        continue;
      }

      if (st.hasPwd && adobePassword) {
        log('正在输入 Adobe 密码并勾选保持登录...');
        await send('Runtime.evaluate', {
          expression: `(() => {
            const el = document.querySelector('#PasswordPage-PasswordField');
            if (el) {
              el.focus();
              const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
              setter.call(el, ${JSON.stringify(adobePassword)});
              el.dispatchEvent(new Event('input', { bubbles: true }));
              el.dispatchEvent(new Event('change', { bubbles: true }));
            }
            const remember = document.querySelector('input[type=checkbox]');
            if (remember && !remember.checked) remember.click();
            setTimeout(() => {
              document.querySelector('[data-id=PasswordPage-ContinueButton], button[type=submit]')?.click();
            }, 300);
          })()`
        });
        submittedAuth = true;
        await new Promise(r => setTimeout(r, 5000));
        continue;
      }

      if (st.hasOtpBtn && !st.hasCodeBoxes && mailClientId && mailRefreshToken) {
        log('正在点击发送邮箱验证码并从 Outlook 自动收取验证码...');
        await send('Runtime.evaluate', {
          expression: `(() => {
            const remember = document.querySelector('input[type=checkbox]');
            if (remember && !remember.checked) remember.click();
            const otpBtn = Array.from(document.querySelectorAll('button, a')).find(b => /one-time code|sign in with a code|continue|继续|验证码/i.test((b.innerText || b.textContent || '').trim()));
            if (otpBtn) otpBtn.click();
          })()`
        });
        await new Promise(r => setTimeout(r, 3000));
        continue;
      }

      if (st.hasCodeBoxes && mailClientId && mailRefreshToken) {
        log('正在轮询 Outlook 收件箱获取最新 6 位验证码...');
        let code = null;
        for (let r = 0; r < 18; r++) {
          await new Promise(res => setTimeout(res, 2500));
          const cur = await fetchLatestOutlookCode(mailClientId, mailRefreshToken);
          if (cur.id && cur.id !== beforeMail.id && cur.code) {
            code = cur.code;
            break;
          }
        }
        if (!code) throw new Error('等待 Outlook 验证码邮件超时');
        log(`已自动收到验证码 ${code}，正在填入...`);
        await send('Page.bringToFront');
        for (let d = 0; d < 6; d++) {
          await send('Runtime.evaluate', {
            expression: `(() => {
              const boxes = document.querySelectorAll('[data-id^=CodeInput-], input[inputmode=numeric]');
              const box = boxes[${d}] || boxes[0];
              if (box) box.focus();
            })()`
          });
          await send('Input.insertText', { text: code[d] });
          await new Promise(res => setTimeout(res, 150));
        }
        await new Promise(res => setTimeout(res, 1500));
        await send('Runtime.evaluate', {
          expression: `(() => {
            if (document.querySelectorAll('[data-id^=CodeInput-], input[inputmode=numeric]').length > 0) {
              document.querySelector('button[type=submit], [data-id*=Continue]')?.click();
            }
          })()`
        });
        submittedAuth = true;
        await new Promise(res => setTimeout(res, 3500));
      }
    }

    log('登录成功！正在进入 Firefly 生图页采集风险会话（约 25 秒）...');
    const hostCheck = await send('Runtime.evaluate', {
      expression: `location.hostname`,
      returnByValue: true
    });
    if (hostCheck.result?.value !== 'firefly.adobe.com') {
      await send('Page.navigate', { url: 'https://firefly.adobe.com/generate/image' });
    }
    // 清理登录页默认写入的隐私拦截 Cookie，确保 Firefly 正常加载 Forter 与 Sherlock 风险组件
    await send('Network.deleteCookies', { name: 'OptanonConsent', domain: '.adobe.com' });
    await send('Network.deleteCookies', { name: 'OptanonAlertBoxClosed', domain: '.adobe.com' });
    await send('Network.deleteCookies', { name: 'kndctr_9E1005A551ED61CA0A490D45_AdobeOrg_consent', domain: '.adobe.com' });
    if (capturedToken) {
      await send('Page.navigate', {
        url: `https://firefly.adobe.com/generate/image#access_token=${encodeURIComponent(capturedToken)}&token_type=bearer&expires_in=86400000`
      });
    } else {
      await send('Page.reload');
    }

    let session = null;
    for (let i = 0; i < 30; i++) {
      await new Promise(r => setTimeout(r, 2000));
      const check = await send('Runtime.evaluate', {
        expression: `(async () => {
          const acceptBtn = document.querySelector('#onetrust-accept-btn-handler');
          if (acceptBtn) acceptBtn.click();
          if (window.OneTrust && typeof window.OneTrust.AllowAll === 'function') {
            try { window.OneTrust.AllowAll(); } catch {}
          }
          const imsTok = window.adobeIMS && window.adobeIMS.getAccessToken && window.adobeIMS.getAccessToken();
          let fiberTok = '', fiberArp = '';
          const el = document.querySelector('firefly-image-generation');
          if (el) {
            const key = Object.keys(el).find(k => k.startsWith('__reactFiber'));
            let fiber = el[key];
            for (let j = 0; fiber && j < 15; j++, fiber = fiber.return) {
              const auth = fiber.memoizedProps?.service?.host?.authentication;
              if (auth) {
                const read = s => new Promise(resolve => {
                  let d, done = false;
                  const t = setTimeout(() => { if (!done) { done = true; resolve(''); } }, 1200);
                  d = s(v => { if (!done) { done = true; clearTimeout(t); if (typeof d === 'function') d(); resolve(v || ''); } });
                });
                [fiberTok, fiberArp] = await Promise.all([read(auth.accessToken), read(auth.arpSessionToken)]);
                break;
              }
            }
          }
          return JSON.stringify({
            url: location.href,
            hasEl: Boolean(el),
            token: (imsTok && imsTok.token) || fiberTok || '',
            arp: fiberArp || ''
          });
        })()`,
        awaitPromise: true,
        returnByValue: true
      });
      const st = check.result?.value ? JSON.parse(check.result.value) : {};
      if (st.url && st.url.includes('denied.html')) {
        throw new Error('当前网络节点不受 Adobe Firefly 支持（denied.html），请开启美国/支持地区 VPN 后重试');
      }
      if (st.token) capturedToken = st.token;
      if (st.url && st.url.includes('access_token=') && !capturedToken) {
        const m = st.url.match(/access_token=([^&]+)/);
        if (m) capturedToken = decodeURIComponent(m[1]);
      }
      const cookiesRes = await send('Network.getAllCookies');
      const sherlock = st.arp || (cookiesRes.cookies || []).find(c => c.name === 'sherlockToken')?.value;
      if (sherlock && capturedToken) {
        try {
          const decoded = JSON.parse(Buffer.from(sherlock, 'base64').toString('utf8'));
          const jwtPayload = JSON.parse(Buffer.from(capturedToken.split('.')[1], 'base64url').toString('utf8'));
          const userId = jwtPayload.user_id || jwtPayload.sub || '';
          session = { token: capturedToken, arp: sherlock, profile: { userId, email } };
          if (decoded && decoded.ftr && !decoded.ftr.includes('___') && !decoded.ftr.endsWith('__tt')) {
            break;
          }
        } catch {}
      }
    }

    if (!session || !session.token || !session.arp) {
      throw new Error('未能获取到完整登录令牌或风险会话');
    }

    const cookiesRes = await send('Network.getAllCookies');
    ws.close();

    const filteredCookies = cookiesRes.cookies.filter(
      c => c.domain.includes('adobe') && c.name !== 'edge_geo_country' && c.name !== 'sherlockToken'
    );
    const cookiePairs = filteredCookies.map(c => `${c.name}=${c.value}`);
    cookiePairs.push(`sherlockToken=${session.arp}`);
    const fullCookie = `${email}----${cookiePairs.join('; ')}`;

    let creditsInfo = null;
    try {
      const credRes = await fetch('https://firefly.adobe.io/v1/credits/balance', {
        headers: {
          Accept: 'application/json',
          Authorization: `Bearer ${session.token}`,
          'X-Api-Key': 'clio-playground-web',
          Origin: 'https://firefly.adobe.com',
          Referer: 'https://firefly.adobe.com/'
        }
      });
      creditsInfo = await credRes.json();
    } catch {}

    log('提取完成！本地临时浏览器已关闭并清理。');
    return {
      email,
      note: note || email,
      userId: session.profile?.userId || '',
      token: session.token,
      arpSession: session.arp,
      cookie: fullCookie,
      credits: creditsInfo?.total?.quota || null,
      planCap: creditsInfo?.total?.planCap || ''
    };
  } finally {
    cleanup();
  }
}

const HTML_PAGE = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <title>Adobe Cookie 自动提取工具 (GUI)</title>
  <style>
    :root { --brand: #5758d6; --bg: #f7f8fa; --ink: #242733; --line: #e4e6ed; }
    * { box-sizing: border-box; }
    body { margin: 0; padding: 36px 20px; font-family: "Microsoft YaHei", Inter, sans-serif; background: var(--bg); color: var(--ink); }
    .wrap { max-width: 760px; margin: 0 auto; background: #fff; border: 1px solid var(--line); border-radius: 16px; padding: 28px 32px; box-shadow: 0 8px 30px rgba(37,42,65,.05); }
    h1 { margin: 0 0 6px; font-size: 22px; }
    .sub { margin: 0 0 22px; font-size: 13px; color: #7c808f; line-height: 1.6; }
    label { display: block; font-size: 13px; font-weight: 600; margin-top: 16px; color: #4c5160; }
    input, textarea { width: 100%; margin-top: 7px; padding: 10px 12px; border: 1px solid var(--line); border-radius: 8px; font: inherit; font-size: 13px; }
    textarea { resize: vertical; font-family: Consolas, monospace; font-size: 12px; line-height: 1.5; }
    .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
    .btn { margin-top: 22px; display: inline-flex; align-items: center; justify-content: center; gap: 8px; background: var(--brand); color: #fff; border: 0; border-radius: 9px; padding: 11px 22px; font-size: 14px; font-weight: 600; cursor: pointer; width: 100%; }
    .btn:disabled { opacity: .6; cursor: not-allowed; }
    .btn-copy { margin-top: 10px; background: #369877; }
    .status { margin-top: 16px; padding: 11px 14px; border-radius: 8px; background: #f0f0fd; color: var(--brand); font-size: 13px; display: none; }
    .status.error { background: #fff0f1; color: #d35761; }
    .result { margin-top: 22px; padding-top: 20px; border-top: 1px solid var(--line); display: none; }
    .meta { font-size: 13px; margin-bottom: 10px; color: #369877; font-weight: 600; }
  </style>
</head>
<body>
  <div class="wrap">
    <h1>Adobe 账号自动登录与 Cookie 提取工具</h1>
    <p class="sub">支持直接粘贴卡密行或分项输入。若卡密含微软邮箱令牌，即使没有 Adobe 密码也会自动通过 Outlook 收取 6 位验证码登录。提取完成后自动关闭并清理本地临时浏览器，复制结果直接粘贴到服务器「添加账号 -> Cookie 导入」即可。</p>
    <form id="form">
      <label>方式一：直接粘贴完整卡密行（自动解析邮箱、微软令牌、密码）
        <textarea id="accountLine" rows="3" placeholder="粘贴如：WaylonCapri8820@outlook.com-—my369352-—9e5f94bc...-—M.C562_BL2...-—4k"></textarea>
      </label>
      <div class="grid">
        <label>方式二：Adobe 账号（邮箱）
          <input id="email" placeholder="如已填上方卡密行可留空">
        </label>
        <label>Adobe 登录密码（可留空走邮箱验证码登录）
          <input id="adobePassword" placeholder="留空则使用微软令牌自动收验证码">
        </label>
      </div>
      <div class="grid">
        <label>微软邮箱 Client ID（选填）
          <input id="mailClientId" placeholder="选填">
        </label>
        <label>微软邮箱 Refresh Token（选填）
          <input id="mailRefreshToken" placeholder="选填">
        </label>
      </div>
      <button id="submitBtn" class="btn" type="submit">一键自动登录并提取 Cookie</button>
    </form>
    <div id="statusBox" class="status"></div>
    <div id="resultBox" class="result">
      <div id="resultMeta" class="meta"></div>
      <label>提取结果（可直接粘贴到服务器后台「添加账号 -> Cookie 导入」）：
        <textarea id="cookieOutput" rows="6" readonly></textarea>
      </label>
      <button id="copyBtn" class="btn btn-copy" type="button">一键复制 Cookie</button>
    </div>
  </div>
  <script>
    const form = document.getElementById('form');
    const statusBox = document.getElementById('statusBox');
    const resultBox = document.getElementById('resultBox');
    const resultMeta = document.getElementById('resultMeta');
    const cookieOutput = document.getElementById('cookieOutput');
    const submitBtn = document.getElementById('submitBtn');
    const copyBtn = document.getElementById('copyBtn');
    let timer = null;

    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      submitBtn.disabled = true;
      resultBox.style.display = 'none';
      statusBox.className = 'status';
      statusBox.style.display = 'block';
      statusBox.textContent = '正在启动...';
      timer = setInterval(async () => {
        try {
          const st = await (await fetch('/api/status')).json();
          if (st.status) statusBox.textContent = st.status;
        } catch {}
      }, 1000);
      try {
        const res = await fetch('/api/extract', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            accountLine: document.getElementById('accountLine').value,
            email: document.getElementById('email').value,
            adobePassword: document.getElementById('adobePassword').value,
            mailClientId: document.getElementById('mailClientId').value,
            mailRefreshToken: document.getElementById('mailRefreshToken').value
          })
        });
        const data = await res.json();
        if (!res.ok || data.error) throw new Error(data.error || '提取失败');
        statusBox.textContent = '提取成功！本地临时浏览器已自动关闭并清理干净。';
        const quota = data.credits ? \`剩余积分：\${data.credits.available} / \${data.credits.total}（套餐类型：\${data.planCap || '未知'}）\` : '已获取登录与风险凭据';
        resultMeta.textContent = \`账号：\${data.email} ｜ \${quota}\`;
        cookieOutput.value = data.cookie;
        resultBox.style.display = 'block';
      } catch (err) {
        statusBox.className = 'status error';
        statusBox.textContent = '提取失败：' + err.message;
      } finally {
        clearInterval(timer);
        submitBtn.disabled = false;
      }
    });

    copyBtn.addEventListener('click', async () => {
      await navigator.clipboard.writeText(cookieOutput.value);
      copyBtn.textContent = '已复制到剪贴板！直接去服务器后台粘贴即可';
      setTimeout(() => { copyBtn.textContent = '一键复制 Cookie'; }, 2500);
    });
  </script>
</body>
</html>`;

if (require.main === module) {
  const server = http.createServer(async (req, res) => {
    if (req.method === 'GET' && (req.url === '/' || req.url.startsWith('/?'))) {
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
      res.end(HTML_PAGE);
      return;
    }
    if (req.method === 'GET' && req.url === '/api/status') {
      res.writeHead(200, { 'Content-Type': 'application/json; charset=utf-8' });
      res.end(JSON.stringify({ status: currentStatus }));
      return;
    }
    if (req.method === 'POST' && req.url === '/api/extract') {
      let body = '';
      req.on('data', chunk => { body += chunk; });
      req.on('end', async () => {
        try {
          const payload = JSON.parse(body || '{}');
          const result = await extractAdobeCookie(payload);
          res.writeHead(200, { 'Content-Type': 'application/json; charset=utf-8' });
          res.end(JSON.stringify(result));
        } catch (err) {
          res.writeHead(400, { 'Content-Type': 'application/json; charset=utf-8' });
          res.end(JSON.stringify({ error: err.message || String(err) }));
        }
      });
      return;
    }
    res.writeHead(404);
    res.end('Not Found');
  });

  server.listen(PORT, '127.0.0.1', () => {
    const url = `http://127.0.0.1:${PORT}`;
    console.log(`Adobe Cookie 自动提取工具 (GUI) 已启动：${url}`);
    try {
      const browserPath = findBrowserPath();
      spawn(browserPath, [`--app=${url}`, '--window-size=840,780'], { detached: true, stdio: 'ignore' }).unref();
    } catch {
      try { execSync(`start "" "${url}"`, { stdio: 'ignore' }); } catch {}
    }
  });
}

module.exports = { extractAdobeCookie, parseAccountLine };
