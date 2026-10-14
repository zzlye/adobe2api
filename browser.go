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
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

// 浏览器续期串行执行，空闲时不为每个账号保留浏览器进程。
var browserRefreshGate = make(chan struct{}, 1)

// 通过页面已有的认证服务获取会话，避免复制会随网页发布变化的模块编号。
const browserSessionScript = `(async () => {
 const read = source => new Promise((resolve,reject) => {
  let dispose, done=false;
  const timer=setTimeout(()=>reject(new Error('会话读取超时')),10000);
  dispose=source(value=>{if(done)return;done=true;clearTimeout(timer);resolve(value);queueMicrotask(()=>{if(typeof dispose==='function')dispose()})});
 });
 // 页面未透出 SDK 的 allVendorsLoaded，先等待初始采集窗口，再读取近期稳定值。
 // 风险会话有周期更新，短暂稳定即可；只有会话编号的初始值不能作为就绪凭据。
 const readArp = source => new Promise((resolve,reject) => {
  let dispose,timer,latest=null,firstAt=0,changedAt=0,done=false;
  const initial = value => {
   try {
    const encoded=value.replace(/-/g,'+').replace(/_/g,'/');
    const payload=JSON.parse(atob(encoded));
    return payload&&typeof payload==='object'&&Object.keys(payload).every(key=>key==='sid'||key==='metadata');
   }catch{return false}
  };
  const finish = error => {
   if(done)return;done=true;clearTimeout(timer);
   if(typeof dispose==='function')dispose();
   error?reject(error):resolve(latest);
  };
  dispose=source(value=>{
   if(done)return;
   const now=Date.now();
   if(value&&!firstAt)firstAt=now;
   if(value!==latest){latest=value;changedAt=now}
  });
  const check = () => {
   const now=Date.now();
   if(now>deadline)return finish(new Error('Adobe 风险会话尚未完成初始化，请重新导入已登录页面的生图请求'));
   if(latest&&firstAt&&now-firstAt>=20000&&now-changedAt>=1500&&!initial(latest))return finish();
   timer=setTimeout(check,250);
  };
  timer=setTimeout(check,250);
 });
 const find = () => {
  const element=document.querySelector('firefly-image-generation');
  if(!element)return null;
  const key=Object.keys(element).find(key=>key.startsWith('__reactFiber'));
  let fiber=element[key];
  for(let i=0;fiber&&i<15;i++,fiber=fiber.return){
   const auth=fiber.memoizedProps?.service?.host?.authentication;
   if(auth?.accessToken&&auth?.arpSessionToken)return auth;
  }
  return null;
 };
 const deadline=Date.now()+60000;
 let auth;
 while(!(auth=find())){if(Date.now()>deadline)throw new Error('未找到 Adobe 登录会话，请重新导入 Cookie');await new Promise(r=>setTimeout(r,500))}
 if(window.adobeIMS)await window.adobeIMS.refreshToken();
 let token,arp,profile;
 do {
  [token,arp,profile]=await Promise.all([read(auth.accessToken),readArp(auth.arpSessionToken),read(auth.userProfile)]);
  if(token&&arp&&profile?.userId)break;
  if(Date.now()>deadline)throw new Error('Adobe 登录或风险会话已失效，请重新登录并导入 Cookie');
  await new Promise(r=>setTimeout(r,500));
 }while(true);
 return {access_token:token,arp_session:arp,profile,expires_at:window.adobeIMS?.getAccessToken()?.expire};
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
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var allocCtx context.Context
	var allocCancel context.CancelFunc
	endpoint := os.Getenv("ADOBE2API_CDP_URL")
	if endpoint != "" {
		// 连接已登录浏览器时仍核对用户标识，防止混用两个账号的会话。
		allocCtx, allocCancel = remote.NewAllocator(ctx, endpoint)
	} else {
		if account.Cookie == "" {
			return account, errors.New("自动续期需要 Cookie，请在账号池导入登录信息")
		}
		profile, err := os.MkdirTemp("", "adobe2api-browser-")
		if err != nil {
			return account, err
		}
		defer os.RemoveAll(profile)
		options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
		options = append(options, chromedp.UserDataDir(profile), chromedp.Flag("disable-dev-shm-usage", true), chromedp.Flag("disk-cache-size", "8388608"), chromedp.Flag("media-cache-size", "1048576"), chromedp.Flag("disable-gpu", true))
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
		for _, target := range targets {
			if target.Type == "page" && strings.HasPrefix(target.URL, "https://firefly.adobe.com/generate/image") {
				browserCtx, browserCancel = chromedp.NewContext(allocCtx, chromedp.WithTargetID(target.TargetID), chromedp.WithDetachOnCancel())
				defer browserCancel()
				break
			}
		}
	} else {
		if err := chromedp.Do(browserCtx, chromedp.Navigate("about:blank")); err != nil {
			return account, fmt.Errorf("启动续期浏览器失败: %w", err)
		}
		request := &http.Request{Header: http.Header{"Cookie": []string{account.Cookie}}}
		// 原始 Cookie 文本没有域信息，将 Adobe SSO Cookie 写入其实际认证域。
		for _, cookie := range request.Cookies() {
			for _, domain := range []string{".adobe.com", ".services.adobe.com", ".adobelogin.com"} {
				secure := true
				_, err := chromedp.Call(browserCtx, network.SetCookie, network.SetCookieParams{Name: cookie.Name, Value: cookie.Value, Domain: domain, Path: "/", Secure: &secure})
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
	var session browserSession
	for {
		current, err := chromedp.Run(browserCtx, chromedp.Evaluate[browserSession](browserSessionScript, chromedp.EvalAwaitPromise))
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
	if account.UserID != "" && account.UserID != session.Profile.UserID {
		return account, errors.New("浏览器登录账号与账号池记录不一致")
	}
	cookies, err := chromedp.Call(browserCtx, storage.GetCookies, storage.GetCookiesParams{})
	if err != nil {
		return account, errors.New("读取更新后的 Cookie 失败")
	}
	values := map[string]string{}
	for _, cookie := range cookies.Cookies {
		domain := strings.TrimPrefix(cookie.Domain, ".")
		if domain == "adobe.com" || strings.HasSuffix(domain, ".adobe.com") || domain == "adobelogin.com" || strings.HasSuffix(domain, ".adobelogin.com") {
			values[cookie.Name] = cookie.Value
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var pairs []string
	for _, name := range names {
		pairs = append(pairs, name+"="+values[name])
	}
	if len(pairs) > 0 {
		account.Cookie = strings.Join(pairs, "; ")
	}
	account.AccessToken = session.AccessToken
	account.ARPSession = session.ARPSession
	account.UserID = session.Profile.UserID
	account.ExpiresAt = parseBrowserExpiry(session.ExpiresAt, session.AccessToken)
	account.LastRefresh = time.Now().UTC()
	account.LastError = ""
	account.Status = "ready"
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
