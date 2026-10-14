package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
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
