package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

// 浏览器续期串行执行，空闲时不为每个账号保留浏览器进程。
var browserRefreshGate = make(chan struct{}, 1)

// 页面定位与会话读取各有独立等待窗口，额外预留浏览器启动和 IMS 请求时间。
const accountRefreshTimeout = 150 * time.Second

// 通过页面已有的认证服务获取会话，避免复制会随网页发布变化的模块编号。
const browserSessionScript = `(async () => {
 const find = () => {
  const element=document.querySelector('firefly-image-generation');
  if(!element)return null;
  const key=Object.keys(element).find(key=>key.startsWith('__reactFiber'));
  let fiber=element[key];
  for(let i=0;fiber&&i<15;i++,fiber=fiber.return){
   const auth=fiber.memoizedProps?.service?.host?.authentication;
   if(auth?.accessToken&&auth?.arpSessionToken&&auth?.userProfile)return auth;
  }
  return null;
 };
 const findDeadline=Date.now()+60000;
 let auth;
 while(!(auth=find())){if(Date.now()>findDeadline)throw new Error('Adobe 生图页面未加载完成，请检查网络后重试');await new Promise(r=>setTimeout(r,500))}
 try{if(window.adobeIMS&&!window.adobeIMS.getAccessToken?.()?.token)await window.adobeIMS.refreshToken()}catch{}
 // SDK 续签和风险采集是两个阶段，前一步不能挤占后面的完整等待窗口。
 const deadline=Date.now()+60000;
 return await new Promise((resolve,reject) => {
  let token=null,profile=null,arp=null,firstAt=0,changedAt=0,timer,done=false;
  const disposers=[];
  const validArp = value => {
   if(typeof value!=='string'||!value.trim())return false;
   try {
    const payload=JSON.parse(atob(value.replace(/-/g,'+').replace(/_/g,'/')));
    if(!payload||typeof payload!=='object'||Array.isArray(payload)||typeof payload.sid!=='string'||!payload.sid.trim())return false;
    if(!['ftr','ark','bfp'].every(key=>typeof payload[key]==='string'&&payload[key].trim()))return false;
    // 内部标记不作为失效依据，供应商完整原值交由真实生成验证。
    const timestamp=Number(payload.ftr.split('_')[1]);
    return !(timestamp>1700000000000&&Date.now()-timestamp>=7*24*60*60*1000);
   }catch{return false}
  };
  const finish = error => {
   if(done)return;done=true;clearTimeout(timer);
   for(const dispose of disposers)try{dispose()}catch{}
   if(error)return reject(error);
   resolve({access_token:token,arp_session:arp,profile,expires_at:window.adobeIMS?.getAccessToken?.()?.expire});
  };
  const subscribe = (source,receive) => {
   const dispose=source(value=>{if(!done)receive(value)});
   if(typeof dispose==='function')disposers.push(dispose);
  };
  try {
   // 持续保留最新值：空首值不能结束订阅，更不能重新启动一轮二十秒风险等待。
   subscribe(auth.accessToken,value=>{token=value});
   subscribe(auth.userProfile,value=>{profile=value});
   subscribe(auth.arpSessionToken,value=>{
    const now=Date.now();
    if(value&&!firstAt)firstAt=now;
    if(value!==arp){arp=value;changedAt=now}
   });
  }catch{return finish(new Error('Adobe 登录会话读取失败，请稍后重试'))}
  const check = () => {
   const now=Date.now();
   const acceptBtn=typeof document!=='undefined'&&document.querySelector?document.querySelector('#onetrust-accept-btn-handler'):null;
   if(typeof acceptBtn?.click==='function')acceptBtn.click();
   if(typeof window!=='undefined'&&typeof window.OneTrust?.AllowAll==='function')try{window.OneTrust.AllowAll()}catch{}
   const loginReady=typeof token==='string'&&token.trim()&&typeof profile?.userId==='string'&&profile.userId.trim();
   // 三路数据在同一窗口核对；风险周期更新时短暂稳定即可，无需反复重新采集。
   if(loginReady&&firstAt&&now-firstAt>=20000&&now-changedAt>=1500&&validArp(arp))return finish();
   if(now>=deadline)return finish(new Error(loginReady?'Adobe 风险会话尚未完成初始化，请打开已登录的 Adobe 生图页面后重新同步':'Adobe 登录会话尚未就绪，请稍后重新同步'));
   timer=setTimeout(check,250);
  };
  timer=setTimeout(check,250);
 });
})()`

