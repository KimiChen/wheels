import { ref } from 'vue'

export type GuestToastType = 'success' | 'error' | 'warning' | 'info'

export interface GuestToast {
  id: string
  type: GuestToastType
  message: string
  duration: number
}

const toasts = ref<GuestToast[]>([])
let toastCounter = 0

function showToast(type: GuestToastType, message: string, duration = 4000): string {
  const id = `guest-toast-${++toastCounter}`
  toasts.value.push({ id, type, message, duration })
  window.setTimeout(() => hideToast(id), duration)
  return id
}

function hideToast(id: string): void {
  const index = toasts.value.findIndex((toast) => toast.id === id)
  if (index !== -1) {
    toasts.value.splice(index, 1)
  }
}

export function useGuestToast() {
  return {
    toasts,
    hideToast,
    showSuccess: (message: string, duration?: number) => showToast('success', message, duration),
    showError: (message: string, duration?: number) => showToast('error', message, duration ?? 5000),
    showWarning: (message: string, duration?: number) => showToast('warning', message, duration),
    showInfo: (message: string, duration?: number) => showToast('info', message, duration),
  }
}
