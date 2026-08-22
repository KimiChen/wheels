<template>
  <Teleport to="body">
    <div class="pointer-events-none fixed right-4 top-4 z-[9999] space-y-3" aria-live="polite">
      <TransitionGroup
        enter-active-class="transition ease-out duration-300"
        enter-from-class="translate-x-full opacity-0"
        enter-to-class="translate-x-0 opacity-100"
        leave-active-class="transition ease-in duration-200"
        leave-from-class="translate-x-0 opacity-100"
        leave-to-class="translate-x-full opacity-0"
      >
        <div
          v-for="toast in toasts"
          :key="toast.id"
          :class="[
            'pointer-events-auto min-w-[300px] max-w-md overflow-hidden rounded-lg border-l-4 bg-white shadow-lg',
            colorClass(toast.type).border,
          ]"
        >
          <div class="flex items-start gap-3 p-4">
            <Icon :name="iconName(toast.type)" size="md" :class="colorClass(toast.type).icon" />
            <p class="min-w-0 flex-1 text-sm leading-relaxed text-slate-900">{{ toast.message }}</p>
            <button
              type="button"
              class="-m-1 rounded p-1 text-slate-400 transition-colors hover:bg-slate-100 hover:text-slate-600"
              aria-label="关闭通知"
              @click="hideToast(toast.id)"
            >
              <Icon name="x" size="sm" />
            </button>
          </div>
        </div>
      </TransitionGroup>
    </div>
  </Teleport>
</template>

<script setup lang="ts">
import Icon from '@/components/icons/Icon.vue'
import { useGuestToast, type GuestToastType } from '@/composables/useGuestToast'

const { toasts, hideToast } = useGuestToast()

function iconName(type: GuestToastType): 'checkCircle' | 'xCircle' | 'exclamationTriangle' | 'infoCircle' {
  if (type === 'success') return 'checkCircle'
  if (type === 'error') return 'xCircle'
  if (type === 'warning') return 'exclamationTriangle'
  return 'infoCircle'
}

function colorClass(type: GuestToastType): { border: string; icon: string } {
  if (type === 'success') return { border: 'border-emerald-500', icon: 'text-emerald-500' }
  if (type === 'error') return { border: 'border-red-500', icon: 'text-red-500' }
  if (type === 'warning') return { border: 'border-amber-500', icon: 'text-amber-500' }
  return { border: 'border-sky-500', icon: 'text-sky-500' }
}
</script>
