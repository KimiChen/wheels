<template>
  <GuestPortalLayout active="overview" title="数据中台">
    <section>
      <div class="mb-4 flex flex-col justify-between gap-3 sm:flex-row sm:items-end">
        <div>
          <p class="text-xs font-semibold uppercase tracking-wide text-wiki-accent">
            Platform Overview
          </p>
          <h3 class="mt-1 font-heading text-xl font-semibold text-wiki-txt">总览</h3>
          <p class="mt-1 text-sm text-wiki-muted">
            汇聚业务系统数据，统一沉淀资产、服务、规则与运行状态。
          </p>
        </div>
        <RouterLink
          :to="isAuthenticated ? dashboardPath : '/login'"
          class="inline-flex items-center justify-center gap-2 rounded-xl border border-wiki-border bg-white px-4 py-2.5 text-sm font-semibold text-wiki-txt transition-colors hover:bg-wiki-surface2"
        >
          <Icon :name="isAuthenticated ? 'grid' : 'login'" size="sm" :stroke-width="2" />
          <span>{{ isAuthenticated ? '进入工作台' : '申请接入' }}</span>
        </RouterLink>
      </div>

      <div class="grid gap-4 md:grid-cols-3">
        <div
          v-for="card in overviewCards"
          :key="card.label"
          class="rounded-xl border border-wiki-border bg-white p-5 transition-all hover:-translate-y-0.5 hover:shadow-[0_4px_16px_rgba(0,0,0,0.06)]"
        >
          <div
            class="mb-4 flex h-10 w-10 items-center justify-center rounded-lg"
            :class="card.iconClass"
          >
            <Icon :name="card.icon" size="sm" :stroke-width="2" />
          </div>
          <p class="text-sm font-medium text-wiki-muted">{{ card.label }}</p>
          <p class="mt-1 font-heading text-2xl font-semibold text-wiki-txt">{{ card.value }}</p>
          <p class="mt-1 text-xs text-wiki-muted">{{ card.detail }}</p>
        </div>
      </div>
    </section>

    <section class="mt-5 grid gap-5 lg:grid-cols-[minmax(0,1.4fr)_minmax(320px,0.8fr)]">
      <div class="rounded-xl border border-wiki-border bg-white p-5">
        <div class="mb-5 flex flex-col justify-between gap-4 sm:flex-row sm:items-center">
          <div>
            <h3 class="font-heading text-base font-semibold text-wiki-txt">
              数据资产概览（近 30 天）
            </h3>
            <p class="mt-1 text-xs text-wiki-muted">今日同步 24 批次 · 异常 0 项</p>
          </div>
          <RouterLink
            to="/login"
            class="inline-flex items-center justify-center gap-2 rounded-xl bg-gradient-to-r from-indigo-500 to-violet-600 px-4 py-2.5 text-sm font-semibold text-white shadow-md shadow-indigo-200 transition-all hover:from-indigo-600 hover:to-violet-700"
          >
            <Icon name="login" size="sm" :stroke-width="2" />
            <span>申请接入</span>
          </RouterLink>
        </div>

        <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-5">
          <div
            v-for="metric in usageMetrics"
            :key="metric.label"
            class="rounded-lg border border-slate-100 bg-slate-50/50 p-4"
          >
            <div class="mb-3 flex items-center gap-2">
              <Icon :name="metric.icon" size="xs" :class="metric.color" :stroke-width="2" />
              <span class="text-xs font-medium text-wiki-muted">{{ metric.label }}</span>
            </div>
            <p class="font-heading text-xl font-semibold text-wiki-txt">{{ metric.value }}</p>
            <p class="mt-1 text-[11px] text-wiki-muted">{{ metric.detail }}</p>
          </div>
        </div>

        <div class="mt-4 grid gap-3 lg:grid-cols-[minmax(0,1fr)_240px]">
          <div class="rounded-lg border border-slate-100 bg-white p-4">
            <div class="mb-3 flex items-center justify-between gap-3">
              <p class="text-sm font-semibold text-wiki-txt">主题域资产分布</p>
              <span class="text-xs text-wiki-muted">实时更新</span>
            </div>
            <div class="space-y-3">
              <div v-for="item in domainAssets" :key="item.label">
                <div class="mb-1 flex items-center justify-between text-xs">
                  <span class="font-medium text-slate-600">{{ item.label }}</span>
                  <span class="text-wiki-muted">{{ item.value }}</span>
                </div>
                <div class="h-2 overflow-hidden rounded-full bg-slate-100">
                  <div class="h-full rounded-full" :class="item.color" :style="{ width: item.width }"></div>
                </div>
              </div>
            </div>
          </div>

          <div class="rounded-lg border border-slate-100 bg-slate-50/60 p-4">
            <p class="text-sm font-semibold text-wiki-txt">本日治理进度</p>
            <div class="mt-3 space-y-3">
              <div v-for="item in qualityChecks" :key="item.label" class="flex items-center justify-between gap-3">
                <div class="min-w-0">
                  <p class="truncate text-xs font-medium text-slate-600">{{ item.label }}</p>
                  <p class="mt-0.5 text-[11px] text-wiki-muted">{{ item.detail }}</p>
                </div>
                <span class="rounded-full bg-white px-2 py-1 text-xs font-semibold text-emerald-600 ring-1 ring-slate-100">
                  {{ item.value }}
                </span>
              </div>
            </div>
          </div>
        </div>
      </div>

      <div
        class="rounded-2xl border border-indigo-200/80 bg-gradient-to-r from-indigo-500 to-violet-600 p-5 text-white shadow-lg shadow-indigo-500/15"
      >
        <div class="mb-5 flex items-center justify-between gap-4">
          <div>
            <h3 class="font-heading text-base font-semibold">平台运行态</h3>
            <p class="mt-1 text-xs text-white/65">实时概览</p>
          </div>
          <div class="rounded-full bg-white/15 p-2">
            <Icon name="chart" size="sm" :stroke-width="2" />
          </div>
        </div>

        <div class="grid gap-3">
          <div
            v-for="metric in platformMetrics"
            :key="metric.label"
            class="rounded-xl bg-white/10 p-4 ring-1 ring-white/10"
          >
            <div class="flex items-center justify-between gap-3">
              <span class="text-sm text-white/75">{{ metric.label }}</span>
              <Icon :name="metric.icon" size="xs" class="text-white/70" :stroke-width="2" />
            </div>
            <p class="mt-2 font-heading text-2xl font-semibold">{{ metric.value }}</p>
            <p class="mt-1 text-xs text-white/60">{{ metric.detail }}</p>
          </div>
        </div>
      </div>
    </section>
  </GuestPortalLayout>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import GuestPortalLayout from './GuestPortalLayout.vue'
