<template>
  <GuestPortalLayout active="orchestration" title="服务编排">
    <section class="rounded-xl border border-wiki-border bg-white p-5">
      <div class="mb-4 flex flex-col justify-between gap-3 sm:flex-row sm:items-center">
        <div>
          <p class="text-xs font-semibold uppercase tracking-wide text-wiki-accent">
            Service Orchestration
          </p>
          <h3 class="mt-1 font-heading text-xl font-semibold text-wiki-txt">服务编排</h3>
          <p class="mt-1 text-sm text-wiki-muted">
            将数据资产封装为目录服务、指标服务与跨域查询服务。
          </p>
        </div>
        <RouterLink
          to="/login"
          class="inline-flex items-center justify-center gap-2 rounded-xl bg-wiki-accent px-4 py-2.5 text-sm font-semibold text-white transition-all hover:bg-indigo-600"
        >
          <Icon name="login" size="sm" :stroke-width="2" />
          <span>登录查看</span>
        </RouterLink>
      </div>

      <div class="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
        <div
          v-for="service in orchestrationServices"
          :key="service.title"
          class="rounded-lg border border-slate-100 bg-slate-50/60 p-4"
        >
          <div class="mb-3 flex h-9 w-9 items-center justify-center rounded-lg bg-white text-wiki-accent">
            <Icon :name="service.icon" size="sm" :stroke-width="2" />
          </div>
          <h4 class="text-sm font-semibold text-wiki-txt">{{ service.title }}</h4>
          <p class="mt-2 text-sm text-wiki-muted">{{ service.description }}</p>
          <p class="mt-3 text-xs font-semibold text-wiki-accent">{{ service.scope }}</p>
        </div>
      </div>
    </section>

    <section class="mt-5 rounded-xl border border-wiki-border bg-white p-5">
      <h3 class="font-heading text-base font-semibold text-wiki-txt">编排能力</h3>
      <div class="mt-4 grid gap-3 md:grid-cols-3">
        <div
          v-for="ability in abilities"
          :key="ability"
          class="rounded-lg bg-wiki-surface2 px-4 py-3 text-sm text-wiki-muted"
        >
          {{ ability }}
        </div>
      </div>
    </section>
  </GuestPortalLayout>
</template>

<script setup lang="ts">
import { RouterLink } from 'vue-router'
import GuestPortalLayout from './GuestPortalLayout.vue'
import Icon from '@/components/icons/Icon.vue'

const orchestrationServices = [
  {
    title: '统一查询服务',
    description: '对多源异构数据提供统一访问入口，屏蔽底层库表和存储差异。',
    scope: '面向经营分析',
    icon: 'search' as const
  },
  {
    title: '指标口径服务',
    description: '沉淀复用指标定义，统一计算口径、版本和发布流程。',
    scope: '面向管理驾驶舱',
    icon: 'calculator' as const
  },
  {
    title: '主题数据服务',
    description: '围绕客户、交易、库存等主题封装标准数据接口。',
    scope: '面向业务系统',
    icon: 'cube' as const
  },
  {
    title: '权限审计服务',
    description: '记录访问申请、授权、调用和导出行为，满足合规追踪要求。',
    scope: '面向安全合规',
    icon: 'clipboard' as const
  }
]

const abilities = [
  '服务版本管理',
  '访问策略配置',
  '流量与调用审计',
  '跨域查询编排',
  '指标口径复用',
  '数据产品发布'
]
</script>
