package model

import (
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

// channelCacheSnapshot is an immutable snapshot of the channel cache.
// All map/slice fields are read-only after creation, so concurrent access
// is safe without locks. Channel pointer fields may be mutated by admin
// operations (CacheUpdateChannel/CacheUpdateChannelStatus) but those are
// infrequent and protected by channelCacheMu.
type channelCacheSnapshot struct {
	// group -> model -> channel IDs sorted by priority (descending)
	group2model2channels map[string]map[string][]int
	// channel ID -> Channel (all channels, including disabled)
	channelsIDM map[int]*Channel
	// channel ID -> parsed Advanced Custom config
	advancedCustomConfig map[int]*dto.AdvancedCustomConfig
	// channel ID -> priority groups (pre-sorted, highest first).
	// Each entry is a slice of priority tiers, where each tier contains
	// channel IDs at that priority level. Built during InitChannelCache
	// to eliminate per-request sorting and map allocation in the hot path.
	priorityGroups map[int][][]int
}

var (
	// channelCachePtr holds the current immutable snapshot.
	// Readers load this atomically; writers swap it under channelCacheMu.
	channelCachePtr atomic.Pointer[channelCacheSnapshot]

	// channelCacheMu protects snapshot rebuilds in CacheUpdateChannel and
	// CacheUpdateChannelStatus. InitChannelCache does NOT hold this lock
	// during the expensive DB+build phase — it only holds it briefly for
	// the final pointer swap.
	channelCacheMu sync.RWMutex
)

// loadSnapshot returns the current cache snapshot. The read lock is held
// only for the atomic pointer load (nanoseconds), then released immediately.
// All subsequent reads work on the immutable snapshot without any lock.
func loadSnapshot() *channelCacheSnapshot {
	channelCacheMu.RLock()
	s := channelCachePtr.Load()
	channelCacheMu.RUnlock()
	return s
}

// buildPriorityGroups pre-computes priority tiers for each (group, model) pair.
// The result maps a channel ID to a [][]int where index i contains all channel
// IDs at the i-th highest priority. This eliminates per-request sorting.
func buildPriorityGroups(g2m2c map[string]map[string][]int, idm map[int]*Channel) map[int][][]int {
	// Collect unique (group, model) pairs and build a lookup key.
	// We use a single flat map keyed by a hash of (group, model).
	type groupModel struct {
		group string
		model string
	}
	seen := make(map[groupModel][][]int)

	for group, m2c := range g2m2c {
		for model, chIDs := range m2c {
			gm := groupModel{group, model}
			if _, ok := seen[gm]; ok {
				continue
			}
			seen[gm] = buildPriorityTiers(chIDs, idm)
		}
	}

	// Map each channel ID to its priority tiers. All channels in the same
	// (group, model) pair share the same tiers.
	result := make(map[int][][]int, len(idm))
	for _, tiers := range seen {
		for _, tier := range tiers {
			for _, id := range tier {
				if _, ok := result[id]; !ok {
					result[id] = tiers
				}
			}
		}
	}
	return result
}

// buildPriorityTiers groups channel IDs by priority (descending).
// Returns a slice of tiers where each tier contains channel IDs at the same priority.
func buildPriorityTiers(chIDs []int, idm map[int]*Channel) [][]int {
	if len(chIDs) == 0 {
		return nil
	}

	// Collect unique priorities
	prioritySet := make(map[int64]bool, 4)
	for _, id := range chIDs {
		if ch, ok := idm[id]; ok {
			prioritySet[ch.GetPriority()] = true
		}
	}

	// Sort priorities descending
	priorities := make([]int, 0, len(prioritySet))
	for p := range prioritySet {
		priorities = append(priorities, int(p))
	}
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))

	// Build tiers
	tiers := make([][]int, len(priorities))
	for i, priority := range priorities {
		tier := make([]int, 0, len(chIDs)/len(priorities)+1)
		for _, id := range chIDs {
			if ch, ok := idm[id]; ok && int(ch.GetPriority()) == priority {
				tier = append(tier, id)
			}
		}
		tiers[i] = tier
	}
	return tiers
}

