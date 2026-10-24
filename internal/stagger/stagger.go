// Package stagger 同渠道多账号批量操作的账号间随机错峰延迟。
//
// 背景（2026-10-24 事故整改）：keepalive 于 22:00:01 同一秒内连续刷新 5 个账号
// 的 token（串行但零延迟，快上游下就是同一秒）；balance/积分包/任务扫描更是
// goroutine 全并发扇出。同渠道多账号同一秒/同窗口批量打上游是典型机器人指纹，
// 疑似上游批量吊销会话的诱因。全站整改口径：
//
//   - 同渠道多账号的批量操作一律串行执行，绝不并发；
//   - 账号间加「基础延迟 + 随机抖动」，固定间隔本身也是指纹，必须随机；
//   - 两档强度：
//     Heavy 重操作档（签到/保活/连登/渠道签到/成长任务执行等写类动作）；
//     Light 轻查询档（余额/积分包/任务扫描/昵称/券码等只读查询）。
//
// 用法（first 范式，放在所有 continue 门控之后、真正发起上游调用之前）：
//
//	first := true
//	for _, st := range pool.List() {
//	    ... 门控 continue ...
//	    if !first {
//	        time.Sleep(stagger.Heavy())
//	    }
//	    first = false
//	    ... 上游调用 ...
//	}
//
// 测试通过 SetTestFactor(0) 统一置零（各包 TestMain 调用），用例不吃错峰耗时。
package stagger

import (
	"math/rand/v2"
	"time"
)

// 重操作档：写类动作（签到/保活/连登/渠道签到/成长任务）。3s + 0~5s 随机。
var (
	heavyBase   = 3 * time.Second
	heavyJitter = 5 * time.Second
)

// 轻查询档：只读查询（余额/积分包/任务扫描/昵称/券码）。1s + 0~2s 随机。
var (
	lightBase   = 1 * time.Second
	lightJitter = 2 * time.Second
)

// 触发抖动档：整点定时任务到点后的随机延迟（0~5min），抹掉「每天准点 hh:00:00
// 发起批量请求」的机器人指纹。挂在定时触发分支，手动入口不走此档。
var fireJitterMax = 5 * time.Minute

// factor 测试缩放系数：0 = 全部延迟归零（TestMain 设置），1 = 生产口径。
var factor = 1.0

// SetTestFactor 缩放所有错峰延迟（测试专用：0 关闭等待，1 恢复生产口径）。
func SetTestFactor(f float64) { factor = f }

// Heavy 重操作档账号间延迟：base + rand(0..jitter)，每次调用独立随机。
func Heavy() time.Duration { return roll(heavyBase, heavyJitter) }

// Light 轻查询档账号间延迟。
func Light() time.Duration { return roll(lightBase, lightJitter) }

// FireJitter 定时任务触发抖动：返回 0..fireJitterMax 的随机延迟，调用方应在
// 定时槽到点后先睡此时长再执行批量动作。factor<=0 时返回 0（测试不吃抖动）。
func FireJitter() time.Duration {
	if factor <= 0 {
		return 0
	}
	return time.Duration(float64(fireJitterMax) * factor * rand.Float64())
}

func roll(base, jitter time.Duration) time.Duration {
	if factor <= 0 {
		return 0
	}
	d := base + time.Duration(rand.Float64()*float64(jitter))
	return time.Duration(float64(d) * factor)
}
