<template>
  <div class="min-h-screen bg-wiki-bg font-body text-wiki-txt">
    <div
      v-if="mobileOpen"
      class="fixed inset-0 z-40 bg-black/30 md:hidden"
      @click="mobileOpen = false"
    ></div>

    <div class="flex min-h-screen">
      <aside
        class="fixed inset-y-0 left-0 z-50 flex w-60 flex-col border-r border-wiki-border bg-white transition-transform duration-200 md:sticky md:top-0 md:translate-x-0"
        :class="mobileOpen ? 'translate-x-0' : '-translate-x-full'"
      >
        <div class="border-b border-wiki-border p-5">
          <RouterLink to="/" class="flex items-center gap-3" @click="mobileOpen = false">
            <div
              class="flex h-9 w-9 items-center justify-center overflow-hidden rounded-xl bg-gradient-to-br from-wiki-accent to-wiki-accent2"
            >
              <Icon name="sparkles" size="md" class="text-white" :stroke-width="2" />
            </div>
            <div class="min-w-0">
              <h1 class="truncate font-heading text-base font-bold text-wiki-txt">
                企业数据中台
              </h1>
              <p class="text-[11px] uppercase tracking-wide text-wiki-muted">DATA FABRIC</p>
            </div>
          </RouterLink>
        </div>

        <nav class="flex-1 py-3">
          <RouterLink
            v-for="item in navItems"
            :key="item.key"
            :to="item.to"
            class="guest-nav-item relative flex items-center gap-3 px-5 py-2.5 text-sm"
            @click="mobileOpen = false"
          >
            <Icon :name="item.icon" size="sm" :stroke-width="2" />
            <span>{{ item.label }}</span>
          </RouterLink>
        </nav>

        <div class="border-t border-wiki-border p-4">
          <div class="mb-3 flex items-center gap-2 text-xs text-wiki-muted">
            <Icon name="globe" size="xs" :stroke-width="2" />
            <span>简体中文</span>
          </div>
          <RouterLink
            :to="isAuthenticated ? dashboardPath : '/login'"
            class="flex items-center gap-2 text-xs text-wiki-muted transition-colors hover:text-wiki-txt"
            @click="mobileOpen = false"
          >
            <Icon name="arrowLeft" size="xs" :stroke-width="2" />
            <span>{{ isAuthenticated ? '进入控制台' : '登录工作台' }}</span>
          </RouterLink>
        </div>
      </aside>

      <main class="flex min-h-screen min-w-0 flex-1 flex-col">
        <header
          class="sticky top-0 z-30 flex items-center justify-between border-b border-wiki-border bg-white/80 px-4 py-3 backdrop-blur-lg md:px-6"
        >
          <div class="flex min-w-0 items-center gap-3">
            <button
              type="button"
              class="rounded-lg p-1.5 text-wiki-muted transition-colors hover:bg-wiki-surface2 md:hidden"
              @click="mobileOpen = true"
              aria-label="Open menu"
            >
              <Icon name="menu" size="md" :stroke-width="2" />
            </button>
            <h2 class="truncate font-heading text-lg font-semibold text-wiki-txt">数据中台</h2>
          </div>

          <div class="hidden items-center gap-2 rounded-lg bg-wiki-surface2 px-3 py-1.5 text-sm sm:flex">
            <Icon name="shield" size="sm" class="text-wiki-accent" />
            <span class="font-semibold">统一认证</span>
            <span class="text-wiki-muted">已启用</span>
          </div>
        </header>

        <section class="flex flex-1 items-center justify-center px-4 py-8 sm:px-6 lg:px-10">
          <div class="w-full max-w-[430px] md:-translate-y-[28px] lg:-translate-x-[120px]">
            <div class="rounded-2xl border border-wiki-border bg-white p-6 shadow-[0_8px_24px_rgba(15,23,42,0.05)] sm:p-8">
              <slot />
            </div>

            <div class="mt-5 text-center text-sm">
              <slot name="footer" />
            </div>

            <p class="mt-6 text-center text-xs text-slate-400">
              &copy; {{ currentYear }} 企业数据中台. All rights reserved.
            </p>
          </div>
        </section>
      </main>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import Icon from '@/components/icons/Icon.vue'

const mobileOpen = ref(false)

const currentYear = computed(() => new Date().getFullYear())
const isAuthenticated = computed(() => hasGuestAuthSession())
const dashboardPath = computed(() => `/${'dashboard'}`)

const navItems = [
  { label: '总览', key: 'overview' as const, to: '/', icon: 'grid' as const },
  { label: '数据目录', key: 'catalog' as const, to: '/catalog', icon: 'database' as const },
  { label: '数据治理', key: 'governance' as const, to: '/governance', icon: 'shield' as const },
  { label: '交换任务', key: 'exchange' as const, to: '/exchange', icon: 'sync' as const },
  { label: '服务编排', key: 'orchestration' as const, to: '/orchestration', icon: 'cube' as const },
  { label: '接入规范', key: 'docs' as const, to: '/docs', icon: 'book' as const }
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

<style scoped>
.guest-nav-item {
  color: #64748b;
  transition: all 0.2s ease;
}

.guest-nav-item:hover {
  background: #f1f5f9;
  color: #0f172a;
}

.guest-nav-item.router-link-active {
  background: rgba(99, 102, 241, 0.08);
  color: #6366f1;
  font-weight: 600;
}

.guest-nav-item.router-link-active::before {
  content: '';
  position: absolute;
  left: 0;
  top: 8px;
  bottom: 8px;
  width: 3px;
  border-radius: 0 3px 3px 0;
  background: #6366f1;
}
</style>