// rebuildGroupModelChannelsForReenable 将重新启用的渠道加回 group2model2channels。
// 从 Ability 表查询该渠道支持的 (group, model) 对，保持其他渠道的映射不变。
func rebuildGroupModelChannelsForReenable(snapG2M2C map[string]map[string][]int, idm map[int]*Channel, channelId int) map[string]map[string][]int {
	ch, ok := idm[channelId]
	if !ok {
		return snapG2M2C
	}
	if ch.Status != common.ChannelStatusEnabled {
		return snapG2M2C
	}

	// 查询该渠道的 ability 记录
	var abilities []*Ability
	DB.Where("channel_id = ?", channelId).Find(&abilities)
	if len(abilities) == 0 {
		return snapG2M2C
	}

	// 深拷贝 group2model2channels
	newG2M2C := make(map[string]map[string][]int, len(snapG2M2C))
	for group, m2c := range snapG2M2C {
		newM2C := make(map[string][]int, len(m2c))
		for model, chIDs := range m2c {
			newSlice := make([]int, len(chIDs))
			copy(newSlice, chIDs)
			newM2C[model] = newSlice
		}
		newG2M2C[group] = newM2C
	}

	// 将渠道加回对应的 (group, model) 条目
	for _, ability := range abilities {
		if newG2M2C[ability.Group] == nil {
			newG2M2C[ability.Group] = make(map[string][]int)
		}
		chIDs := newG2M2C[ability.Group][ability.Model]
		// 避免重复添加
		found := false
		for _, id := range chIDs {
			if id == channelId {
				found = true
				break
			}
		}
		if !found {
			newG2M2C[ability.Group][ability.Model] = append(chIDs, channelId)
		}
	}

	// 按优先级排序（与 InitChannelCache 保持一致）
	for _, m2c := range newG2M2C {
		for model, chIDs := range m2c {
			sort.Slice(chIDs, func(i, j int) bool {
				ci, ok1 := idm[chIDs[i]]
				cj, ok2 := idm[chIDs[j]]
				if !ok1 || !ok2 {
					return false
				}
				return ci.GetPriority() > cj.GetPriority()
			})
			m2c[model] = chIDs
		}
	}

	return newG2M2C
}

