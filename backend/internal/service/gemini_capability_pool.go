package service

import (
	"context"
	"time"
)

// GeminiCapabilityMissCoversPool 判断「所有账号都不支持这个请求」是否成立：分组里每一个启用的、
// 声明支持该模型的账号都在 tried（本次请求已经试过、并且都是 capability_miss）里。
//
// 选号失败本身不是证明：选号还会过滤掉暂时不可调度的账号（限流、过载、RPM、额度、窗口费用、
// 利润门、售价区间等），这些账号没有被验证过。所以这里按分组的完整账号列表判断，只排除管理员停用、
// 不可调度开关关闭、已过期、平台不符或不支持该模型的账号；任何一个没试过就返回 false。
// 查询失败也返回 false（无法证明时按上游故障处理）。
func (s *GatewayService) GeminiCapabilityMissCoversPool(ctx context.Context, groupID *int64, requestedModel string, tried map[int64]struct{}) bool {
	if s == nil || s.accountRepo == nil || len(tried) == 0 {
		return false
	}
	group, err := s.resolveGroupByIDForSellRate(ctx, groupID)
	if err != nil || group == nil {
		return false
	}
	ctx = s.withGroupContext(ctx, group)
	platform, hasForcePlatform, err := s.resolvePlatform(ctx, groupID, group, requestedModel)
	if err != nil {
		return false
	}
	useMixed := (platform == PlatformAnthropic || platform == PlatformGemini) && !hasForcePlatform
	accounts, err := s.accountRepo.ListByGroup(ctx, group.ID)
	if err != nil {
		return false
	}
	now := time.Now()
	pool := 0
	for i := range accounts {
		acc := &accounts[i]
		if !acc.IsActive() || !acc.Schedulable || !s.isAccountAllowedForPlatform(acc, platform, useMixed) {
			continue
		}
		if acc.AutoPauseOnExpired && acc.ExpiresAt != nil && !now.Before(*acc.ExpiresAt) {
			continue
		}
		if requestedModel != "" && !s.isModelSupportedByAccountWithContext(ctx, acc, requestedModel) {
			continue
		}
		pool++
		if _, ok := tried[acc.ID]; !ok {
			return false
		}
	}
	return pool > 0
}