// 已有访问令牌与用户标识时，仅在无 Cookie 的纯净浏览器中采集四项风险会话，绝不触碰或轮换 IMS 登录 Cookie。
const arpOnlySessionScript = `(async () => {
 const find = () => {
  const element=document.querySelector('firefly-image-generation');
  if(!element)return null;
  const key=Object.keys(element).find(key=>key.startsWith('__reactFiber'));
  let fiber=element[key];
  for(let i=0;fiber&&i<15;i++,fiber=fiber.return){
   const auth=fiber.memoizedProps?.service?.host?.authentication;
   if(auth?.arpSessionToken)return auth;
  }
  return null;
 };
 const findDeadline=Date.now()+60000;
 let auth;
 while(!(auth=find())){if(Date.now()>findDeadline)throw new Error('Adobe 生图页面未加载完成，请检查网络后重试');await new Promise(r=>setTimeout(r,500))}
 const deadline=Date.now()+60000;
 return await new Promise((resolve,reject) => {
  let arp=null,firstAt=0,changedAt=0,timer,done=false,dispose=null;
  const validArp = value => {
   if(typeof value!=='string'||!value.trim())return false;
   try {
    const payload=JSON.parse(atob(value.replace(/-/g,'+').replace(/_/g,'/')));
    if(!payload||typeof payload!=='object'||Array.isArray(payload)||typeof payload.sid!=='string'||!payload.sid.trim())return false;
    if(!['ftr','ark','bfp'].every(key=>typeof payload[key]==='string'&&payload[key].trim())||payload.ftr.includes('___'))return false;
    const timestamp=Number(payload.ftr.split('_')[1]);
    return !(timestamp>1700000000000&&Date.now()-timestamp>=7*24*60*60*1000);
   }catch{return false}
  };
  const finish = error => {
   if(done)return;done=true;clearTimeout(timer);
   try{if(typeof dispose==='function')dispose()}catch{}
   if(error)return reject(error);
   resolve({arp_session:arp});
  };
  try {
   dispose=auth.arpSessionToken(value=>{
    if(done)return;
    const now=Date.now();
    if(value&&!firstAt)firstAt=now;
    if(value!==arp){arp=value;changedAt=now}
   });
  }catch{return finish(new Error('Adobe 风险会话读取失败，请稍后重试'))}
  const check = () => {
   const now=Date.now();
   const acceptBtn=typeof document!=='undefined'&&document.querySelector?document.querySelector('#onetrust-accept-btn-handler'):null;
   if(typeof acceptBtn?.click==='function')acceptBtn.click();
   if(typeof window!=='undefined'&&typeof window.OneTrust?.AllowAll==='function')try{window.OneTrust.AllowAll()}catch{}
   if(firstAt&&now-firstAt>=12000&&now-changedAt>=1500&&validArp(arp))return finish();
   if(now>=deadline)return finish(new Error('Adobe 风险会话尚未完成初始化，请稍后重试'));
   timer=setTimeout(check,250);
  };
  timer=setTimeout(check,250);
 });
})()`

type browserSession struct {
	AccessToken string          `json:"access_token"`
	ARPSession  string          `json:"arp_session"`
	ExpiresAt   json.RawMessage `json:"expires_at"`
	Profile     struct {
		UserID string `json:"userId"`
		Email  string `json:"email"`
		Name   string `json:"displayName"`
	} `json:"profile"`
}