func InitChannelCache() {
	if !common.MemoryCacheEnabled {
		return
	}

	// Load current snapshot for preserving polling index state.
	// This is safe without the write lock because we only read the old
	// snapshot's channelsIDM to copy polling indexes.
	oldSnapshot := channelCachePtr.Load()

	newChannelId2channel := make(map[int]*Channel)
	newChannel2advancedCustomConfig := make(map[int]*dto.AdvancedCustomConfig)
	var channels []*Channel
	DB.Find(&channels)
	for _, channel := range channels {
		newChannelId2channel[channel.Id] = channel
		if channel.Type == constant.ChannelTypeAdvancedCustom {
			if config := channel.GetOtherSettings().AdvancedCustom; config != nil {
				newChannel2advancedCustomConfig[channel.Id] = config
			}
		}
	}

	// Preserve multi-key polling index from old snapshot
	for id, channel := range newChannelId2channel {
		if channel.ChannelInfo.IsMultiKey {
			channel.Keys = channel.GetKeys()
			if channel.ChannelInfo.MultiKeyMode == constant.MultiKeyModePolling {
				if oldSnapshot != nil {
					if oldChannel, ok := oldSnapshot.channelsIDM[id]; ok {
						if oldChannel.ChannelInfo.IsMultiKey && oldChannel.ChannelInfo.MultiKeyMode == constant.MultiKeyModePolling {
							channel.ChannelInfo.MultiKeyPollingIndex = oldChannel.ChannelInfo.MultiKeyPollingIndex
						}
					}
				}
			}
		}
	}

	var abilities []*Ability
	DB.Find(&abilities)
	groups := make(map[string]bool)
	for _, ability := range abilities {
		groups[ability.Group] = true
	}
	newGroup2model2channels := make(map[string]map[string][]int)
	for group := range groups {
		newGroup2model2channels[group] = make(map[string][]int)
	}
	for _, channel := range channels {
		if channel.Status != common.ChannelStatusEnabled {
			continue // skip disabled channels
		}
		groups := strings.Split(channel.Group, ",")
		for _, group := range groups {
			models := strings.Split(channel.Models, ",")
			for _, model := range models {
				if _, ok := newGroup2model2channels[group][model]; !ok {
					newGroup2model2channels[group][model] = make([]int, 0)
				}
				newGroup2model2channels[group][model] = append(newGroup2model2channels[group][model], channel.Id)
			}
		}
	}

	// sort by priority (descending)
	for group, model2channels := range newGroup2model2channels {
		for model, channels := range model2channels {
			sort.Slice(channels, func(i, j int) bool {
				return newChannelId2channel[channels[i]].GetPriority() > newChannelId2channel[channels[j]].GetPriority()
			})
			newGroup2model2channels[group][model] = channels
		}
	}

	// Pre-compute priority groups for the hot path
	priorityGroups := buildPriorityGroups(newGroup2model2channels, newChannelId2channel)

	// Build the new immutable snapshot
	newSnapshot := &channelCacheSnapshot{
		group2model2channels: newGroup2model2channels,
		channelsIDM:          newChannelId2channel,
		advancedCustomConfig: newChannel2advancedCustomConfig,
		priorityGroups:       priorityGroups,
	}

	// Atomic swap — readers see the new snapshot immediately.
	// Hold the write lock briefly to serialize with CacheUpdateChannel.
	channelCacheMu.Lock()
	channelCachePtr.Store(newSnapshot)
	channelCacheMu.Unlock()

	common.SysLog("channels synced from database")
}

func SyncChannelCache(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		common.SysLog("syncing channels from database")
		InitChannelCache()
	}
}

// GetRandomSatisfiedChannel selects a weighted-random channel for the given
// group, model, and retry count. The hot path uses pre-computed priority tiers
// to avoid per-request sorting and map allocation.
func GetRandomSatisfiedChannel(group string, model string, retry int, requestPath string) (*Channel, error) {
	// if memory cache is disabled, get channel directly from database
	if !common.MemoryCacheEnabled {
		return GetChannel(group, model, retry, requestPath)
	}

	snap := loadSnapshot()
	if snap == nil {
		return nil, nil
	}

	// First, try to find channels with the exact model name.
	channels := filterChannelsByRequestPath(snap, snap.group2model2channels[group][model], requestPath)

	// If no channels found, try to find channels with the normalized model name.
	if len(channels) == 0 {
		normalizedModel := ratio_setting.FormatMatchingModelName(model)
		channels = filterChannelsByRequestPath(snap, snap.group2model2channels[group][normalizedModel], requestPath)
	}

	if len(channels) == 0 {
		return nil, nil
	}

	if len(channels) == 1 {
		if channel, ok := snap.channelsIDM[channels[0]]; ok {
			return channel, nil
		}
		return nil, fmt.Errorf("数据库一致性错误，渠道# %d 不存在，请联系管理员修复", channels[0])
	}

	// Use pre-computed priority tiers from the snapshot.
	// Look up the first channel's tiers (all channels in the same (group, model)
	// share the same priority tiers).
	tiers := snap.priorityGroups[channels[0]]
	if len(tiers) == 0 {
		// Fallback: build tiers on the fly (should not happen after InitChannelCache)
		return getSatisfiedChannelFallback(snap, channels, retry, group, model)
	}

	if retry >= len(tiers) {
		retry = len(tiers) - 1
	}
	targetTier := tiers[retry]

	// Collect channels in the target priority tier
	var sumWeight int
	targetChannels := make([]*Channel, 0, len(targetTier))
	for _, channelId := range targetTier {
		if channel, ok := snap.channelsIDM[channelId]; ok {
			sumWeight += channel.GetWeight()
			targetChannels = append(targetChannels, channel)
		}
	}

	if len(targetChannels) == 0 {
		return nil, fmt.Errorf("no channel found, group: %s, model: %s, retry: %d", group, model, retry)
	}

	return selectByWeight(targetChannels, sumWeight)
}

