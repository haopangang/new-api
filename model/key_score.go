package model

import (
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// KeyScoreState 是纯内存的 key 评分状态，服务重启后重新评分
type KeyScoreState struct {
	Score          int64 // 当前评分 (0-100)
	AvgRespTime    int64 // 平均响应时间 (ms)，EMA
	TotalRequests  int64 // 总请求次数
	SuccessCount   int64 // 成功次数
	LastUsedTime   int64 // 上次使用时间 (Unix秒)，用于保证所有 key 都有机会
	LastStatusCode int   // 最近一次请求的 HTTP 状态码（0 表示无记录）
}

// GoodKeyScoreThreshold 评分 >= 此值的 key 视为"及格"（初始分 50，只有保持或恢复到初始水平才算及格）
const GoodKeyScoreThreshold int64 = 50

var (
	keyScoreStore   = make(map[int]map[string]*KeyScoreState) // channelId -> apiKey -> state
	keyScoreStoreMu sync.RWMutex
)

// getOrCreateKeyState 获取或创建 key 的评分状态（纯内存）
func getOrCreateKeyState(channelId int, key string) *KeyScoreState {
	// 读锁快速路径
	keyScoreStoreMu.RLock()
	if chMap, ok := keyScoreStore[channelId]; ok {
		if state, ok := chMap[key]; ok {
			keyScoreStoreMu.RUnlock()
			return state
		}
	}
	keyScoreStoreMu.RUnlock()

	// 写锁慢路径
	keyScoreStoreMu.Lock()
	defer keyScoreStoreMu.Unlock()
	if chMap, ok := keyScoreStore[channelId]; ok {
		if state, ok := chMap[key]; ok {
			return state
		}
	} else {
		keyScoreStore[channelId] = make(map[string]*KeyScoreState)
	}
	state := &KeyScoreState{Score: common.KeyScoreDefault}
	keyScoreStore[channelId][key] = state
	return state
}

// GetKeyScore 获取 key 的当前评分（内存），未初始化时返回默认值
func (channel *Channel) GetKeyScore(key string) int64 {
	keyScoreStoreMu.RLock()
	defer keyScoreStoreMu.RUnlock()
	if chMap, ok := keyScoreStore[channel.Id]; ok {
		if state, ok := chMap[key]; ok {
			return state.Score
		}
	}
	return common.KeyScoreDefault
}

// DebugKeyScoreStore 打印当前 key 评分存储状态（调试用）
func DebugKeyScoreStore(channelId int) {
	keyScoreStoreMu.RLock()
	defer keyScoreStoreMu.RUnlock()
	chMap, ok := keyScoreStore[channelId]
	if !ok {
		common.SysLog(fmt.Sprintf("[KeyScore] channel=%d: no entry in store", channelId))
		return
	}
	common.SysLog(fmt.Sprintf("[KeyScore] channel=%d: %d keys in store", channelId, len(chMap)))
	for key, state := range chMap {
		preview := key
		if len(preview) > 10 {
			preview = preview[:6] + "..." + preview[len(preview)-4:]
		}
		common.SysLog(fmt.Sprintf("[KeyScore] channel=%d key=%s: score=%d total=%d success=%d lastStatus=%d",
			channelId, preview, state.Score, state.TotalRequests, state.SuccessCount, state.LastStatusCode))
	}
}

// GetKeyAvgResponseTime 获取 key 的平均响应时间（内存）
func (channel *Channel) GetKeyAvgResponseTime(key string) int64 {
	keyScoreStoreMu.RLock()
	defer keyScoreStoreMu.RUnlock()
	if chMap, ok := keyScoreStore[channel.Id]; ok {
		if state, ok := chMap[key]; ok {
			return state.AvgRespTime
		}
	}
	return 0
}

// GetKeyTotalRequests 获取 key 的总请求次数（内存）
func (channel *Channel) GetKeyTotalRequests(key string) int64 {
	keyScoreStoreMu.RLock()
	defer keyScoreStoreMu.RUnlock()
	if chMap, ok := keyScoreStore[channel.Id]; ok {
		if state, ok := chMap[key]; ok {
			return state.TotalRequests
		}
	}
	return 0
}

// GetKeySuccessCount 获取 key 的成功次数（内存）
func (channel *Channel) GetKeySuccessCount(key string) int64 {
	keyScoreStoreMu.RLock()
	defer keyScoreStoreMu.RUnlock()
	if chMap, ok := keyScoreStore[channel.Id]; ok {
		if state, ok := chMap[key]; ok {
			return state.SuccessCount
		}
	}
	return 0
}

// GetKeyLastStatusCode 获取 key 的最近一次请求状态码（内存）
func (channel *Channel) GetKeyLastStatusCode(key string) int {
	keyScoreStoreMu.RLock()
	defer keyScoreStoreMu.RUnlock()
	if chMap, ok := keyScoreStore[channel.Id]; ok {
		if state, ok := chMap[key]; ok {
			return state.LastStatusCode
		}
	}
	return 0
}

// InitKeyScore 初始化 key 评分（首次使用时，确保 store 中有条目）
func (channel *Channel) InitKeyScore(key string) {
	_ = getOrCreateKeyState(channel.Id, key)
}

// PurgeKeyScore 删除指定 key 的评分数据（key 被删除时调用）
func (channel *Channel) PurgeKeyScore(key string) {
	keyScoreStoreMu.Lock()
	defer keyScoreStoreMu.Unlock()
	if chMap, ok := keyScoreStore[channel.Id]; ok {
		delete(chMap, key)
	}
}

// countGoodKeys 统计渠道中启用的及格 key 的数量（评分 >= GoodKeyScoreThreshold，仅统计启用的 key）
func (channel *Channel) countGoodKeys() int {
	keys := channel.GetKeys()
	keyScoreStoreMu.RLock()
	defer keyScoreStoreMu.RUnlock()
	chMap, ok := keyScoreStore[channel.Id]
	if !ok {
		return 0
	}
	count := 0
	for i, key := range keys {
		// 只统计启用的 key
		status := 1
		if channel.ChannelInfo.MultiKeyStatusList != nil {
			if s, exists := channel.ChannelInfo.MultiKeyStatusList[i]; exists {
				status = s
			}
		}
		if status != 1 {
			continue
		}
		if state, ok := chMap[key]; ok && state.Score >= GoodKeyScoreThreshold {
			count++
		}
	}
	return count
}

// calcScoreMultiplier 根据及格 key 数量动态计算奖惩乘数
//
// 核心逻辑：key 多 → 罚重恢复慢（可以精挑细选），key 少 → 罚轻恢复快（必须保活）
//
// 映射关系：
//
//	及格 key 数    乘数    效果
//	1             0.5     极度宽容：几乎不罚，快速恢复
//	3             0.6     宽容：罚轻恢复快
//	7             0.8     偏宽容
//	15            1.0     中性：标准奖惩
//	30            1.5     偏严格
//	50+           2.0     严格：重罚慢恢复，精挑细选
//
// 安全底线：无论如何，乘数不会低于 0.5，保证系统不会因评分机制而瘫痪
func calcScoreMultiplier(goodKeyCount int) float64 {
	if goodKeyCount <= 1 {
		return 0.5 // 只剩 1 个好 key，极度宽容
	}
	if goodKeyCount >= 50 {
		return 2.0 // 50+ 好 key，严格筛选
	}
	// 线性插值：1→0.5, 50→2.0
	// multiplier = 0.5 + (goodKeyCount - 1) / 49 * 1.5
	return 0.5 + float64(goodKeyCount-1)/49.0*1.5
}

// calcDynamicBoost 根据乘数计算成功加分
func calcDynamicBoost(multiplier float64) int64 {
	boost := int64(float64(common.KeyScoreSuccessBoost) * multiplier)
	if boost < 1 {
		boost = 1 // 最少加 1 分
	}
	return boost
}

// calcDynamicPenalty 根据乘数计算扣分
func calcDynamicPenalty(basePenalty int64, multiplier float64) int64 {
	penalty := int64(float64(basePenalty) * multiplier)
	if penalty < 1 {
		penalty = 1 // 最少扣 1 分
	}
	return penalty
}

// CalcDynamicSlowThreshold 根据输入 token 数量计算动态慢响应阈值
// 大上下文自然需要更多时间，阈值应相应放宽
// 基础 10s + 每 10K tokens 加 5s，上限 120s
func CalcDynamicSlowThreshold(estimatedTokens int) int64 {
	base := common.SlowResponseThresholdMs // 10000ms = 10s
	extra := int64(estimatedTokens/10000) * 5000
	threshold := base + extra
	maxThreshold := int64(120000) // 120s 上限
	if threshold > maxThreshold {
		threshold = maxThreshold
	}
	return threshold
}

// UpdateKeyScoreSuccess 成功请求时更新评分
// 记录响应时间，根据速度加分，慢响应触发扣分和冷却
// estimatedTokens 用于计算动态慢响应阈值
func (channel *Channel) UpdateKeyScoreSuccess(key string, responseTimeMs int64, estimatedTokens int) {
	if !channel.ChannelInfo.IsMultiKey {
		return
	}

	state := getOrCreateKeyState(channel.Id, key)

	// 记录响应时间 (EMA)
	state.AvgRespTime = calcEMA(state.AvgRespTime, responseTimeMs)

	// 总请求次数+1
	state.TotalRequests++
	state.LastUsedTime = time.Now().Unix()
	state.LastStatusCode = 200

	// 动态慢响应阈值
	slowThreshold := CalcDynamicSlowThreshold(estimatedTokens)

	// 慢响应检查
	if responseTimeMs >= slowThreshold {
		channel.applySlowResponsePenalty(key, responseTimeMs, slowThreshold, state)
		return
	}

	// 正常速度响应：加分（根据及格 key 数量动态调整）
	state.SuccessCount++
	goodKeys := channel.countGoodKeys()
	multiplier := calcScoreMultiplier(goodKeys)
	boost := calcDynamicBoost(multiplier)
	state.Score += boost
	if state.Score > common.KeyScoreMax {
		state.Score = common.KeyScoreMax
	}
}

// applySlowResponsePenalty 慢响应惩罚：扣分（根据及格 key 数量动态调整）
func (channel *Channel) applySlowResponsePenalty(key string, responseTimeMs int64, slowThreshold int64, state *KeyScoreState) {
	goodKeys := channel.countGoodKeys()
	multiplier := calcScoreMultiplier(goodKeys)
	penalty := calcDynamicPenalty(common.KeyScoreSlowPenalty, multiplier)
	state.Score -= penalty
	if state.Score < common.KeyScoreMin {
		state.Score = common.KeyScoreMin
	}
}

// UpdateKeyScoreOn429 429 错误时扣分（根据及格 key 数量动态调整）
func (channel *Channel) UpdateKeyScoreOn429(key string) {
	if !channel.ChannelInfo.IsMultiKey {
		return
	}

	state := getOrCreateKeyState(channel.Id, key)
	state.TotalRequests++
	state.LastUsedTime = time.Now().Unix()
	state.LastStatusCode = 429

	goodKeys := channel.countGoodKeys()
	multiplier := calcScoreMultiplier(goodKeys)
	penalty := calcDynamicPenalty(common.KeyScore429Penalty, multiplier)
	oldScore := state.Score
	state.Score -= penalty
	if state.Score < common.KeyScoreMin {
		state.Score = common.KeyScoreMin
	}
	preview := key
	if len(preview) > 10 {
		preview = preview[:6] + "..." + preview[len(preview)-4:]
	}
	common.SysLog(fmt.Sprintf("[KeyScore] channel=%d key=%s score: %d -> %d (penalty=%d, goodKeys=%d, multiplier=%.2f)",
		channel.Id, preview, oldScore, state.Score, penalty, goodKeys, multiplier))
}

// UpdateKeyScoreOnError 服务端错误时扣分（500/502/503/504，根据及格 key 数量动态调整）
func (channel *Channel) UpdateKeyScoreOnError(key string, statusCode int) {
	if !channel.ChannelInfo.IsMultiKey {
		return
	}

	state := getOrCreateKeyState(channel.Id, key)
	state.TotalRequests++
	state.LastUsedTime = time.Now().Unix()
	state.LastStatusCode = statusCode

	goodKeys := channel.countGoodKeys()
	multiplier := calcScoreMultiplier(goodKeys)
	penalty := calcDynamicPenalty(common.KeyScoreErrorPenalty, multiplier)
	oldScore := state.Score
	state.Score -= penalty
	if state.Score < common.KeyScoreMin {
		state.Score = common.KeyScoreMin
	}
	preview := key
	if len(preview) > 10 {
		preview = preview[:6] + "..." + preview[len(preview)-4:]
	}
	common.SysLog(fmt.Sprintf("[KeyScore] channel=%d key=%s score: %d -> %d (error %d, penalty=%d, goodKeys=%d, multiplier=%.2f)",
		channel.Id, preview, oldScore, state.Score, statusCode, penalty, goodKeys, multiplier))
}

// SelectKeyByScore 基于评分的加权随机选择
// 保证所有 key 都有机会被选中：评分高的优先，但低分 key 也有最低保障
// 通过 LastUsedTime 追加"久未使用"加权，确保不会有任何 key 被饿死
// 参数: keyToIndex = key string -> index 映射
// 返回: 被选中 key 的 index
func (channel *Channel) SelectKeyByScore(keyToIndex map[string]int) int {
	if len(keyToIndex) == 0 {
		return 0
	}

	now := time.Now().Unix()
	var totalWeight int64
	keys := make([]string, 0, len(keyToIndex))
	weights := make([]int64, 0, len(keyToIndex))

	for key := range keyToIndex {
		state := getOrCreateKeyState(channel.Id, key)
		score := state.Score
		if score < common.KeyScoreMin {
			score = common.KeyScoreMin
		}

		// 基础权重：评分 + 最低保障（确保低分 key 也有 10 的权重）
		weight := score + 10

		// 久未使用加权：超过 30 秒未使用，每 30 秒加 5 权重
		// 这保证了即使所有 key 评分都很低，每个 key 也会被轮流使用
		if state.LastUsedTime > 0 {
			idleSec := now - state.LastUsedTime
			if idleSec > 30 {
				weight += (idleSec / 30) * 5
			}
		} else {
			// 从未使用的 key，给一个初始加权
			weight += 20
		}

		keys = append(keys, key)
		weights = append(weights, weight)
		totalWeight += weight
	}

	if len(keys) == 1 {
		return keyToIndex[keys[0]]
	}

	if totalWeight == 0 {
		return keyToIndex[keys[now%int64(len(keys))]]
	}

	// 加权随机
	r := now % totalWeight
	for i, w := range weights {
		r -= w
		if r < 0 {
			return keyToIndex[keys[i]]
		}
	}
	return keyToIndex[keys[len(keys)-1]]
}

// calcEMA 计算指数移动平均
func calcEMA(current, newValue int64) int64 {
	if current == 0 {
		return newValue
	}
	return (current*7 + newValue*3) / 10
}