func (a *App) refreshBrowser(ctx context.Context, account Account) (Account, error) {
	select {
	case browserRefreshGate <- struct{}{}:
		defer func() { <-browserRefreshGate }()
	case <-ctx.Done():
		return account, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, accountRefreshTimeout)
	defer cancel()
	endpoint := browserEndpointForAccount(account, os.Getenv("ADOBE2API_CDP_URL"))
	if account.RiskRefreshRequired {
		// 风险拒绝后使用独立会话，不能从已连接页面再次读回同一个旧值。
		endpoint = ""
	}
	updated, err := a.refreshBrowserSession(ctx, account, endpoint)
	if err != nil && endpoint != "" && account.Cookie != "" && ctx.Err() == nil {
		// 当前登录页可能是池内另一个账号，失败时改用本账号 Cookie 独立续期。
		return a.refreshBrowserSession(ctx, account, "")
	}
	return updated, err
}

// Cookie-only 账号尚不能核对当前浏览器身份，必须放在独立浏览器中恢复。
func browserEndpointForAccount(account Account, endpoint string) string {
	if account.Cookie != "" && account.UserID == "" {
		return ""
	}
	return strings.TrimSpace(endpoint)
}

func validateBrowserSession(account Account, session browserSession) error {
	if session.AccessToken == "" || session.ARPSession == "" || session.Profile.UserID == "" {
		return errors.New("Adobe 登录会话不完整，请重新登录后同步")
	}
	if account.UserID != "" && account.UserID != session.Profile.UserID {
		return errors.New("浏览器登录账号与账号池记录不一致")
	}
	if arpSessionExpired(session.ARPSession, time.Now()) {
		return errors.New("Adobe 风险会话尚未完成初始化，请重新同步")
	}
	return nil
}

