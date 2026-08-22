<template>
  <GuestPortalLayout active="governance" title="数据治理">
    <section class="rounded-xl border border-wiki-border bg-white p-5">
      <div class="mb-4 flex flex-col justify-between gap-3 sm:flex-row sm:items-center">
        <div>
          <p class="text-xs font-semibold uppercase tracking-wide text-wiki-accent">
            Governance
          </p>
          <h3 class="mt-1 font-heading text-xl font-semibold text-wiki-txt">数据治理</h3>
          <p class="mt-1 text-sm text-wiki-muted">
            统一配置质量规则、血缘追踪、数据分级与审批策略，保障数据可信使用。
          </p>
        </div>
        <span class="rounded-lg bg-emerald-50 px-3 py-1.5 text-xs font-semibold text-emerald-700">
          312 条规则在线
        </span>
      </div>

      <div class="grid gap-4 md:grid-cols-3">
        <div
          v-for="item in governanceItems"
          :key="item.title"
          class="rounded-lg border border-slate-100 bg-slate-50/60 p-4"
        >
          <div class="mb-3 flex items-center gap-2">
            <Icon :name="item.icon" size="sm" :class="item.iconClass" :stroke-width="2" />
            <h4 class="text-sm font-semibold text-wiki-txt">{{ item.title }}</h4>
          </div>
          <p class="text-sm text-wiki-muted">{{ item.description }}</p>
          <p class="mt-3 text-xs font-semibold text-wiki-accent">{{ item.status }}</p>
        </div>
      </div>
    </section>

    <section class="mt-5 rounded-xl border border-wiki-border bg-white p-5">
      <h3 class="font-heading text-base font-semibold text-wiki-txt">治理闭环</h3>
      <div class="mt-4 grid gap-3 md:grid-cols-4">
        <div
          v-for="step in governanceSteps"
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

const governanceItems = [
  {
    title: '质量规则',
    description: '对完整性、唯一性、及时性、值域范围进行自动校验，异常结果进入待办闭环。',
    status: '通过率 99.6%',
    icon: 'checkCircle' as const,
    iconClass: 'text-emerald-500'
  },
  {
    title: '血缘追踪',
    description: '沉淀表级、字段级链路关系，支持指标口径回溯、影响分析与变更评审。',
    status: '1,904 条链路',
    icon: 'link' as const,
    iconClass: 'text-indigo-500'
  },
  {
    title: '分级权限',
    description: '按主题域、敏感等级和组织角色配置访问策略，统一审批与审计记录。',
    status: '18 个业务域覆盖',
    icon: 'shield' as const,
    iconClass: 'text-violet-500'
  }
]

const governanceSteps = [
  { index: '01', title: '规则定义', description: '配置质量规则、敏感等级和负责人。' },
  { index: '02', title: '自动稽核', description: '按调度周期执行校验并记录结果。' },
  { index: '03', title: '异常派发', description: '异常进入责任人待办并保留处理轨迹。' },
  { index: '04', title: '复核发布', description: '复核通过后同步目录状态与服务等级。' }
]
</script>
