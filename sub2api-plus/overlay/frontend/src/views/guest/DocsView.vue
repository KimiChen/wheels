<template>
  <GuestPortalLayout active="docs" title="接入规范">
    <section class="grid gap-5 lg:grid-cols-[minmax(0,1fr)_360px]">
      <div class="rounded-xl border border-wiki-border bg-white p-5">
        <div class="mb-4 flex items-center gap-2">
          <Icon name="book" size="sm" class="text-wiki-accent" :stroke-width="2" />
          <h3 class="font-heading text-base font-semibold text-wiki-txt">接入规范</h3>
        </div>
        <p class="text-sm text-wiki-muted">
          面向业务系统提供统一数据模型、逻辑数据仓库与实时 CDC 管道，降低跨库查询与报表整合成本。
        </p>
        <div class="mt-4 rounded-xl bg-slate-900 p-4 font-mono text-xs text-slate-100">
          <p class="text-emerald-300">SELECT customer_id, risk_level, updated_at</p>
          <p class="pl-3 text-slate-300">FROM fabric.customer_profile</p>
          <p class="pl-3 text-slate-300">WHERE biz_date = CURRENT_DATE</p>
          <p class="pl-3 text-amber-200">AND data_quality_status = 'PASS';</p>
        </div>
      </div>

      <div class="rounded-xl border border-wiki-border bg-white p-5">
        <div class="mb-4 flex items-center justify-between gap-3">
          <div>
            <h3 class="font-heading text-base font-semibold text-wiki-txt">最近任务</h3>
            <p class="text-xs text-wiki-muted">接入流程状态</p>
          </div>
          <Icon name="clock" size="sm" class="text-slate-300" :stroke-width="1.5" />
        </div>
        <div class="flex min-h-[130px] flex-col items-center justify-center text-center">
          <Icon name="clock" size="xl" class="mb-3 text-slate-200" :stroke-width="1.5" />
          <p class="text-sm text-wiki-muted">暂无异常任务</p>
        </div>
      </div>
    </section>

    <section class="mt-5 rounded-xl border border-wiki-border bg-white p-5">
      <h3 class="font-heading text-base font-semibold text-wiki-txt">接入步骤</h3>
      <div class="mt-4 grid gap-3 md:grid-cols-4">
        <div
          v-for="step in docSteps"
          :key="step.title"
          class="rounded-lg bg-wiki-surface2 p-4"
        >
          <p class="text-xs font-semibold text-wiki-accent">{{ step.index }}</p>
          <h4 class="mt-2 text-sm font-semibold text-wiki-txt">{{ step.title }}</h4>
          <p class="mt-1 text-xs text-wiki-muted">{{ step.description }}</p>
        </div>
      </div>
    </section>
  </GuestPortalLayout>
</template>

<script setup lang="ts">
import GuestPortalLayout from './GuestPortalLayout.vue'
import Icon from '@/components/icons/Icon.vue'

const docSteps = [
  { index: '01', title: '提交申请', description: '说明业务场景、数据范围和使用周期。' },
  { index: '02', title: '权限评审', description: '按主题域负责人和数据等级完成审批。' },
  { index: '03', title: '联调验证', description: '完成字段映射、质量规则和同步策略验证。' },
  { index: '04', title: '上线运行', description: '接入监控、审计和变更管理流程。' }
]
</script>
