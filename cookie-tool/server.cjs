// Adobe 账号自动登录与 Cookie（含风险会话 sherlockToken）提取工具
// 启动后访问 http://127.0.0.1:18899 即可输入账号密码或完整卡密行，自动完成登录、验证码收取、风险会话提取并清理本地浏览器缓存。

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

// 解析组合卡密行（支持 ---- 或 -— 或 —— 分隔）
function parseAccountLine(raw) {
  const line = String(raw || '').trim();
  if (!line) return null;
  const parts = line.split(/(?:-+|—+|\u2014+){2,}|-\u2014|\u2014-/).map(s => s.trim()).filter(Boolean);
  if (parts.length >= 6) {
    return {
      email: parts[0],
      emailPassword: parts[1],
      mailClientId: parts[2],
      mailRefreshToken: parts[3],
      note: parts[4],
      adobePassword: parts[parts.length - 1]
    };
  }
  if (parts.length === 2) {
    return { email: parts[0], adobePassword: parts[1] };
  }
  return null;
}

// 通过微软 OAuth2 refresh_token 自动从 Outlook 收件箱读取最新 Adobe 6 位验证码
async function fetchOutlookAdobeCode(clientId, refreshToken, afterTimestampMs) {
  if (!clientId || !refreshToken) return null;
  for (const endpoint of [
    'https://login.microsoftonline.com/common/oauth2/v2.0/token',
    'https://login.live.com/oauth20_token.srf'
  ]) {
    try {
      const tokenRes = await fetch(endpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({
          client_id: clientId,
          grant_type: 'refresh_token',
          refresh_token: refreshToken
        })
      });
      const tokenData = await tokenRes.json();
      if (!tokenData.access_token) continue;

      const mailRes = await fetch(
        'https://graph.microsoft.com/v1.0/me/messages?$top=8&$select=subject,receivedDateTime,bodyPreview,body',
        { headers: { Authorization: `Bearer ${tokenData.access_token}` } }
      );
      const mailData = await mailRes.json();
      const messages = Array.isArray(mailData.value) ? mailData.value : [];
      for (const msg of messages) {
        const receivedAt = Date.parse(msg.receivedDateTime || '') || 0;
        if (afterTimestampMs && receivedAt + 60000 < afterTimestampMs) continue;
        const text = `${msg.subject || ''} ${msg.bodyPreview || ''} ${(msg.body && msg.body.content) || ''}`;
        if (!/adobe|verification|code|验证码/i.test(text)) continue;
        const match = text.match(/\b(\d{6})\b/);
        if (match) return match[1];
      }
    } catch {
      // 忽略单次拉取错误，外层轮询重试
    }
  }
  return null;
}