// getSatisfiedChannelFallback handles the case where priority tiers are not
// pre-computed. This is a safety net and should rarely be reached.
func getSatisfiedChannelFallback(snap *channelCacheSnapshot, channels []int, retry int, group, model string) (*Channel, error) {
	uniquePriorities := make(map[int]bool)
	for _, channelId := range channels {
		if channel, ok := snap.channelsIDM[channelId]; ok {
			uniquePriorities[int(channel.GetPriority())] = true
		} else {
			return nil, fmt.Errorf("数据库一致性错误，渠道# %d 不存在，请联系管理员修复", channelId)
		}
	}
	var sortedUniquePriorities []int
	for priority := range uniquePriorities {
		sortedUniquePriorities = append(sortedUniquePriorities, priority)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(sortedUniquePriorities)))

	if retry >= len(uniquePriorities) {
		retry = len(uniquePriorities) - 1
	}
	targetPriority := int64(sortedUniquePriorities[retry])

	var sumWeight int
	var targetChannels []*Channel
	for _, channelId := range channels {
		if channel, ok := snap.channelsIDM[channelId]; ok {
			if channel.GetPriority() == targetPriority {
				sumWeight += channel.GetWeight()
				targetChannels = append(targetChannels, channel)
			}
		} else {
			return nil, fmt.Errorf("数据库一致性错误，渠道# %d 不存在，请联系管理员修复", channelId)
		}
	}

	if len(targetChannels) == 0 {
		return nil, fmt.Errorf("no channel found, group: %s, model: %s, priority: %d", group, model, targetPriority)
	}

	return selectByWeight(targetChannels, sumWeight)
}

// selectByWeight performs weighted random selection among channels.
func selectByWeight(targetChannels []*Channel, sumWeight int) (*Channel, error) {
	smoothingFactor := 1
	smoothingAdjustment := 0

	if sumWeight == 0 {
		sumWeight = len(targetChannels) * 100
		smoothingAdjustment = 100
	} else if sumWeight/len(targetChannels) < 10 {
		smoothingFactor = 100
	}

	totalWeight := sumWeight * smoothingFactor
	randomWeight := rand.Intn(totalWeight)

	for _, channel := range targetChannels {
		randomWeight -= channel.GetWeight()*smoothingFactor + smoothingAdjustment
		if randomWeight < 0 {
			return channel, nil
		}
	}
	return nil, errors.New("channel not found")
}

// filterChannelsByRequestPath restricts candidates by request path. Only Advanced
// Custom (type 58) channels are path-checked: they are kept only when one of their
// configured routes matches requestPath. All other channel types always pass.
// When requestPath is empty (non-relay callers) filtering is skipped.
func filterChannelsByRequestPath(snap *channelCacheSnapshot, channels []int, requestPath string) []int {
	if requestPath == "" || len(channels) == 0 {
		return channels
	}
	filtered := make([]int, 0, len(channels))
	for _, channelId := range channels {
		channel, ok := snap.channelsIDM[channelId]
		if !ok {
			// keep it so the downstream consistency error is raised as before
			filtered = append(filtered, channelId)
			continue
		}
		if channel.Type != constant.ChannelTypeAdvancedCustom {
			filtered = append(filtered, channelId)
			continue
		}
		if config := snap.advancedCustomConfig[channelId]; config != nil && config.SupportsPath(requestPath) {
			filtered = append(filtered, channelId)
		}
	}
	return filtered
}

func CacheGetChannel(id int) (*Channel, error) {
	if !common.MemoryCacheEnabled {
		return GetChannelById(id, true)
	}
	snap := loadSnapshot()
	if snap == nil {
		return nil, fmt.Errorf("渠道# %d，已不存在", id)
	}
	c, ok := snap.channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("渠道# %d，已不存在", id)
	}
	return c, nil
}

