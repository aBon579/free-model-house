import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { useProbeAll, useProbes } from '../api/client'

/**
 * 一轮探测的**真实**状态。
 *
 * ⚠️ **为什么需要这个 hook** ——
 *
 * 整轮探测是**异步**的：后端立刻返回 202，探测在后台跑（可能几十秒）。
 *
 * 之前的前端在 `onSuccess` 里就弹「探测完成」——
 * 那时候**一个模型都还没探完**。用户看到"完成"，
 * 但列表里的状态还在变 —— 这是在**骗人**。
 *
 * 现在改成：
 *   ① 点击 → 按钮转圈 + 显示「正在探测 12/27」
 *   ② 真的跑完了（`progress.running` 变 false）→ 弹「探测完成」
 *   ③ 中途失败 → 弹「探测中断」
 */
export interface ProbeRound {
  /** 是否正在探测。 */
  running: boolean
  /** 主体探完了，正在收尾。 */
  finalizing: boolean
  /** 已探完 / 总数。 */
  done: number
  total: number
  /** 触发一轮。 */
  start: () => void
  /** 上一次这轮的结果（用来弹 toast）。 */
  lastResult: 'done' | 'failed' | null
  /**
   * 上一轮的**共性失败原因**。
   *
   * ⚠️ **为什么需要它** ——
   *
   * 「探测完成，但模型状态全是『未探测』」这句话**没有信息量**。
   * 用户不知道是网络断了、证书缺了、还是上游挂了。
   *
   * 绝大多数情况是**所有模型因同一个原因失败** ——
   * 把那个原因提出来，用户才能行动。
   */
  lastFailure: string
  /** 那个原因占了多少个模型。 */
  lastFailureCount: number
  /** 上轮探通了几个。 */
  lastAvailable: number
}

export function useProbeRound(onFinish?: (result: 'done' | 'failed') => void): ProbeRound {
  const probeAll = useProbeAll()
  const { data: probes } = useProbes()
  const progress = probes?.progress
  const qc = useQueryClient()

  // ⚠️ **"这一轮真的结束了"的判定** ——
  //
  //   判据：**已完成的轮次编号变了**。
  //
  //   `progress.round` 是后端在**一轮结束时**才写的，
  //   所以它变了 = 整轮真的完了。
  //
  //   ⚠️ **为什么不是 `done == total`** ——
  //
  //   `done == total` 只说明"每个模型都问过了"，
  //   但**结论还没落定**：
  //     - 限流的模型要等冷却窗口再问一次
  //     - 瞬时故障的模型要立刻再问一次
  //     - 最后才写进面板
  //
  //   那些复核可能改写结论（"限流" → "可用"）。
  //   在复核之前就弹"探测完成"，用户看到的列表**还是旧的** ——
  //   那还是在骗人。
  //
  //   所以：**整轮真的完了才提示**。
  //   等待期间界面显示进度（12/27）和"正在复核"，
  //   用户知道发生了什么。
  //
  //   ⚠️ 为什么不看 `running` 从 true 变 false？
  //   极快的轮次（没有目标模型）可能**整个过程都观测不到** ——
  //   前端根本没看到 `running: true`，于是永远不触发。
  //   轮次号是**后端在结束时写的**，一定可靠。
  const startRound = useRef<number | null>(null)

  const [lastResult, setLastResult] = useState<'done' | 'failed' | null>(null)

  // ⚠️ **回调放进 ref** ——
  //
  //   调用方传的是**内联箭头函数**，每次渲染都是新引用。
  //   直接把它放进 useEffect 的依赖里 → effect 每次渲染都重跑。
  //
  //   那本身不致命，但配合下面的逻辑会有竞态：
  //   渲染 → effect 重跑 → 状态判断可能在中途被重置。
  //   放进 ref 就只在"真的变化"时判断。
  const finishRef = useRef(onFinish)
  finishRef.current = onFinish

  useEffect(() => {
    if (!progress) return

    // ⚠️ **轮次号变了** = 整轮真的结束了。
    const start = startRound.current
    if (start !== null && progress.round !== start) {
      startRound.current = null
      const ok = probeAll.isError
      setLastResult(ok ? 'failed' : 'done')
      // ⚠️ **刷新模型清单** —— 探测改了可用性，
      //   清单上的状态（可用/异常/未探测）也得跟着变。
      qc.invalidateQueries({ queryKey: ['models'] })
      finishRef.current?.(ok ? 'failed' : 'done')
    }
    // ⚠️ **依赖里不能有 `onFinish`** —— 它每次渲染都变（见上面的说明）。
  }, [progress?.round, probeAll.isError])

  return {
    // ⚠️ **本地也在跑**（刚点了按钮，后端还没来得及更新进度）
    //   或**后端在跑** —— 两个都算"正在探测"。
    running: probeAll.isPending || !!progress?.running,
    // ⚠️ **收尾中** —— 主体探完了，在等限流复核。
    finalizing: !!progress?.finalizing,
    done: progress?.done ?? 0,
    total: progress?.total ?? 0,
    start: () => {
      setLastResult(null)
      // ⚠️ **记住起点的轮次号** —— 结束时要跟它比。
      startRound.current = progress?.round ?? null
      probeAll.mutate()
    },
    lastResult,
    // ⚠️ **上轮的共性失败原因** —— 让"全是未探测"能说清为什么。
    lastFailure: progress?.lastFailure ?? '',
    lastFailureCount: progress?.lastFailureCount ?? 0,
    lastAvailable: progress?.lastAvailable ?? 0,
  }
}
