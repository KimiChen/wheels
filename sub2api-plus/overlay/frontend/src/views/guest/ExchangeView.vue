<template>
  <GuestPortalLayout active="exchange" title="交换任务">
    <section class="rounded-xl border border-wiki-border bg-white p-5">
      <div class="mb-4 flex flex-col justify-between gap-3 sm:flex-row sm:items-center">
        <div>
          <p class="text-xs font-semibold uppercase tracking-wide text-wiki-accent">
            Data Exchange
          </p>
          <h3 class="mt-1 font-heading text-xl font-semibold text-wiki-txt">交换任务</h3>
          <p class="mt-1 text-sm text-wiki-muted">
            批处理、CDC、事件驱动与文件交换任务统一编排监控。
          </p>
        </div>
        <span class="rounded-lg bg-wiki-surface2 px-3 py-1.5 text-xs font-semibold text-wiki-muted">
          今日 724 批次
        </span>
      </div>

      <div class="overflow-x-auto rounded-lg border border-wiki-border">
        <table class="w-full min-w-[760px] text-left text-sm">
          <thead class="bg-wiki-surface2 text-xs uppercase tracking-wide text-wiki-muted">
            <tr>
              <th class="px-4 py-3 font-semibold">任务名称</th>
              <th class="px-4 py-3 font-semibold">类型</th>
              <th class="px-4 py-3 font-semibold">周期</th>
              <th class="px-4 py-3 font-semibold">最近执行</th>
              <th class="px-4 py-3 font-semibold">处理量</th>
              <th class="px-4 py-3 text-right font-semibold">状态</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-wiki-border">
            <tr
              v-for="task in exchangeTasks"
              :key="task.name"
              class="transition-colors hover:bg-wiki-bg"
            >
              <td class="px-4 py-3 font-medium text-wiki-txt">{{ task.name }}</td>
              <td class="px-4 py-3 text-wiki-muted">{{ task.type }}</td>
              <td class="px-4 py-3 text-wiki-muted">{{ task.schedule }}</td>
              <td class="px-4 py-3 text-wiki-muted">{{ task.lastRun }}</td>
              <td class="px-4 py-3 text-wiki-muted">{{ task.volume }}</td>
              <td class="px-4 py-3 text-right font-semibold text-emerald-600">{{ task.status }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <section class="mt-5 grid gap-4 md:grid-cols-3">
      <div
        v-for="metric in exchangeMetrics"
        :key="metric.label"
        class="rounded-xl border border-wiki-border bg-white p-5"
      >
        <Icon :name="metric.icon" size="sm" :class="metric.color" :stroke-width="2" />
        <p class="mt-4 text-sm text-wiki-muted">{{ metric.label }}</p>
        <p class="mt-1 font-heading text-2xl font-semibold text-wiki-txt">{{ metric.value }}</p>
        <p class="mt-1 text-xs text-wiki-muted">{{ metric.detail }}</p>
      </div>
    </section>
  </GuestPortalLayout>
</template>

<script setup lang="ts">
import GuestPortalLayout from './GuestPortalLayout.vue'
import Icon from '@/components/icons/Icon.vue'

const exchangeTasks = [
  { name: '客户画像实时同步', type: 'CDC', schedule: '实时', lastRun: '2 分钟前', volume: '184K 行', status: '正常' },
  { name: '交易明细准实时汇聚', type: '流批一体', schedule: '5 分钟', lastRun: '1 分钟前', volume: '426K 行', status: '正常' },
  { name: '库存快照入仓', type: '批处理', schedule: '15 分钟', lastRun: '8 分钟前', volume: '76K 行', status: '正常' },
  { name: '经营指标日结', type: '调度任务', schedule: '每日', lastRun: '06:10', volume: '864 张表', status: '正常' }
]

const exchangeMetrics = [
  { label: '实时通道', value: '36', detail: 'CDC 运行中', icon: 'download' as const, color: 'text-sky-500' },
  { label: '调度成功率', value: '99.8%', detail: '近 24 小时', icon: 'checkCircle' as const, color: 'text-emerald-500' },
  { label: '平均同步延迟', value: '2.4s', detail: '核心实时链路', icon: 'clock' as const, color: 'text-violet-500' }
]
</script>