import Icon from '@/components/icons/Icon.vue'

const isAuthenticated = computed(() => hasGuestAuthSession())
const dashboardPath = computed(() => `/${'dashboard'}`)

const overviewCards = [
  {
    label: '已登记数据资产',
    value: '1,248 项',
    detail: '覆盖 18 个业务域',
    icon: 'database' as const,
    iconClass: 'bg-indigo-50 text-wiki-accent'
  },
  {
    label: '数据服务',
    value: '86 个服务',
    detail: '统一目录与权限',
    icon: 'cube' as const,
    iconClass: 'bg-violet-50 text-wiki-accent2'
  },
  {
    label: '质量规则',
    value: '312 条在线',
    detail: '自动稽核与追溯',
    icon: 'shield' as const,
    iconClass: 'bg-purple-50 text-purple-500'
  }
]

const usageMetrics = [
  { label: '交换批次', value: '724', detail: '今日已完成', icon: 'sync' as const, color: 'text-indigo-500' },
  { label: '实时通道', value: '36', detail: 'CDC 运行中', icon: 'download' as const, color: 'text-sky-500' },
  { label: '服务调用', value: '18.6K', detail: '业务系统访问', icon: 'upload' as const, color: 'text-emerald-500' },
  { label: '质量稽核', value: '99.6%', detail: '规则通过率', icon: 'database' as const, color: 'text-amber-500' },
  { label: '数据血缘', value: '1,904', detail: '字段级链路', icon: 'calculator' as const, color: 'text-violet-600' }
]

const domainAssets = [
  { label: '客户域', value: '386 项', width: '92%', color: 'bg-indigo-500' },
  { label: '交易域', value: '312 项', width: '78%', color: 'bg-sky-500' },
  { label: '供应链域', value: '214 项', width: '58%', color: 'bg-emerald-500' },
  { label: '经营分析域', value: '176 项', width: '46%', color: 'bg-violet-500' }
]

const qualityChecks = [
  { label: '字段完整性', value: '通过', detail: '128 条规则' },
  { label: '主键唯一性', value: '通过', detail: '64 条规则' },
  { label: '同步时效性', value: '达标', detail: 'SLA 96.8%' },
  { label: '敏感分级', value: '完成', detail: '32 个标签' }
]

const platformMetrics = [
  { label: '数据新鲜度', value: '96.8%', detail: '按 SLA 达标', icon: 'database' as const },
  { label: '平均同步延迟', value: '2.4s', detail: '实时链路', icon: 'clock' as const },
  { label: '可用性', value: '99.95%', detail: '核心服务', icon: 'bolt' as const }
]

function hasGuestAuthSession(): boolean {
  try {
    const token = localStorage.getItem('auth_token')
    const rawUser = localStorage.getItem('auth_user')
    if (!token || !rawUser) return false
    const user = JSON.parse(rawUser)
    return Boolean(user && typeof user === 'object')
  } catch {
    return false
  }
}
</script>
