package server

import "fmt"

// 调用 Key 的业务错误。
//
// ⚠️ 错误文案直接给用户看 —— 所以在这里定义，
//   不在存储层（存储层不该懂"什么算超长"）。

// errLabelTooLong 标签超过 6 字符。
func errLabelTooLong(got int) error {
	return fmt.Errorf("标签最多 %d 个字（现在 %d 个）—— 名字太长认不出是谁就失去意义了", MaxKeyLabelChars, got)
}

// errKeyNotFound 找不到那把 Key。
var errKeyNotFound error = fmt.Errorf("没有这把 Key —— 它可能已经被删除了")

// errDisableLast 不能停用/删除最后一把启用的 Key。
//
// ⚠️ 这是**自我保护** —— 停掉最后一把 = 所有 Agent 一起失效，
//
//	你自己也进不了管理台之外的一切。真要彻底关门，用防火墙。
var errDisableLast error = fmt.Errorf("不能停用最后一把启用的 Key —— 那样所有设备都调不了了；先建一把新的再停这把")

// errTierLocked 网关级限制生效时，单 Key 的档位被锁定。
//
// ⚠️ 报错要说清**怎么解锁** —— 用户看到"改不了"的第一反应
//
//	是"界面坏了"，告诉他去关网关级限制才知道下一步。
var errTierLocked error = fmt.Errorf("网关级限制生效中 —— 单 Key 档位被强制跟随，先到上面的「网关级限制」切回 off")