func (a *App) refreshBrowserSession(ctx context.Context, account Account, endpoint string) (Account, error) {
	arpOnly := endpoint == "" && account.AccessToken != "" && adobeUserID(account) != ""
	var allocCtx context.Context
	var allocCancel context.CancelFunc
	if endpoint != "" {
		// 连接已登录浏览器时仍核对用户标识，防止混用两个账号的会话。
		allocCtx, allocCancel = remote.NewAllocator(ctx, endpoint)
	} else {
		if !arpOnly && account.Cookie == "" {
			return account, errors.New("自动续期需要 Cookie，请在账号池导入登录信息")
		}
		profile, err := os.MkdirTemp("", "adobe2api-browser-")
		if err != nil {
			return account, err
		}
		defer os.RemoveAll(profile)
		options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
		options = append(options,
			chromedp.UserDataDir(profile),
			chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"),
			chromedp.WindowSize(1920, 1080),
			chromedp.Flag("disable-blink-features", "AutomationControlled"),
			chromedp.Flag("disable-dev-shm-usage", true),
			chromedp.Flag("disk-cache-size", "8388608"),
			chromedp.Flag("media-cache-size", "1048576"),
			chromedp.Flag("disable-gpu", true),
		)
		if path := os.Getenv("ADOBE2API_CHROME"); path != "" {
			options = append(options, chromedp.ExecPath(path))
		}
		if os.Getenv("ADOBE2API_CHROME_NO_SANDBOX") == "1" {
			options = append(options, chromedp.NoSandbox)
		}
		if proxy := a.store.Settings().ProxyURL; proxy != "" {
			parsed, err := url.Parse(proxy)
			if err != nil {
				return account, errors.New("代理地址无效")
			}
			if parsed.User != nil {
				return account, errors.New("浏览器续期暂不支持带账号密码的代理，请使用本地代理出口")
			}
			options = append(options, chromedp.ProxyServer(proxy))
		}
		allocCtx, allocCancel = chromedp.NewExecAllocator(ctx, options...)
	}
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()
	if endpoint != "" {
		targets, err := chromedp.Targets(browserCtx)
		if err != nil {
			return account, fmt.Errorf("连接浏览器失败: %w", err)
		}
		found := false
		for _, target := range targets {
			if target.Type == "page" && strings.HasPrefix(target.URL, "https://firefly.adobe.com/generate/image") {
				browserCtx, browserCancel = chromedp.NewContext(browserCtx, chromedp.WithTargetID(target.TargetID), chromedp.WithDetachOnCancel())
				defer browserCancel()
				found = true
				break
			}
		}
		if !found {
			return account, errors.New("已连接的浏览器没有打开 Adobe 生图页面，请先打开并登录")
		}
	} else {
		if err := chromedp.Do(browserCtx, chromedp.Navigate("about:blank")); err != nil {
			return account, fmt.Errorf("启动续期浏览器失败: %w", err)
		}
		if !arpOnly {
			request := &http.Request{Header: http.Header{"Cookie": []string{account.Cookie}}}
			skipCookies := map[string]bool{
				"OptanonConsent":        true,
				"OptanonAlertBoxClosed": true,
				"kndctr_9E1005A551ED61CA0A490D45_AdobeOrg_consent": true,
				"edge_geo_country": true,
				"sherlockToken":    true,
				"forter":           true,
				"forterToken":      true,
				"ftrset":           true,
				"ftr_ncd":          true,
				"ftr_blst_1h":      true,
				"arkose":           true,
				"bfp":              true,
				"ff_session_guid":  true,
			}
			// 仅向 .adobe.com 写入认证 Cookie，避免 .services.adobe.com 残留旧值覆盖浏览器旋转后的新 ims_sid。
			for _, cookie := range request.Cookies() {
				if skipCookies[cookie.Name] || strings.HasPrefix(cookie.Name, "TR10309906466_") {
					continue
				}
				secure := true
				_, err := chromedp.Call(browserCtx, network.SetCookie, network.SetCookieParams{Name: cookie.Name, Value: cookie.Value, Domain: ".adobe.com", Path: "/", Secure: &secure})
				if err != nil {
					return account, errors.New("写入浏览器会话失败")
				}
			}
		}
		// 直接发送导航，不等待广告和统计资源结束；登录服务由下方脚本自行等待。
		navigation, err := chromedp.Call(browserCtx, page.Navigate, page.NavigateParams{URL: "https://firefly.adobe.com/generate/image"})
		if err != nil {
			return account, fmt.Errorf("打开 Adobe 续期页面失败: %w", err)
		}
		if navigation.ErrorText != "" {
			return account, errors.New("Adobe 续期页面连接失败，请检查出口")
		}
	}
	script := browserSessionScript
	if arpOnly {
		script = arpOnlySessionScript
	}
	var session browserSession
	for {
		current, err := chromedp.Run(browserCtx, chromedp.Evaluate[browserSession](script, chromedp.EvalAwaitPromise))
		if err == nil {
			session = current
			break
		}
		// 页面跳转会销毁原执行环境，只重试该类临时错误，不处理登录挑战。
		message := err.Error()
		if !strings.Contains(message, "Execution context was destroyed") && !strings.Contains(message, "Cannot find context with specified id") && !strings.Contains(message, "Inspected target navigated") {
			return account, fmt.Errorf("浏览器续期失败: %w", err)
		}
		select {
		case <-ctx.Done():
			return account, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if arpOnly {
		if arpSessionExpired(session.ARPSession, time.Now()) {
			return account, errors.New("Adobe 风险会话尚未完成初始化，请重新同步")
		}
		account.ARPSession = session.ARPSession
		account.LastRefresh = time.Now().UTC()
		account.LastError = ""
		account.Status = "unverified"
		return account, nil
	}
	if err := validateBrowserSession(account, session); err != nil {
		return account, err
	}
	// 只导出 IMS 实际目标可发送的 Cookie，避免其他 Adobe 域的同名旧值覆盖登录。
	cookies, err := chromedp.Call(browserCtx, network.GetCookies, network.GetCookiesParams{URLs: []string{adobeIMSURL}})
	if err != nil {
		return account, errors.New("读取更新后的 Cookie 失败")
	}
	if cookie := browserCookieHeader(cookies.Cookies, adobeIMSURL); cookie != "" {
		account.Cookie = cookie
	} else {
		return account, errors.New("浏览器没有可用于 Adobe 登录续期的 Cookie，请重新登录")
	}
	account.AccessToken = session.AccessToken
	account.ARPSession = session.ARPSession
	account.UserID = session.Profile.UserID
	account.ExpiresAt = parseBrowserExpiry(session.ExpiresAt, session.AccessToken)
	account.LastRefresh = time.Now().UTC()
	account.LastError = ""
	// 会话读取成功只表示拿到了凭据，真实生图成功后才标记账号可用。
	account.Status = "unverified"
	if account.Email == "" {
		account.Email = session.Profile.Email
	}
	if account.Name == "" {
		account.Name = session.Profile.Name
	}
	if account.APIKey == "" {
		account.APIKey = "clio-playground-web"
	}
	if account.ClientID == "" {
		account.ClientID = "clio-playground-web"
	}
	return account, nil
}

// 同名 Cookie 优先采用目标域最具体的作用域，其次是更具体的路径，结果与返回顺序无关。
func browserCookieHeader(cookies []*network.Cookie, targetURL string) string {
	parsed, err := url.Parse(targetURL)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	requestPath := parsed.Path
	if requestPath == "" {
		requestPath = "/"
	}
	selected := make(map[string]*network.Cookie)
	now := float64(time.Now().Unix())
	for _, cookie := range cookies {
		if cookie == nil || !validCaptureCookie(cookie.Name, cookie.Value) || (cookie.Secure && parsed.Scheme != "https") || (!cookie.Session && cookie.Expires > 0 && cookie.Expires <= now) {
			continue
		}
		domain := strings.ToLower(strings.TrimPrefix(cookie.Domain, "."))
		if domain == "" || (host != domain && !(strings.HasPrefix(cookie.Domain, ".") && strings.HasSuffix(host, "."+domain))) {
			continue
		}
		path := cookie.Path
		if path == "" {
			path = "/"
		}
		if requestPath != path && !(strings.HasPrefix(requestPath, path) && (strings.HasSuffix(path, "/") || (len(requestPath) > len(path) && requestPath[len(path)] == '/'))) {
			continue
		}
		previous := selected[cookie.Name]
		if previous == nil || browserCookieMoreSpecific(cookie, previous) {
			selected[cookie.Name] = cookie
		}
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, name+"="+selected[name].Value)
	}
	return strings.Join(pairs, "; ")
}

func browserCookieMoreSpecific(current, previous *network.Cookie) bool {
	currentDomain := strings.TrimPrefix(current.Domain, ".")
	previousDomain := strings.TrimPrefix(previous.Domain, ".")
	if len(currentDomain) != len(previousDomain) {
		return len(currentDomain) > len(previousDomain)
	}
	currentPath, previousPath := current.Path, previous.Path
	if currentPath == "" {
		currentPath = "/"
	}
	if previousPath == "" {
		previousPath = "/"
	}
	if len(currentPath) != len(previousPath) {
		return len(currentPath) > len(previousPath)
	}
	if current.Secure != previous.Secure {
		return current.Secure
	}
	if current.Expires != previous.Expires {
		return current.Expires > previous.Expires
	}
	// 相同作用域通常只有一项，最后的固定排序防止分区等重复值造成随机覆盖。
	return current.Value > previous.Value
}

// IMS 不同版本会返回日期、秒或毫秒时间戳，缺失时读取令牌中的有效期提示。
func parseBrowserExpiry(raw json.RawMessage, token string) time.Time {
	var value any
	if len(raw) > 0 && json.Unmarshal(raw, &value) == nil {
		switch expiry := value.(type) {
		case float64:
			if result := browserUnixExpiry(expiry); !result.IsZero() {
				return result
			}
		case string:
			if parsed, err := time.Parse(time.RFC3339Nano, expiry); err == nil {
				return parsed.UTC()
			}
			if numeric, err := strconv.ParseFloat(expiry, 64); err == nil {
				if result := browserUnixExpiry(numeric); !result.IsZero() {
					return result
				}
			}
		}
	}
	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		if payload, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Expires float64 `json:"exp"`
			}
			if json.Unmarshal(payload, &claims) == nil {
				return browserUnixExpiry(claims.Expires)
			}
		}
	}
	return time.Time{}
}

func browserUnixExpiry(value float64) time.Time {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 || value > 253402300799000 {
		return time.Time{}
	}
	if value > 100000000000 {
		return time.UnixMilli(int64(value)).UTC()
	}
	return time.Unix(int64(value), 0).UTC()
}
