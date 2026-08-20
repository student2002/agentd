// clock.go 提供可测试的时间抽象层，解耦业务逻辑与系统时间的直接依赖。
//
// 本文件包含：
//   - Clock 接口：抽象 time.Now() 调用，提升代码可测试性
//   - RealClock：生产环境实现，返回真实系统时间
//   - FakeClock：测试实现，支持手动推进时间，便于模拟超时和定时场景
//
// 使用场景：
//   - 业务代码中通过 Clock 接口获取时间，而非直接调用 time.Now()
//   - 单元测试中注入 FakeClock，精确控制时间流逝，验证超时逻辑
//   - 定时任务调度器中使用 Clock 接口，支持测试环境下的快速验证
package clock

import "time"

// Clock 接口抽象了 time.Now() 调用，用于提升代码可测试性。
// 实现者提供时间获取能力，业务代码通过接口调用而非直接依赖 time 包。
//
// 生产环境: 使用 RealClock 返回系统真实时间。
// 测试环境: 使用 FakeClock 返回可手动控制的固定时间，便于模拟时间流逝场景。
type Clock interface {
	// Now 返回当前时间。
	Now() time.Time
}

// RealClock 是 Clock 接口的生产环境实现，返回真实的系统时间。
type RealClock struct{}

// Now 返回当前系统时间。
func (RealClock) Now() time.Time { return time.Now() }

// FakeClock 是 Clock 接口的测试实现，返回可手动推进的固定时间。
// 用于单元测试中模拟时间相关行为，如超时、定时任务触发等场景。
type FakeClock struct {
	fixed time.Time
}

// NewFakeClock 创建一个设置为指定时间的 FakeClock 实例。
//
// 参数：
//   - t: 假时钟的初始时间
//
// 返回：
//   - *FakeClock: 初始化后的假时钟实例
func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{fixed: t}
}

// Now 返回假时钟当前设定的时间，不会随系统时钟变化。
func (f *FakeClock) Now() time.Time { return f.fixed }

// Advance 将假时钟按指定时间间隔向前推进，模拟时间流逝。
//
// 参数：
//   - d: 要推进的时间间隔，支持负值（回退时间）
func (f *FakeClock) Advance(d time.Duration) {
	f.fixed = f.fixed.Add(d)
}