func CacheGetChannelInfo(id int) (*ChannelInfo, error) {
	if !common.MemoryCacheEnabled {
		channel, err := GetChannelById(id, true)
		if err != nil {
			return nil, err
		}
		return &channel.ChannelInfo, nil
	}
	snap := loadSnapshot()
	if snap == nil {
		return nil, fmt.Errorf("渠道# %d，已不存在", id)
	}
	c, ok := snap.channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("渠道# %d，已不存在", id)
	}
	return &c.ChannelInfo, nil
}

func CacheUpdateChannelStatus(id int, status int) {
	if !common.MemoryCacheEnabled {
		return
	}
	channelCacheMu.Lock()
	defer channelCacheMu.Unlock()

	snap := channelCachePtr.Load()
	if snap == nil {
		return
	}
	ch, ok := snap.channelsIDM[id]
	if !ok {
		return
	}

	// 创建 Channel 副本以避免修改旧快照中共享的指针
	chCopy := *ch
	chCopy.Status = status

	if status != common.ChannelStatusEnabled {
		// 禁用：从 group2model2channels 中移除
		newG2M2C := make(map[string]map[string][]int, len(snap.group2model2channels))
		for group, m2c := range snap.group2model2channels {
			newM2C := make(map[string][]int, len(m2c))
			for model, channels := range m2c {
				newSlice := make([]int, 0, len(channels))
				for _, cid := range channels {
					if cid != id {
						newSlice = append(newSlice, cid)
					}
				}
				newM2C[model] = newSlice
			}
			newG2M2C[group] = newM2C
		}

		newIDM := maps.Clone(snap.channelsIDM)
		newIDM[id] = &chCopy

		newSnap := &channelCacheSnapshot{
			group2model2channels: newG2M2C,
			channelsIDM:          newIDM,
			advancedCustomConfig: snap.advancedCustomConfig,
			priorityGroups:       buildPriorityGroups(newG2M2C, newIDM),
		}
		channelCachePtr.Store(newSnap)
	} else {
		// 启用：重新构建完整快照以将渠道加回 group2model2channels
		newIDM := maps.Clone(snap.channelsIDM)
		newIDM[id] = &chCopy

		newG2M2C := rebuildGroupModelChannelsForReenable(snap.group2model2channels, newIDM, id)
		newSnap := &channelCacheSnapshot{
			group2model2channels: newG2M2C,
			channelsIDM:          newIDM,
			advancedCustomConfig: snap.advancedCustomConfig,
			priorityGroups:       buildPriorityGroups(newG2M2C, newIDM),
		}
		channelCachePtr.Store(newSnap)
	}
}

func CacheUpdateChannel(channel *Channel) {
	if !common.MemoryCacheEnabled || channel == nil {
		return
	}
	channelCacheMu.Lock()
	defer channelCacheMu.Unlock()

	snap := channelCachePtr.Load()
	if snap == nil {
		return
	}
	if oldChannel, ok := snap.channelsIDM[channel.Id]; ok {
		logger.LogDebug(nil, "CacheUpdateChannel before: id=%d, name=%s, status=%d, polling_index=%d", channel.Id, channel.Name, channel.Status, oldChannel.ChannelInfo.MultiKeyPollingIndex)
	}

	// Clone channelsIDM and replace the channel
	newIDM := maps.Clone(snap.channelsIDM)
	newIDM[channel.Id] = channel

	newSnap := &channelCacheSnapshot{
		group2model2channels: snap.group2model2channels,
		channelsIDM:          newIDM,
		advancedCustomConfig: snap.advancedCustomConfig,
		priorityGroups:       snap.priorityGroups,
	}
	channelCachePtr.Store(newSnap)

	logger.LogDebug(nil, "CacheUpdateChannel after: id=%d, name=%s, status=%d, polling_index=%d", channel.Id, channel.Name, channel.Status, channel.ChannelInfo.MultiKeyPollingIndex)
}