// 执行单次账号自动登录与 Cookie 提取
async function extractAdobeCookie(params, onProgress = () => {}) {
  const parsed = parseAccountLine(params.accountLine) || {};
  const email = (params.email || parsed.email || '').trim();
  const adobePassword = (params.adobePassword || parsed.adobePassword || '').trim();
  const mailClientId = (params.mailClientId || parsed.mailClientId || '').trim();
  const mailRefreshToken = (params.mailRefreshToken || parsed.mailRefreshToken || '').trim();
  const note = (params.note || parsed.note || '').trim();

  if (!email || !adobePassword) {
    throw new Error('请填写 Adobe 邮箱账号和 Adobe 登录密码（或粘贴完整卡密行）');
  }

  fs.mkdirSync(TMP_ROOT, { recursive: true });
  const profileDir = path.join(TMP_ROOT, `adobe-cookie-tool-${Date.now()}`);
  const debugPort = 19222 + Math.floor(Math.random() * 500);
  const browserPath = findBrowserPath();

  const loginUrl =
    'https://auth.services.adobe.com/en_US/index.html?callback=https%3A%2F%2Fims-na1.adobelogin.com%2Fims%2Fadobeid%2Fclio-playground-web%2FAdobeID%2Ftoken%3Fredirect_uri%3Dhttps%253A%252F%252Ffirefly.adobe.com%252Fgenerate%252Fimage&client_id=clio-playground-web&scope=AdobeID%2Cfirefly_api%2Copenid%2Cpps.read%2Cpps.write%2Cadditional_info.projectedProductContext%2Cadditional_info.ownerOrg%2Cuds_read%2Cuds_write%2Cab.manage%2Cread_organizations%2Cadditional_info.roles%2Caccount_cluster.read%2Ccreative_production%2Ctk_platform%2Ctk_platform_sync%2Caf_byof%2Cprofile%2Cdii_lr_ml_unlimited&state=%7B%22ac%22%3A%22firefly.adobe.com%22%7D&locale=en_US&flow_type=token&idp_flow_type=login';

  onProgress('正在启动独立隔离浏览器...');
  const child = spawn(
    browserPath,
    [
      `--remote-debugging-port=${debugPort}`,
      `--user-data-dir=${profileDir}`,
      '--no-first-run',
      '--no-default-browser-check',
      'about:blank'
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
    // 等待 CDP 端口就绪
    let pageTarget = null;
    for (let i = 0; i < 25; i++) {
      await new Promise(r => setTimeout(r, 400));
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

    onProgress(`正在打开 Adobe 登录页并填写邮箱 ${email}...`);
    await send('Page.navigate', { url: loginUrl });

    // 等待邮箱输入框出现
    for (let i = 0; i < 30; i++) {
      await new Promise(r => setTimeout(r, 1000));
      const check = await send('Runtime.evaluate', {
        expression: `Boolean(document.querySelector('#EmailPage-EmailField'))`,
        returnByValue: true
      });
      if (check.result?.value) break;
    }

    await send('Runtime.evaluate', {
      expression: `(() => {
        const el = document.querySelector('#EmailPage-EmailField');
        if (el) {
          el.focus();
          const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
          setter.call(el, '');
          el.dispatchEvent(new Event('input', { bubbles: true }));
        }
      })()`
    });
    await send('Input.insertText', { text: email });
    await new Promise(r => setTimeout(r, 500));
    await send('Runtime.evaluate', {
      expression: `document.querySelector('[data-id=EmailPage-ContinueButton]')?.click()`
    });

    onProgress('正在等待密码输入框或验证码挑战...');
    for (let step = 0; step < 45; step++) {
      await new Promise(r => setTimeout(r, 1200));
      const stateRes = await send('Runtime.evaluate', {
        expression: `JSON.stringify({
          url: location.href,
          hasPwd: Boolean(document.querySelector('#PasswordPage-PasswordField')),
          hasCodeInput: Boolean(document.querySelector('[data-id=CodeInput-0], input[autocomplete=one-time-code]')),
          hasVerifyBtn: location.hash.includes('challenge/verify') && Boolean(document.querySelector('button[data-id=ChallengePushPage-Continue], button[type=submit]')),
          hasSkipPhone: location.hash.includes('add-security-phone-number') || Boolean(document.querySelector('[data-id=PP-AddSecurityPhoneNumber-skip-btn]')),
          hasFirefly: location.hostname === 'firefly.adobe.com'
        })`,
        returnByValue: true
      });
      const st = JSON.parse(stateRes.result.value);

      if (st.hasFirefly) break;

      if (st.hasSkipPhone) {
        onProgress('检测到绑定手机号提示，正在自动跳过...');
        await send('Runtime.evaluate', {
          expression: `(() => {
            const btn = document.querySelector('[data-id=PP-AddSecurityPhoneNumber-skip-btn]') ||
              Array.from(document.querySelectorAll('button')).find(b => /not now|skip|以后|跳过/i.test(b.innerText));
            if (btn) btn.click();
          })()`
        });
        continue;
      }

      if (st.hasPwd) {
        onProgress('正在自动填写 Adobe 密码并勾选保持登录...');
        await send('Runtime.evaluate', {
          expression: `(() => {
            const el = document.querySelector('#PasswordPage-PasswordField');
            if (el) {
              el.focus();
              const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
              setter.call(el, '');
              el.dispatchEvent(new Event('input', { bubbles: true }));
            }
            const remember = document.querySelector('input[type=checkbox]');
            if (remember && !remember.checked) remember.click();
          })()`
        });
        await send('Input.insertText', { text: adobePassword });
        await new Promise(r => setTimeout(r, 500));
        await send('Runtime.evaluate', {
          expression: `document.querySelector('[data-id=PasswordPage-ContinueButton]')?.click()`
        });
        await new Promise(r => setTimeout(r, 4000));
        continue;
      }

      if (st.hasVerifyBtn && !st.hasCodeInput) {
        onProgress('触发邮箱身份验证，正在请求发送验证码...');
        const sendTime = Date.now();
        await send('Runtime.evaluate', {
          expression: `(() => {
            const btn = document.querySelector('button[data-id=ChallengePushPage-Continue], button[type=submit]') ||
              Array.from(document.querySelectorAll('button')).find(b => /continue|继续/i.test(b.innerText));
            if (btn) btn.click();
          })()`
        });
        await new Promise(r => setTimeout(r, 4000));
        if (mailClientId && mailRefreshToken) {
          onProgress('正在通过邮箱令牌自动读取 Outlook 验证码...');
          for (let retry = 0; retry < 10; retry++) {
            await new Promise(r => setTimeout(r, 3000));
            const code = await fetchOutlookAdobeCode(mailClientId, mailRefreshToken, sendTime);
            if (code) {
              onProgress(`已自动获取邮箱验证码 ${code}，正在填入...`);
              await send('Runtime.evaluate', {
                expression: `(() => {
                  const first = document.querySelector('[data-id=CodeInput-0], input[autocomplete=one-time-code], input[type=number], input[type=text]');
                  if (first) first.focus();
                })()`
              });
              await send('Input.insertText', { text: code });
              await new Promise(r => setTimeout(r, 1000));
              await send('Runtime.evaluate', {
                expression: `document.querySelector('button[type=submit]')?.click()`
              });
              break;
            }
          }
        } else {
          onProgress('请在弹出的浏览器窗口中手动输入邮箱收到的 6 位验证码...');
        }
        continue;
      }
    }

    onProgress('已进入 Firefly 页面，正在等待风险会话（ftr / sherlockToken）生成（约 22 秒）...');
    for (let i = 0; i < 35; i++) {
      await new Promise(r => setTimeout(r, 2000));
      const check = await send('Runtime.evaluate', {
        expression: `JSON.stringify({ url: location.href, hasEl: Boolean(document.querySelector('firefly-image-generation')) })`,
        returnByValue: true
      });
      const st = JSON.parse(check.result.value);
      if (st.url.includes('denied.html')) {
        throw new Error('当前网络节点地区不受 Adobe Firefly 支持（跳转到 denied.html），请开启美国/支持地区 VPN 后重试');
      }
      if (st.hasEl) break;
    }

    // 等待 22 秒让 Forter (ftr) 与指纹完成采集
    await new Promise(r => setTimeout(r, 22000));

    const evalRes = await send('Runtime.evaluate', {
      expression: `(async () => {
        const el = document.querySelector('firefly-image-generation');
        if (!el) return JSON.stringify({ error: '未加载出 Firefly 生图组件，请检查网络节点' });
        const key = Object.keys(el).find(k => k.startsWith('__reactFiber'));
        let fiber = el[key];
        let auth;
        for (let i = 0; fiber && i < 15; i++, fiber = fiber.return) {
          if (fiber.memoizedProps?.service?.host?.authentication) {
            auth = fiber.memoizedProps.service.host.authentication;
            break;
          }
        }
        if (!auth) return JSON.stringify({ error: '未找到认证上下文' });
        const read = s => new Promise(resolve => {
          let d;
          d = s(v => { if (typeof d === 'function') d(); resolve(v); });
        });
        const [token, arp, profile] = await Promise.all([read(auth.accessToken), read(auth.arpSessionToken), read(auth.userProfile)]);
        return JSON.stringify({ token, arp, profile });
      })()`,
      awaitPromise: true,
      returnByValue: true
    });

    const session = JSON.parse(evalRes.result.value);
    if (session.error || !session.token || !session.arp) {
      throw new Error(session.error || '未能获取到完整登录令牌或风险会话');
    }

    const cookiesRes = await send('Network.getAllCookies');
    ws.close();

    // 过滤出 Adobe 相关 Cookie，并将本次新鲜采集的 arp_session 写入 sherlockToken
    const filteredCookies = cookiesRes.cookies.filter(
      c => c.domain.includes('adobe') && c.name !== 'edge_geo_country' && c.name !== 'sherlockToken'
    );
    const cookiePairs = filteredCookies.map(c => `${c.name}=${c.value}`);
    cookiePairs.push(`sherlockToken=${session.arp}`);
    const fullCookie = `${email}----${cookiePairs.join('; ')}`;

    // 查询该账号当前积分与套餐状态
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

    return {
      email,
      note: note || email,
      userId: session.profile?.userId || '',
      cookie: fullCookie,
      arpSession: session.arp,
      credits: creditsInfo?.total?.quota || null,
      planCap: creditsInfo?.total?.planCap || ''
    };
  } finally {
    // 无论成功或失败，立即关闭本地临时浏览器并删除缓存目录，避免本地浏览器刷新 ftr 顶掉服务器会话
    cleanup();
  }
}

const HTML_PAGE = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <title>Adobe Cookie 自动提取工具</title>
  <style>
    :root { --brand: #5758d6; --bg: #f7f8fa; --ink: #242733; --line: #e4e6ed; }
    * { box-sizing: border-box; }
    body { margin: 0; padding: 36px 20px; font-family: "Microsoft YaHei", Inter, sans-serif; background: var(--bg); color: var(--ink); }
    .wrap { max-width: 760px; margin: 0 auto; background: #fff; border: 1px solid var(--line); border-radius: 16px; padding: 28px 32px; box-shadow: 0 8px 30px rgba(37,42,65,.05); }
    h1 { margin: 0 0 6px; font-size: 22px; }
    .sub { margin: 0 0 22px; font-size: 13px; color: #7c808f; }
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
    <p class="sub">提取完成后会自动关闭并销毁本地临时浏览器缓存，复制生成的 Cookie 直接去服务器「添加账号 -> Cookie 导入」粘贴即可。</p>
    <form id="form">
      <label>方式一：直接粘贴完整卡密行（自动识别邮箱、Adobe密码与微软令牌）
        <textarea id="accountLine" rows="3" placeholder="例如：VanderBowen4891@outlook.com----邮箱密码----client_id----refresh_token----备注----Adobe密码"></textarea>
      </label>
      <div class="grid">
        <label>方式二：Adobe 账号（邮箱）
          <input id="email" placeholder="如已填上方卡密行可留空">
        </label>
        <label>Adobe 登录密码
          <input id="adobePassword" placeholder="如已填上方卡密行可留空">
        </label>
      </div>
      <div class="grid">
        <label>微软邮箱 Client ID（选填，用于自动收验证码）
          <input id="mailClientId" placeholder="选填">
        </label>
        <label>微软邮箱 Refresh Token（选填，用于自动收验证码）
          <input id="mailRefreshToken" placeholder="选填">
        </label>
      </div>
      <button id="submitBtn" class="btn" type="submit">一键登录并提取 Cookie</button>
    </form>
    <div id="statusBox" class="status"></div>
    <div id="resultBox" class="result">
      <div id="resultMeta" class="meta"></div>
      <label>提取结果（含邮箱前缀与 sherlockToken 风险会话，可直接粘贴到服务器后台「Cookie 导入」）：
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

    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      submitBtn.disabled = true;
      resultBox.style.display = 'none';
      statusBox.className = 'status';
      statusBox.style.display = 'block';
      statusBox.textContent = '正在启动独立浏览器并执行自动登录（全流程约 35～50 秒，请勿关闭窗口）...';
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

const server = http.createServer(async (req, res) => {
  if (req.method === 'GET' && (req.url === '/' || req.url.startsWith('/?'))) {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end(HTML_PAGE);
    return;
  }
  if (req.method === 'POST' && req.url === '/api/extract') {
    let body = '';
    req.on('data', chunk => { body += chunk; });
    req.on('end', async () => {
      try {
        const payload = JSON.parse(body || '{}');
        const result = await extractAdobeCookie(payload, msg => console.log('[提取进度]', msg));
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
  console.log(`Adobe Cookie 自动提取工具已启动：${url}`);
  try {
    execSync(`start "" "${url}"`, { stdio: 'ignore' });
  } catch {}
});
