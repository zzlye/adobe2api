package main

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// 会话门按账号隔离；生图共享读侧，凭据轮换独占写侧，不串行化正常生成。
func (a *App) sessionGate(id string) *accountSessionGate {
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	if a.sessionGates == nil {
		a.sessionGates = make(map[string]*accountSessionGate)
	}
	if a.sessionGates[id] == nil {
		a.sessionGates[id] = &accountSessionGate{}
	}
	return a.sessionGates[id]
}

// 仅在 IMS 明确拒绝登录凭据时停止自动重试；网络故障、408 和普通令牌失效仍可恢复。
func adobeLoginRequired(err error) bool {
	var upstream *UpstreamError
	return errors.As(err, &upstream) && upstream.StatusCode == http.StatusUnauthorized && upstream.Code == "invalid_credentials"
}

func accountNeedsRefresh(account Account, now time.Time) bool {
	return account.RiskRefreshRequired || account.AccessToken == "" || account.UserID == "" || account.Status == "pending" || account.Status == "expired" || (!account.ExpiresAt.IsZero() && account.ExpiresAt.Sub(now) < time.Minute) || (account.Cookie != "" && arpSessionExpired(account.ARPSession, now))
}

// 等待会话门后重新取凭据，预约请求时的快照可能已被前一个续期更新。
func (a *App) readGenerationAccount(ctx context.Context, id string) (Account, func(), error) {
	release, err := a.sessionGate(id).acquire(ctx, false)
	if err != nil {
		return Account{}, nil, err
	}
	account, err := a.findAdminAccount(id)
	if err != nil {
		release()
		return Account{}, nil, err
	}
	return account, release, nil
}

// 保存时保留管理员的开关和限额；旧请求不能覆盖较新续期的凭据及恢复状态。
func (a *App) saveGenerationAccount(account Account) error {
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	current, err := a.findAdminAccount(account.ID)
	if err != nil {
		return err
	}
	account.Name, account.Enabled, account.RequestsPerMinute = current.Name, current.Enabled, current.RequestsPerMinute
	if current.LastRefresh.After(account.LastRefresh) {
		account.Cookie, account.AccessToken, account.RefreshToken, account.ARPSession = current.Cookie, current.AccessToken, current.RefreshToken, current.ARPSession
		account.ExpiresAt, account.LastRefresh = current.ExpiresAt, current.LastRefresh
		account.RiskRefreshRequired = current.RiskRefreshRequired
		account.LoginRequired = current.LoginRequired
		account.Status, account.LastError = current.Status, current.LastError
	}
	return a.store.SaveAccount(account)
}

// 已结束的生成请求才申请写侧，避免多个失败请求相互等待升级读锁。
func (a *App) refreshGenerationAccount(ctx context.Context, failed Account, rejection *UpstreamError) (Account, error) {
	release, err := a.sessionGate(failed.ID).acquire(ctx, true)
	if err != nil {
		return failed, err
	}
	defer release()
	current, err := a.findAdminAccount(failed.ID)
	if err != nil {
		return failed, err
	}
	// 已排队的失败请求也必须复用认证失败结论，不能再次轮换已被拒绝的凭据。
	if current.LoginRequired {
		return current, errors.New("Adobe 登录状态已失效，请重新同步登录信息")
	}
	if rejection == nil {
		if !accountNeedsRefresh(current, time.Now()) {
			return current, nil
		}
	} else {
		// 同时失败的请求复用第一个恢复结果；408 必须比较风险会话，不能只比较令牌刷新时间。
		changed := rejection.StatusCode == http.StatusUnauthorized && current.AccessToken != failed.AccessToken || rejection.StatusCode == http.StatusRequestTimeout && current.ARPSession != failed.ARPSession
		if changed && !accountNeedsRefresh(current, time.Now()) {
			return current, nil
		}
		if current.RiskRefreshRequired && current.LastRefresh.After(failed.LastRefresh) {
			// 同一批失败请求共享前一次恢复失败，不能紧接着重复启动浏览器。
			return current, errors.New("账号风险会话尚未恢复，请稍后重试")
		}
		if rejection.StatusCode == http.StatusRequestTimeout {
			current.RiskRefreshRequired = true
		}
	}
	refreshCtx, cancel := context.WithTimeout(ctx, accountRefreshTimeout)
	defer cancel()
	updated, refreshErr := a.generator.Refresh(refreshCtx, current)
	if refreshErr != nil {
		updated = mergeConfirmedRefresh(current, updated)
		// 记录本轮尝试，阻止旧请求的成功收尾把恢复意图和失败状态覆盖回去。
		updated.LastRefresh = time.Now().UTC()
		updated.Status, updated.LastError = "error", safeError(refreshErr)
		updated.LoginRequired = updated.LoginRequired || adobeLoginRequired(refreshErr)
	} else {
		updated.LoginRequired = false
	}
	// 必须在释放写侧之前落盘，否则等待者仍可能拿旧 Cookie 再次续签。
	if saveErr := a.saveGenerationAccount(updated); saveErr != nil {
		return updated, errors.Join(refreshErr, saveErr)
	}
	return updated, refreshErr
}
