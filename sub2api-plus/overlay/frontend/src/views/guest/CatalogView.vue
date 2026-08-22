<template>
  <GuestPortalLayout active="catalog" title="数据目录">
    <section class="rounded-xl border border-wiki-border bg-white p-5">
      <div class="mb-4 flex flex-col justify-between gap-3 sm:flex-row sm:items-center">
        <div>
          <p class="text-xs font-semibold uppercase tracking-wide text-wiki-accent">
            Data Catalog
          </p>
          <h3 class="mt-1 font-heading text-xl font-semibold text-wiki-txt">数据目录</h3>
          <p class="mt-1 text-sm text-wiki-muted">
            按主题域登记来源系统、同步策略、数据量级与服务等级，形成统一资产入口。
          </p>
        </div>
        <div class="rounded-lg bg-wiki-surface2 p-1 text-xs font-medium text-wiki-muted">
          <span class="rounded-md bg-white px-3 py-1 text-wiki-txt shadow-sm">24小时</span>
          <span class="px-3 py-1">7天</span>
        </div>
      </div>

      <div class="overflow-x-auto rounded-lg border border-wiki-border">
        <table class="w-full min-w-[760px] text-left text-sm">
          <thead class="bg-wiki-surface2 text-xs uppercase tracking-wide text-wiki-muted">
            <tr>
              <th class="px-4 py-3 font-semibold">主题域</th>
              <th class="px-4 py-3 font-semibold">数据源</th>
              <th class="px-4 py-3 font-semibold">数据量</th>
              <th class="px-4 py-3 font-semibold">同步策略</th>
              <th class="px-4 py-3 font-semibold">最近校验</th>
              <th class="px-4 py-3 text-right font-semibold">服务等级</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-wiki-border">
            <tr
              v-for="row in catalogRows"
              :key="row.domain"
              class="transition-colors hover:bg-wiki-bg"
            >
              <td class="px-4 py-3 font-medium text-wiki-txt">{{ row.domain }}</td>
              <td class="px-4 py-3 text-wiki-muted">{{ row.sources }}</td>
              <td class="px-4 py-3 text-wiki-muted">{{ row.output }}</td>
              <td class="px-4 py-3 text-wiki-muted">{{ row.work }}</td>
              <td class="px-4 py-3 text-wiki-muted">{{ row.latency }}</td>
              <td class="px-4 py-3 text-right font-semibold text-wiki-accent">{{ row.sla }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <section class="mt-5 grid gap-4 md:grid-cols-3">
      <div
        v-for="card in catalogCards"
        :key="card.title"
        class="rounded-xl border border-wiki-border bg-white p-5"
      >
        <div class="mb-4 flex h-10 w-10 items-center justify-center rounded-lg" :class="card.iconClass">
          <Icon :name="card.icon" size="sm" :stroke-width="2" />
        </div>
        <h4 class="font-heading text-base font-semibold text-wiki-txt">{{ card.title }}</h4>
        <p class="mt-2 text-sm text-wiki-muted">{{ card.description }}</p>
      </div>
    </section>
  </GuestPortalLayout>
</template>

<script setup lang="ts">
import GuestPortalLayout from './GuestPortalLayout.vue'
import Icon from '@/components/icons/Icon.vue'

const catalogRows = [
  { domain: '客户主数据', sources: 'CRM / 会员中心', output: '18.2M 行', work: 'CDC 实时', latency: '2 分钟前', sla: '核心' },
  { domain: '交易明细', sources: '订单库 / 支付库', output: '42.6M 行', work: '准实时', latency: '1 分钟前', sla: '核心' },
  { domain: '库存与供应链', sources: 'ERP / WMS', output: '7.4M 行', work: '15 分钟批', latency: '通过', sla: '重要' },
  { domain: '经营指标', sources: '财务 / BI', output: '864 张宽表', work: '小时级', latency: '通过', sla: '重要' },
  { domain: '风控标签', sources: '规则引擎 / 日志', output: '2,318 标签', work: '事件驱动', latency: '通过', sla: '敏感' }
]

const catalogCards = [
  {
    title: '统一资产视图',
    description: '沉淀表、指标、标签、宽表、数据服务等资产，便于跨部门检索与复用。',
    icon: 'database' as const,
    iconClass: 'bg-indigo-50 text-wiki-accent'
  },
  {
    title: '主题域归档',
    description: '按客户、交易、库存、经营等主题域管理负责人、来源系统与使用范围。',
    icon: 'grid' as const,
    iconClass: 'bg-violet-50 text-wiki-accent2'
  },
  {
    title: '资产生命周期',
    description: '覆盖登记、评审、发布、变更、下线流程，保持目录内容可信可追溯。',
    icon: 'sync' as const,
    iconClass: 'bg-emerald-50 text-emerald-600'
  }
]
</script>
