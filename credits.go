package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const adobeCreditsURL = "https://firefly.adobe.io/v1/credits/balance"

// adobeCreditQuota 使用指针区分明确返回的零与缺失字段。
type adobeCreditQuota struct {
	Total     *float64 `json:"total"`
	Used      *float64 `json:"used"`
	Available *float64 `json:"available"`
}

type adobeCreditBalance struct {
	Total struct {
		Quota          adobeCreditQuota `json:"quota"`
		PlanCap        string           `json:"planCap"`
		AvailableUntil string           `json:"availableUntil"`
	} `json:"total"`
}

// validCreditQuota 排除不完整、负数及余额与使用量互相矛盾的快照。
func validCreditQuota(quota adobeCreditQuota) bool {
	if quota.Total == nil || quota.Used == nil || quota.Available == nil {
		return false
	}
	for _, value := range []*float64{quota.Total, quota.Used, quota.Available} {
		if math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
			return false
		}
	}
	return math.Abs(*quota.Total-*quota.Used-*quota.Available) <= 0.000001
}

// creditConsumption 仅比较同一额度周期；套餐变化、重置或数据矛盾时保持未知。
func creditConsumption(before, after *adobeCreditBalance) *float64 {
	if before == nil || after == nil || !validCreditQuota(before.Total.Quota) || !validCreditQuota(after.Total.Quota) {
		return nil
	}
	if before.Total.PlanCap == "" || before.Total.PlanCap != after.Total.PlanCap || before.Total.AvailableUntil == "" || before.Total.AvailableUntil != after.Total.AvailableUntil {
		return nil
	}
	periodEnd, err := time.Parse(time.RFC3339Nano, before.Total.AvailableUntil)
	if err != nil || !periodEnd.After(time.Now()) {
		return nil
	}
	if math.Abs(*before.Total.Quota.Total-*after.Total.Quota.Total) > 0.000001 {
		return nil
	}
	used := *after.Total.Quota.Used - *before.Total.Quota.Used
	available := *before.Total.Quota.Available - *after.Total.Quota.Available
	if used < 0 || available < 0 || math.Abs(used-available) > 0.000001 {
		return nil
	}
	return &used
}

// readCreditBalance 属于辅助记录，读取失败不会中断生图或猜测扣费数字。
func (g *AdobeGenerator) readCreditBalance(ctx context.Context, client *http.Client, account Account, log *RequestLog) *adobeCreditBalance {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	balance, err := g.fetchCreditBalance(ctx, client, account, log)
	if err != nil {
		return nil
	}
	return balance
}

func (g *AdobeGenerator) fetchCreditBalance(ctx context.Context, client *http.Client, account Account, log *RequestLog) (*adobeCreditBalance, error) {
	userID := adobeUserID(account)
	if account.AccessToken == "" || userID == "" {
		return nil, errors.New("账号缺少积分查询凭据")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, adobeCreditsURL, nil)
	if err != nil {
		return nil, err
	}
	// 积分计数器使用独立的公开客户端标识，凭据不进入请求记录。
	request.Header.Set("Authorization", "Bearer "+account.AccessToken)
	request.Header.Set("X-Api-Key", "SunbreakWebUI1")
	request.Header.Set("X-Account-Id", userID)
	request.Header.Set("Accept", "application/json")
	response, err := g.do(client, request, log)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, errors.New("Adobe 积分余额暂时不可用")
	}
	var balance adobeCreditBalance
	if err := json.NewDecoder(io.LimitReader(response.Body, 128<<10)).Decode(&balance); err != nil {
		return nil, errors.New("Adobe 积分余额响应无效")
	}
	if !validCreditQuota(balance.Total.Quota) {
		return nil, errors.New("Adobe 积分余额信息不完整")
	}
	return &balance, nil
}

// inferAccountPlan 根据积分总额度与配额上限类型推断账号的 Firefly 会员等级。
func inferAccountPlan(total float64, planCap string) string {
	switch {
	case total >= 45000:
		return "premium"
	case total >= 9000:
		return "pro_plus"
	case total >= 3500:
		return "pro"
	case total >= 1500:
		return "standard"
	case planCap == "FREE" || total <= 100:
		return "free"
	default:
		return "paid"
	}
}

// applyCreditBalance 将上游返回的可用积分、总额度与会员等级写入账号。
func applyCreditBalance(account *Account, balance *adobeCreditBalance) {
	if account == nil || balance == nil || !validCreditQuota(balance.Total.Quota) {
		return
	}
	available := *balance.Total.Quota.Available
	total := *balance.Total.Quota.Total
	account.Credits = &available
	account.CreditsTotal = &total
	account.Plan = inferAccountPlan(total, balance.Total.PlanCap)
	if until := strings.TrimSpace(balance.Total.AvailableUntil); until != "" {
		account.PlanExpiresAt = until
	}
}

// effectiveAccountPlan 返回账号当前识别出的会员等级，兼容旧数据按总额或可用余额推断。
func effectiveAccountPlan(account Account) string {
	if account.Plan != "" {
		return account.Plan
	}
	if account.CreditsTotal != nil {
		return inferAccountPlan(*account.CreditsTotal, "")
	}
	if account.Credits != nil {
		switch {
		case *account.Credits >= 45000:
			return "premium"
		case *account.Credits >= 9000:
			return "pro_plus"
		case *account.Credits >= 3500:
			return "pro"
		case *account.Credits >= 1500:
			return "standard"
		}
	}
	return ""
}

// inputRequiresPremiumForFree 判断当前请求是否只有 Firefly Premium（5万积分）才能免积分生图。
func inputRequiresPremiumForFree(input GenerateInput) bool {
	if input.Resolution == "4K" {
		return true
	}
	// 21:9 的 2K（3024x1296）及任何单边超过 2560 的尺寸在 Adobe 计费中按 4K 计费。
	if input.Resolution == "2K" && input.AspectRatio == "21:9" {
		return true
	}
	if input.Size != "" && input.Size != "auto" {
		if w, h, err := parseAdobeSize(input.Size); err == nil && (w > 2560 || h > 2560) {
			return true
		}
	}
	// GPT Image 全系列（含 gpt-image-2 与 gpt-image-2.5）在 high / max 画质（detailLevel > 3）下仅 Premium 套餐免积分。
	if strings.HasPrefix(input.Model, "gpt-image") {
		if input.Quality == "high" || input.Quality == "max" {
			return true
		}
		if input.DetailLevel != nil && *input.DetailLevel > 3 {
			return true
		}
	}
	return false
}

// accountCoversFreeGeneration 判断账号会员等级在当前请求分辨率、尺寸与画质下是否支持免积分无限生图。
func accountCoversFreeGeneration(account Account, input GenerateInput) bool {
	switch effectiveAccountPlan(account) {
	case "premium":
		return true
	case "pro_plus":
		return !inputRequiresPremiumForFree(input)
	case "pro":
		return !inputRequiresPremiumForFree(input) && (input.Resolution == "" || input.Resolution == "512" || input.Resolution == "1K") && (input.Model == "" || input.Model == "gemini-3.1-flash-image-preview")
	case "standard", "free", "paid":
		return false
	default:
		// 尚未完成首次积分与会员查询的账号允许参与首次刷新校验。
		return account.Credits == nil && account.CreditsTotal == nil
	}
}

